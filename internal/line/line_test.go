package line

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"events":[]}`)
	good := sign("s3cret", body)
	if !VerifySignature("s3cret", body, good) {
		t.Fatal("valid signature rejected")
	}
	cases := map[string]bool{
		"wrong secret":  VerifySignature("other", body, good),
		"tampered body": VerifySignature("s3cret", []byte(`{"events":[1]}`), good),
		"empty sig":     VerifySignature("s3cret", body, ""),
		"garbage sig":   VerifySignature("s3cret", body, "not-base64!!"),
		"empty secret":  VerifySignature("", body, sign("", body)),
	}
	for name, ok := range cases {
		if ok {
			t.Errorf("%s must fail", name)
		}
	}
}

func TestBindingCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, hash, err := NewBindingCode()
		if err != nil || len(code) != CodeLength || len(hash) != 32 {
			t.Fatalf("%q %v", code, err)
		}
		if strings.ContainsAny(code, "01OIL") {
			t.Fatalf("look-alike character in %q", code)
		}
		if n, ok := NormalizeCode(code); !ok || n != code {
			t.Fatalf("code does not round-trip: %q", code)
		}
		seen[code] = true
	}
	if len(seen) < 199 {
		t.Fatal("codes are not random enough")
	}
}

func TestNormalizeCode(t *testing.T) {
	if c, ok := NormalizeCode(" abcd-efgh "); !ok || c != "ABCDEFGH" {
		t.Fatalf("%q %v", c, ok)
	}
	for _, bad := range []string{"", "hello", "ABCDEFG", "ABCDEFGHJ", "ABCD0EFG", "你好嗎你好嗎你好"} {
		if _, ok := NormalizeCode(bad); ok {
			t.Errorf("%q should not be a code", bad)
		}
	}
}

func TestPushSendsRetryKeyAndTreats409AsSuccess(t *testing.T) {
	var gotKey, gotAuth string
	var body map[string]any
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotAuth = r.Header.Get("X-Line-Retry-Key"), r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := NewClient("TOKEN")
	c.BaseURL = srv.URL
	if err := c.Push(context.Background(), "U123", "hi", "key-1"); err != nil {
		t.Fatal(err)
	}
	if gotKey != "key-1" || gotAuth != "Bearer TOKEN" || body["to"] != "U123" {
		t.Fatalf("%q %q %v", gotKey, gotAuth, body)
	}
	status = 409
	if err := c.Push(context.Background(), "U123", "hi", "key-1"); err != nil {
		t.Fatalf("409 (retry key already accepted) must count as delivered: %v", err)
	}
	for code, perm := range map[int]bool{400: true, 403: true, 429: false, 500: false, 503: false} {
		status = code
		err := c.Push(context.Background(), "U", "x", "k")
		var se *StatusError
		if !errors.As(err, &se) || se.Permanent() != perm {
			t.Errorf("status %d: err=%v want permanent=%v", code, err, perm)
		}
	}
}

// ---- webhook ---------------------------------------------------------------

type fakeBinder struct {
	validHash []byte
	bound     map[string]string
	blocked   map[string]bool
	err       error
}

func (f *fakeBinder) Bind(_ context.Context, h []byte, uid string, _ time.Time) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if !bytes.Equal(h, f.validHash) {
		return "", ErrCodeInvalid
	}
	f.bound[uid] = "我自己"
	return "我自己", nil
}
func (f *fakeBinder) SetBlocked(_ context.Context, uid string, b bool) error {
	f.blocked[uid] = b
	return nil
}

type fakeReplier struct{ msgs []string }

func (f *fakeReplier) Reply(_ context.Context, _, m string) error {
	f.msgs = append(f.msgs, m)
	return nil
}

func post(h http.Handler, secret string, payload string, override string) *httptest.ResponseRecorder {
	body := []byte(payload)
	req := httptest.NewRequest("POST", "/line/webhook", bytes.NewReader(body))
	sig := sign(secret, body)
	if override != "" {
		sig = override
	}
	req.Header.Set("X-Line-Signature", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func msgEvent(uid, text string) string {
	b, _ := json.Marshal(map[string]any{"events": []any{map[string]any{
		"type": "message", "replyToken": "rt", "source": map[string]any{"type": "user", "userId": uid},
		"message": map[string]any{"type": "text", "text": text}}}})
	return string(b)
}

func newHook(t *testing.T) (*Webhook, *fakeBinder, *fakeReplier, string) {
	code, hash, _ := NewBindingCode()
	b := &fakeBinder{validHash: hash, bound: map[string]string{}, blocked: map[string]bool{}}
	r := &fakeReplier{}
	return NewWebhook("sec", b, r), b, r, code
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	w, b, _, code := newHook(t)
	payload := msgEvent("U1", code)
	if rec := post(w, "sec", payload, "AAAA"); rec.Code != 401 {
		t.Fatalf("wrong signature: %d", rec.Code)
	}
	if rec := post(w, "wrong-secret", payload, ""); rec.Code != 401 {
		t.Fatalf("signed with wrong secret: %d", rec.Code)
	}
	req := httptest.NewRequest("POST", "/line/webhook", strings.NewReader(payload)) // no header at all
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 401 || len(b.bound) != 0 {
		t.Fatalf("missing signature must be rejected and change nothing: %d %v", rec.Code, b.bound)
	}
}

func TestWebhookVerifyPingAndBadJSON(t *testing.T) {
	w, _, _, _ := newHook(t)
	if rec := post(w, "sec", `{"destination":"U","events":[]}`, ""); rec.Code != 200 {
		t.Fatalf("LINE's Verify button sends empty events: %d", rec.Code)
	}
	if rec := post(w, "sec", `not json`, ""); rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestWebhookBindsWithValidCode(t *testing.T) {
	w, b, r, code := newHook(t)
	if rec := post(w, "sec", msgEvent("U1", strings.ToLower(code[:4])+"-"+code[4:]), ""); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	if b.bound["U1"] == "" || len(r.msgs) != 1 || !strings.Contains(r.msgs[0], "綁定成功") {
		t.Fatalf("%v %v", b.bound, r.msgs)
	}
}

func TestWebhookWrongCodesAreRateLimited(t *testing.T) {
	w, b, r, code := newHook(t)
	for i := 0; i < 5; i++ {
		post(w, "sec", msgEvent("U9", "ZZZZZZZZ"), "")
	}
	// Even the right code is refused now: the attacker cannot keep guessing.
	post(w, "sec", msgEvent("U9", code), "")
	if len(b.bound) != 0 || !strings.Contains(r.msgs[len(r.msgs)-1], "次數過多") {
		t.Fatalf("%v %v", b.bound, r.msgs)
	}
	// Other people are unaffected.
	post(w, "sec", msgEvent("U2", code), "")
	if b.bound["U2"] == "" {
		t.Fatal("limit must be per LINE user")
	}
}

func TestWebhookOtherTextAndAlreadyBound(t *testing.T) {
	w, b, r, code := newHook(t)
	post(w, "sec", msgEvent("U1", "你好"), "")
	if len(b.bound) != 0 || !strings.Contains(r.msgs[0], "綁定碼") {
		t.Fatalf("%v", r.msgs)
	}
	b.err = ErrAlreadyBound
	post(w, "sec", msgEvent("U1", code), "")
	if !strings.Contains(r.msgs[1], "已經綁定") {
		t.Fatalf("%v", r.msgs)
	}
}

func TestWebhookFollowUnfollowAndIgnoresGroups(t *testing.T) {
	w, b, _, _ := newHook(t)
	unf, _ := json.Marshal(map[string]any{"events": []any{map[string]any{"type": "unfollow", "source": map[string]any{"type": "user", "userId": "U5"}}}})
	post(w, "sec", string(unf), "")
	if !b.blocked["U5"] {
		t.Fatal("unfollow must mark the contact blocked")
	}
	fol, _ := json.Marshal(map[string]any{"events": []any{map[string]any{"type": "follow", "replyToken": "r", "source": map[string]any{"type": "user", "userId": "U5"}}}})
	post(w, "sec", string(fol), "")
	if b.blocked["U5"] {
		t.Fatal("follow must reactivate")
	}
	grp, _ := json.Marshal(map[string]any{"events": []any{map[string]any{"type": "message", "replyToken": "r",
		"source":  map[string]any{"type": "group", "groupId": "G1", "userId": "U7"},
		"message": map[string]any{"type": "text", "text": "ABCDEFGH"}}}})
	before := len(b.bound)
	post(w, "sec", string(grp), "")
	if len(b.bound) != before {
		t.Fatal("group messages must be ignored")
	}
}

func TestMulticast(t *testing.T) {
	var path, key string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("X-Line-Retry-Key")
		_ = json.NewDecoder(r.Body).Decode(&body)
	}))
	defer srv.Close()
	c := NewClient("T")
	c.BaseURL = srv.URL
	if err := c.Multicast(context.Background(), []string{"U1", "U2"}, "hi", "k"); err != nil {
		t.Fatal(err)
	}
	if path != "/v2/bot/message/multicast" || key != "k" || len(body["to"].([]any)) != 2 {
		t.Fatalf("%s %s %v", path, key, body)
	}
	if err := c.Multicast(context.Background(), nil, "hi", "k"); err == nil {
		t.Fatal("empty recipient list must be refused before any request")
	}
	if err := c.Multicast(context.Background(), make([]string, MaxMulticast+1), "hi", "k"); err == nil {
		t.Fatal("over LINE's limit must be refused")
	}
}

func TestRecipientGoneOnlyForRecipientProblems(t *testing.T) {
	for code, want := range map[int]bool{400: true, 403: true, 404: true, 401: false, 429: false, 500: false} {
		if got := (&StatusError{Code: code}).RecipientGone(); got != want {
			t.Errorf("%d: got %v want %v", code, got, want)
		}
	}
}

var _ = io.Discard
