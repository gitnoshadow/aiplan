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
	"time"

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
}

type Sender interface {
	Push(ctx context.Context, to, msg, retryKey string) error
}

type permanent interface{ Permanent() bool }

type Scheduler struct {
	Store    Store
	Sender   Sender
	Interval time.Duration
	Now      func() time.Time
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
		for _, d := range batch {
			s.process(ctx, d, now)
		}
	}
}

func (s *Scheduler) process(ctx context.Context, d Due, now time.Time) {
	// A reminder for an event that already started is noise: skip and record it.
	if !now.Before(d.EventStart) {
		s.log("skip", s.Store.MarkSkipped(ctx, d.ID, "event_started"))
		return
	}
	if d.ContactStatus != "active" || d.LineUserID == "" {
		s.log("fail", s.Store.MarkFailed(ctx, d.ID, d.Attempts, "contact_inactive"))
		return
	}
	msg := remind.Format(remind.Message{
		Title: d.Title, Location: d.Location, Notes: d.Notes, Tips: d.Tips, Start: d.EventStart, Late: d.Late,
	}, now)

	err := s.Sender.Push(ctx, d.LineUserID, msg, RetryKey(d.ID))
	if err == nil {
		s.log("sent", s.Store.MarkSent(ctx, d.ID, now))
		return
	}
	attempts := d.Attempts + 1
	reason := errReason(err)
	var p permanent
	if errors.As(err, &p) && p.Permanent() {
		s.log("fail", s.Store.MarkFailed(ctx, d.ID, attempts, reason))
		return
	}
	if next, ok := remind.NextAttempt(attempts, now); ok {
		s.log("retry", s.Store.Retry(ctx, d.ID, attempts, next, reason))
		return
	}
	s.log("fail", s.Store.MarkFailed(ctx, d.ID, attempts, reason))
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

// RetryKey is a stable UUID-shaped key per delivery, so a retry after a crash
// between "LINE accepted" and "we recorded it" cannot deliver twice.
func RetryKey(deliveryID int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("vpa-delivery-%d", deliveryID)))
	b := h[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
