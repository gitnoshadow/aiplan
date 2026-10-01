// Package scheduler sends due reminders. It runs inside the single app
// instance, scans every minute and is safe to restart: anything due but not
// yet sent is picked up on the next scan.
package scheduler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"voiceplan/internal/line"
	"voiceplan/internal/plan"
	"voiceplan/internal/remind"
)

// Due is one delivery that is ready to send.
type Due struct {
	ID            int64
	ContactID     int64
	Attempts      int // attempts already made
	Late          bool
	EventStart    time.Time
	Title         string
	Location      string
	Notes         string
	Tips          string
	LineUserID    string
	ContactStatus string // pending | active | blocked
	Enabled       bool   // false = the owner paused this contact
	IsSelf        bool   // the owner gets the full version; everyone else the short one
}

type Store interface {
	// Claim atomically moves up to limit due deliveries to "sending" and returns them.
	Claim(ctx context.Context, now time.Time, limit int) ([]Due, error)
	MarkSent(ctx context.Context, id int64, now time.Time) error
	MarkSkipped(ctx context.Context, id int64, reason string) error
	MarkFailed(ctx context.Context, id int64, attempts int, reason string) error
	Retry(ctx context.Context, id int64, attempts int, next time.Time, reason string) error
	// RecoverStuck returns deliveries stuck in "sending" (crash mid-send) to pending.
	RecoverStuck(ctx context.Context, olderThan time.Time) (int64, error)
	// MarkContactInvalid flags a contact whose LINE account can no longer receive messages.
	MarkContactInvalid(ctx context.Context, contactID int64) error
	// MonthUsage returns messages pushed so far in a Taipei month ("2026-10").
	MonthUsage(ctx context.Context, month string) (int, error)
}

type Sender interface {
	Push(ctx context.Context, to, msg, retryKey string) error
	Multicast(ctx context.Context, to []string, msg, retryKey string) error
}

type permanent interface{ Permanent() bool }
type recipientGone interface{ RecipientGone() bool }

type Scheduler struct {
	Store        Store
	Sender       Sender
	Interval     time.Duration
	Now          func() time.Time
	MonthlyLimit int // 0 = do not enforce
}

func New(store Store, sender Sender) *Scheduler {
	return &Scheduler{Store: store, Sender: sender, Interval: time.Minute, Now: time.Now}
}

// Run scans immediately and then every Interval until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	s.Tick(ctx)
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick processes everything due at this moment. Exposed for tests.
func (s *Scheduler) Tick(ctx context.Context) {
	now := s.Now()
	if n, err := s.Store.RecoverStuck(ctx, now.Add(-10*time.Minute)); err != nil {
		log.Printf("scheduler: recover stuck: %v", err)
	} else if n > 0 {
		log.Printf("scheduler: recovered %d stuck deliveries", n)
	}
	for {
		batch, err := s.Store.Claim(ctx, now, 50)
		if err != nil {
			log.Printf("scheduler: claim: %v", err)
			return
		}
		if len(batch) == 0 {
			return
		}
		s.processBatch(ctx, batch, now)
	}
}

type group struct {
	text string
	ds   []Due
}

func (s *Scheduler) processBatch(ctx context.Context, batch []Due, now time.Time) {
	var groups []*group
	byText := map[string]*group{}

	for _, d := range batch {
		switch {
		case !now.Before(d.EventStart): // already started: a reminder now is just noise
			s.log("skip", s.Store.MarkSkipped(ctx, d.ID, "event_started"))
			continue
		case !d.Enabled:
			s.log("skip", s.Store.MarkSkipped(ctx, d.ID, "contact_disabled"))
			continue
		case d.ContactStatus != "active" || d.LineUserID == "":
			s.log("fail", s.Store.MarkFailed(ctx, d.ID, d.Attempts, "contact_inactive"))
			continue
		}
		text := Render(d, now)
		g := byText[text]
		if g == nil {
			g = &group{text: text}
			byText[text] = g
			groups = append(groups, g)
		}
		g.ds = append(g.ds, d)
	}
	for _, g := range groups {
		s.sendGroup(ctx, g, now)
	}
}

// Render builds the message for one recipient. The owner gets the full version
// (with the pre-event tips); everyone else gets the short one.
func Render(d Due, now time.Time) string {
	m := remind.Message{Title: d.Title, Location: d.Location, Notes: d.Notes, Start: d.EventStart, Late: d.Late}
	if d.IsSelf {
		m.Tips = d.Tips
	}
	return remind.Format(m, now)
}

func (s *Scheduler) sendGroup(ctx context.Context, g *group, now time.Time) {
	ds := g.ds
	// Owner first, so a nearly-empty quota is spent on the person who set it up.
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].IsSelf && !ds[j].IsSelf })

	if s.MonthlyLimit > 0 {
		used, err := s.Store.MonthUsage(ctx, now.In(plan.Taipei).Format("2006-01"))
		if err != nil {
			log.Printf("scheduler: month usage: %v", err) // fail open: better a reminder than none
		} else {
			remaining := s.MonthlyLimit - used
			if remaining < 0 {
				remaining = 0
			}
			if remaining < len(ds) {
				for _, d := range ds[remaining:] {
					s.log("fail", s.Store.MarkFailed(ctx, d.ID, d.Attempts, "quota_exceeded"))
				}
				ds = ds[:remaining]
			}
		}
	}
	for len(ds) > 0 {
		n := len(ds)
		if n > line.MaxMulticast {
			n = line.MaxMulticast
		}
		s.send(ctx, ds[:n], g.text, now)
		ds = ds[n:]
	}
}

func (s *Scheduler) send(ctx context.Context, ds []Due, text string, now time.Time) {
	ids := make([]int64, len(ds))
	to := make([]string, len(ds))
	for i, d := range ds {
		ids[i], to[i] = d.ID, d.LineUserID
	}
	var err error
	if len(ds) == 1 {
		err = s.Sender.Push(ctx, to[0], text, RetryKey(ids...))
	} else {
		err = s.Sender.Multicast(ctx, to, text, RetryKey(ids...))
	}
	if err == nil {
		for _, d := range ds {
			s.log("sent", s.Store.MarkSent(ctx, d.ID, now))
		}
		return
	}

	reason := errReason(err)
	var perm permanent
	isPerm := errors.As(err, &perm) && perm.Permanent()
	if isPerm && len(ds) == 1 {
		// With one recipient we know whose fault it is. With a multicast we cannot tell.
		var gone recipientGone
		if errors.As(err, &gone) && gone.RecipientGone() {
			s.log("invalidate", s.Store.MarkContactInvalid(ctx, ds[0].ContactID))
		}
	}
	for _, d := range ds {
		attempts := d.Attempts + 1
		if isPerm {
			s.log("fail", s.Store.MarkFailed(ctx, d.ID, attempts, reason))
			continue
		}
		if next, ok := remind.NextAttempt(attempts, now); ok {
			s.log("retry", s.Store.Retry(ctx, d.ID, attempts, next, reason))
		} else {
			s.log("fail", s.Store.MarkFailed(ctx, d.ID, attempts, reason))
		}
	}
}

func (s *Scheduler) log(what string, err error) {
	if err != nil {
		log.Printf("scheduler: %s: %v", what, err)
	}
}

// errReason keeps the stored/logged reason free of message content.
func errReason(err error) string {
	var se interface{ Error() string }
	if errors.As(err, &se) {
		return se.Error()
	}
	return "send_failed"
}

// RetryKey is a stable UUID-shaped key for a set of deliveries, so a retry after
// a crash between "LINE accepted" and "we recorded it" cannot deliver twice.
func RetryKey(deliveryIDs ...int64) string {
	ids := append([]int64(nil), deliveryIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	h := sha256.Sum256([]byte("vpa-delivery-" + strings.Join(parts, "-")))
	b := h[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
