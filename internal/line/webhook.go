package line

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

var (
	ErrCodeInvalid  = errors.New("binding code invalid, used or expired")
	ErrAlreadyBound = errors.New("this LINE account is already bound to another contact")
)

// Binder is implemented by the data layer.
type Binder interface {
	// Bind consumes a valid one-time code and links lineUserID to its contact.
	Bind(ctx context.Context, codeHash []byte, lineUserID string, now time.Time) (contactName string, err error)
	// SetBlocked flags contacts of this LINE user as blocked (unfollow) or active (follow).
	SetBlocked(ctx context.Context, lineUserID string, blocked bool) error
}

type Replier interface {
	Reply(ctx context.Context, replyToken, msg string) error
}

type Webhook struct {
	Secret  string
	Binder  Binder
	Replier Replier
	Now     func() time.Time
	lim     *limiter
}

func NewWebhook(secret string, b Binder, r Replier) *Webhook {
	return &Webhook{Secret: secret, Binder: b, Replier: r, Now: time.Now, lim: newLimiter(5, 10*time.Minute)}
}

type event struct {
	Type       string `json:"type"`
	ReplyToken string `json:"replyToken"`
	Source     struct {
		Type   string `json:"type"`
		UserID string `json:"userId"`
	} `json:"source"`
	Message struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"message"`
}

// ServeHTTP verifies the signature first; nothing in the body is trusted before that.
func (w *Webhook) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, 1<<20))
	if err != nil {
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}
	if !VerifySignature(w.Secret, body, r.Header.Get("X-Line-Signature")) {
		http.Error(rw, "invalid signature", http.StatusUnauthorized)
		return
	}
	var payload struct {
		Events []event `json:"events"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	for _, ev := range payload.Events { // an empty list is LINE's "Verify" ping
		w.handle(ctx, ev)
	}
	rw.WriteHeader(http.StatusOK)
}

func (w *Webhook) reply(ctx context.Context, ev event, msg string) {
	if ev.ReplyToken == "" {
		return
	}
	if err := w.Replier.Reply(ctx, ev.ReplyToken, msg); err != nil {
		log.Printf("line reply: %v", err)
	}
}

func (w *Webhook) handle(ctx context.Context, ev event) {
	if ev.Source.Type != "user" || ev.Source.UserID == "" {
		return // groups/rooms are out of scope for v1
	}
	uid := ev.Source.UserID
	switch ev.Type {
	case "unfollow":
		if err := w.Binder.SetBlocked(ctx, uid, true); err != nil {
			log.Printf("line unfollow: %v", err)
		}
	case "follow":
		if err := w.Binder.SetBlocked(ctx, uid, false); err != nil {
			log.Printf("line follow: %v", err)
		}
		w.reply(ctx, ev, "歡迎!請輸入 App 上產生的 8 碼綁定碼,完成後就能收到提醒。")
	case "message":
		if ev.Message.Type != "text" {
			return
		}
		w.handleText(ctx, ev, uid)
	}
}

func (w *Webhook) handleText(ctx context.Context, ev event, uid string) {
	code, ok := NormalizeCode(ev.Message.Text)
	if !ok {
		w.reply(ctx, ev, "請輸入 App 上產生的 8 碼綁定碼。")
		return
	}
	now := w.Now()
	if w.lim.blocked(uid, now) {
		w.reply(ctx, ev, "嘗試次數過多,請 10 分鐘後再試。")
		return
	}
	name, err := w.Binder.Bind(ctx, HashCode(code), uid, now)
	switch {
	case err == nil:
		w.lim.reset(uid)
		w.reply(ctx, ev, "綁定成功!之後「"+name+"」的提醒會傳到這裡。")
	case errors.Is(err, ErrCodeInvalid):
		w.lim.fail(uid, now)
		w.reply(ctx, ev, "綁定碼無效或已過期,請回 App 重新產生。")
	case errors.Is(err, ErrAlreadyBound):
		w.reply(ctx, ev, "這個 LINE 帳號已經綁定給其他聯絡人了。")
	default:
		log.Printf("line bind: %v", err)
		w.reply(ctx, ev, "綁定時發生錯誤,請稍後再試。")
	}
}

// limiter allows a few wrong codes per LINE user per window (brute-force guard).
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	m      map[string]*entry
}
type entry struct {
	fails int
	first time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, m: map[string]*entry{}}
}

func (l *limiter) blocked(k string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[k]
	if e == nil {
		return false
	}
	if now.Sub(e.first) > l.window {
		delete(l.m, k)
		return false
	}
	return e.fails >= l.max
}

func (l *limiter) fail(k string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[k]
	if e == nil || now.Sub(e.first) > l.window {
		e = &entry{first: now}
		l.m[k] = e
	}
	e.fails++
}

func (l *limiter) reset(k string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, k)
}
