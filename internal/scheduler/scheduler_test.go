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
	mu          sync.Mutex
	rows        []*row
	sent        int
	usage       int
	invalidated []int64
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
func (m *memStore) MarkContactInvalid(_ context.Context, id int64) error {
	m.invalidated = append(m.invalidated, id)
	return nil
}
func (m *memStore) MonthUsage(context.Context, string) (int, error) { return m.usage + m.sent, nil }

type fakeSender struct {
	calls  []string
	keys   []string
	multis [][]string // recipients of each multicast
	errs   []error    // consumed in order; nil entries succeed
}

func (f *fakeSender) next() error {
	if len(f.errs) > 0 {
		e := f.errs[0]
		f.errs = f.errs[1:]
		return e
	}
	return nil
}

func (f *fakeSender) Push(_ context.Context, to, msg, key string) error {
	f.calls = append(f.calls, to+"|"+msg)
	f.keys = append(f.keys, key)
	return f.next()
}

func (f *fakeSender) Multicast(_ context.Context, to []string, msg, key string) error {
	f.calls = append(f.calls, "MULTI|"+msg)
	f.keys = append(f.keys, key)
	f.multis = append(f.multis, to)
	return f.next()
}

func newSched(rows []*row, snd *fakeSender, now time.Time) (*Scheduler, *memStore) {
	st := &memStore{rows: rows}
	s := New(st, snd)
	s.Now = func() time.Time { return now }
	return s, st
}

func pending(id int64, start time.Time) *row {
	return &row{Due: Due{ID: id, ContactID: id, EventStart: start, Title: "看牙醫", LineUserID: "U1", ContactStatus: "active",
		Enabled: true, IsSelf: true, Tips: "・帶健保卡", Notes: "掛號 5 號"}, status: "pending", next: t0.Add(-time.Minute)}
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

func other(id int64, user string, start time.Time) *row {
	r := pending(id, start)
	r.IsSelf, r.LineUserID = false, user
	return r
}

func TestOwnerGetsFullVersionOthersGetShortWithNotes(t *testing.T) {
	snd := &fakeSender{}
	start := t0.Add(time.Hour)
	s, _ := newSched([]*row{pending(1, start), other(2, "U2", start)}, snd, t0)
	s.Tick(context.Background())
	if len(snd.calls) != 2 {
		t.Fatalf("%v", snd.calls)
	}
	var full, short string
	for _, c := range snd.calls {
		if strings.HasPrefix(c, "U1|") {
			full = c
		}
		if strings.HasPrefix(c, "U2|") {
			short = c
		}
	}
	if !strings.Contains(full, "行前注意事項") || !strings.Contains(full, "・帶健保卡") {
		t.Fatalf("owner must get tips: %s", full)
	}
	if strings.Contains(short, "行前注意事項") || strings.Contains(short, "・帶健保卡") {
		t.Fatalf("family must NOT get the tips: %s", short)
	}
	if !strings.Contains(short, "掛號 5 號") || !strings.Contains(short, "看牙醫") {
		t.Fatalf("short version keeps title and notes: %s", short)
	}
}

func TestSameMessageRecipientsAreCombinedIntoOneMulticast(t *testing.T) {
	snd := &fakeSender{}
	start := t0.Add(time.Hour)
	s, st := newSched([]*row{other(1, "U1", start), other(2, "U2", start), other(3, "U3", start)}, snd, t0)
	s.Tick(context.Background())
	s.Tick(context.Background())
	if len(snd.calls) != 1 || len(snd.multis) != 1 || len(snd.multis[0]) != 3 {
		t.Fatalf("expected a single multicast to 3 people: %v", snd.calls)
	}
	for _, r := range st.rows {
		if r.status != "sent" {
			t.Fatalf("%+v", r)
		}
	}
	if st.sent != 3 {
		t.Fatalf("every recipient counts as one message: %d", st.sent)
	}
}

func TestSinglePushAndMulticastUseDifferentStableKeys(t *testing.T) {
	if RetryKey(1, 2, 3) != RetryKey(3, 1, 2) {
		t.Fatal("the key must not depend on order")
	}
	if RetryKey(1, 2) == RetryKey(1, 2, 3) || RetryKey(1) == RetryKey(2) {
		t.Fatal("different delivery sets need different keys")
	}
}

func TestQuotaGuardStopsBeforeExceedingAndOwnerGoesFirst(t *testing.T) {
	snd := &fakeSender{}
	start := t0.Add(time.Hour)
	// Two recipients with identical text would normally be one multicast; make one the owner.
	own := pending(1, start)
	own.Tips = "" // same rendered text as the family version
	fam := other(2, "U2", start)
	s, st := newSched([]*row{fam, own}, snd, t0)
	s.MonthlyLimit = 200
	st.usage = 199 // one message left
	s.Tick(context.Background())
	if st.rows[1].status != "sent" || st.rows[0].status != "failed" || st.rows[0].reason != "quota_exceeded" {
		t.Fatalf("owner=%s family=%s/%s", st.rows[1].status, st.rows[0].status, st.rows[0].reason)
	}
	if len(snd.calls) != 1 || !strings.HasPrefix(snd.calls[0], "U1|") {
		t.Fatalf("only the owner may be sent: %v", snd.calls)
	}
}

func TestQuotaFullSendsNothing(t *testing.T) {
	snd := &fakeSender{}
	s, st := newSched([]*row{pending(1, t0.Add(time.Hour))}, snd, t0)
	s.MonthlyLimit = 200
	st.usage = 200
	s.Tick(context.Background())
	if len(snd.calls) != 0 || st.rows[0].reason != "quota_exceeded" {
		t.Fatalf("%v %+v", snd.calls, st.rows[0])
	}
}

func TestDisabledContactIsSkipped(t *testing.T) {
	r := other(1, "U2", t0.Add(time.Hour))
	r.Enabled = false
	snd := &fakeSender{}
	s, st := newSched([]*row{r}, snd, t0)
	s.Tick(context.Background())
	if len(snd.calls) != 0 || st.rows[0].status != "skipped" || st.rows[0].reason != "contact_disabled" {
		t.Fatalf("%+v", st.rows[0])
	}
}

func TestBlockedRecipientFlagsContactButBadTokenDoesNot(t *testing.T) {
	snd := &fakeSender{errs: []error{&line.StatusError{Code: 403}}}
	s, st := newSched([]*row{pending(7, t0.Add(time.Hour))}, snd, t0)
	s.Tick(context.Background())
	if len(st.invalidated) != 1 || st.invalidated[0] != 7 || st.rows[0].status != "failed" {
		t.Fatalf("a recipient problem must flag the contact: %v %s", st.invalidated, st.rows[0].status)
	}
	// 401 = our channel token is wrong. Flagging everybody as blocked would be a disaster.
	snd = &fakeSender{errs: []error{&line.StatusError{Code: 401}}}
	s, st = newSched([]*row{pending(8, t0.Add(time.Hour))}, snd, t0)
	s.Tick(context.Background())
	if len(st.invalidated) != 0 || st.rows[0].status != "failed" {
		t.Fatalf("401 must not flag the contact: %v", st.invalidated)
	}
}

func TestMulticastFailureDoesNotBlameAnyone(t *testing.T) {
	snd := &fakeSender{errs: []error{&line.StatusError{Code: 400}}}
	start := t0.Add(time.Hour)
	s, st := newSched([]*row{other(1, "U1", start), other(2, "U2", start)}, snd, t0)
	s.Tick(context.Background())
	if len(st.invalidated) != 0 {
		t.Fatalf("cannot tell which recipient failed in a multicast: %v", st.invalidated)
	}
}

func TestMulticastTransientFailureRetriesEveryone(t *testing.T) {
	snd := &fakeSender{errs: []error{&line.StatusError{Code: 503}}}
	start := t0.Add(5 * time.Hour)
	s, st := newSched([]*row{other(1, "U1", start), other(2, "U2", start)}, snd, t0)
	s.Tick(context.Background())
	for _, r := range st.rows {
		if r.status != "pending" || r.Attempts != 1 || !r.next.Equal(t0.Add(time.Minute)) {
			t.Fatalf("%+v", r)
		}
	}
}
