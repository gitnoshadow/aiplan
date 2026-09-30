package scheduler

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"voiceplan/internal/line"
	"voiceplan/internal/plan"
)

var t0 = time.Date(2026, 9, 30, 10, 0, 0, 0, plan.Taipei)

type row struct {
	Due
	status string
	next   time.Time
	reason string
}

type memStore struct {
	mu   sync.Mutex
	rows []*row
	sent int
}

func (m *memStore) Claim(_ context.Context, now time.Time, limit int) ([]Due, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Due
	for _, r := range m.rows {
		if r.status == "pending" && !r.next.After(now) && len(out) < limit {
			r.status = "sending"
			out = append(out, r.Due)
		}
	}
	return out, nil
}
func (m *memStore) find(id int64) *row {
	for _, r := range m.rows {
		if r.ID == id {
			return r
		}
	}
	return nil
}
func (m *memStore) MarkSent(_ context.Context, id int64, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.find(id).status = "sent"
	m.sent++
	return nil
}
func (m *memStore) MarkSkipped(_ context.Context, id int64, why string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.find(id)
	r.status, r.reason = "skipped", why
	return nil
}
func (m *memStore) MarkFailed(_ context.Context, id int64, a int, why string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.find(id)
	r.status, r.Attempts, r.reason = "failed", a, why
	return nil
}
func (m *memStore) Retry(_ context.Context, id int64, a int, next time.Time, why string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.find(id)
	r.status, r.Attempts, r.next, r.reason = "pending", a, next, why
	return nil
}
func (m *memStore) RecoverStuck(context.Context, time.Time) (int64, error) { return 0, nil }

type fakeSender struct {
	calls []string
	keys  []string
	errs  []error // consumed in order; nil entries succeed
}

func (f *fakeSender) Push(_ context.Context, to, msg, key string) error {
	f.calls = append(f.calls, to+"|"+msg)
	f.keys = append(f.keys, key)
	if len(f.errs) > 0 {
		e := f.errs[0]
		f.errs = f.errs[1:]
		return e
	}
	return nil
}

func newSched(rows []*row, snd *fakeSender, now time.Time) (*Scheduler, *memStore) {
	st := &memStore{rows: rows}
	s := New(st, snd)
	s.Now = func() time.Time { return now }
	return s, st
}

func pending(id int64, start time.Time) *row {
	return &row{Due: Due{ID: id, ContactID: 1, EventStart: start, Title: "看牙醫", LineUserID: "U1", ContactStatus: "active"},
		status: "pending", next: t0.Add(-time.Minute)}
}

func TestSendsDueReminderOnce(t *testing.T) {
	snd := &fakeSender{}
	s, st := newSched([]*row{pending(1, t0.Add(time.Hour))}, snd, t0)
	s.Tick(context.Background())
	s.Tick(context.Background()) // a second scan must not resend
	if len(snd.calls) != 1 || st.rows[0].status != "sent" || st.sent != 1 {
		t.Fatalf("calls=%d status=%s", len(snd.calls), st.rows[0].status)
	}
	if !strings.HasPrefix(snd.calls[0], "U1|⏰ 提醒:看牙醫") {
		t.Fatalf("%s", snd.calls[0])
	}
}

func TestNotDueYetIsLeftAlone(t *testing.T) {
	r := pending(1, t0.Add(3*time.Hour))
	r.next = t0.Add(2 * time.Hour)
	snd := &fakeSender{}
	s, st := newSched([]*row{r}, snd, t0)
	s.Tick(context.Background())
	if len(snd.calls) != 0 || st.rows[0].status != "pending" {
		t.Fatal("future reminders must wait")
	}
}

// Catch-up: the service was down when the reminder became due. It must still go out.
func TestCatchUpAfterDowntime(t *testing.T) {
	r := pending(1, t0.Add(30*time.Minute))
	r.next = t0.Add(-2 * time.Hour) // should have been sent two hours ago
	snd := &fakeSender{}
	s, st := newSched([]*row{r}, snd, t0)
	s.Tick(context.Background())
	if len(snd.calls) != 1 || st.rows[0].status != "sent" {
		t.Fatalf("missed reminder was not caught up: %s", st.rows[0].status)
	}
}

func TestStartedEventsAreSkippedNotSent(t *testing.T) {
	snd := &fakeSender{}
	s, st := newSched([]*row{pending(1, t0.Add(-time.Minute)), pending(2, t0)}, snd, t0)
	s.Tick(context.Background())
	if len(snd.calls) != 0 {
		t.Fatal("nothing may be sent for events that already started")
	}
	for _, r := range st.rows {
		if r.status != "skipped" || r.reason != "event_started" {
			t.Fatalf("%+v", r)
		}
	}
}

func TestInactiveContactFailsWithoutSending(t *testing.T) {
	r := pending(1, t0.Add(time.Hour))
	r.ContactStatus = "blocked"
	snd := &fakeSender{}
	s, st := newSched([]*row{r}, snd, t0)
	s.Tick(context.Background())
	if len(snd.calls) != 0 || st.rows[0].status != "failed" || st.rows[0].reason != "contact_inactive" {
		t.Fatalf("%+v", st.rows[0])
	}
}

func TestTransientFailureRetriesWithBackoffThenGivesUp(t *testing.T) {
	snd := &fakeSender{errs: []error{&line.StatusError{Code: 503}, &line.StatusError{Code: 503}, &line.StatusError{Code: 429}, &line.StatusError{Code: 500}}}
	now := t0
	s, st := newSched([]*row{pending(1, t0.Add(10*time.Hour))}, snd, now)
	wait := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	for i, w := range wait {
		s.Tick(context.Background())
		r := st.rows[0]
		if r.status != "pending" || r.Attempts != i+1 || !r.next.Equal(now.Add(w)) {
			t.Fatalf("attempt %d: status=%s attempts=%d next=%v", i+1, r.status, r.Attempts, r.next)
		}
		now = r.next
		s.Now = func() time.Time { return now }
	}
	s.Tick(context.Background()) // 4th failure: out of attempts
	if st.rows[0].status != "failed" || len(snd.calls) != 4 {
		t.Fatalf("status=%s calls=%d", st.rows[0].status, len(snd.calls))
	}
}

func TestPermanentFailureIsNotRetried(t *testing.T) {
	snd := &fakeSender{errs: []error{&line.StatusError{Code: 403}}}
	s, st := newSched([]*row{pending(1, t0.Add(time.Hour))}, snd, t0)
	s.Tick(context.Background())
	if st.rows[0].status != "failed" || len(snd.calls) != 1 {
		t.Fatalf("%s %d", st.rows[0].status, len(snd.calls))
	}
	s.Tick(context.Background())
	if len(snd.calls) != 1 {
		t.Fatal("must not retry a permanent failure")
	}
}

func TestNetworkErrorIsTransient(t *testing.T) {
	snd := &fakeSender{errs: []error{errors.New("connection reset")}}
	s, st := newSched([]*row{pending(1, t0.Add(time.Hour))}, snd, t0)
	s.Tick(context.Background())
	if st.rows[0].status != "pending" {
		t.Fatalf("%s", st.rows[0].status)
	}
}

func TestRetryKeyIsStableUUIDShaped(t *testing.T) {
	k := RetryKey(42)
	if k != RetryKey(42) || k == RetryKey(43) {
		t.Fatal("key must be stable per delivery and unique across deliveries")
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(k) {
		t.Fatalf("not UUID-shaped: %s", k)
	}
}
