package calsync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"voiceplan/internal/gcal"
	"voiceplan/internal/plan"
)

var t0 = time.Date(2026, 10, 1, 10, 0, 0, 0, plan.Taipei)

func local() LocalEvent {
	return LocalEvent{ID: 1, Title: "看牙醫", Start: t0.Add(48 * time.Hour), DurationMinutes: 90, Location: "康美診所", Notes: "帶健保卡", Status: "active"}
}

// remoteLike builds the remote event that exactly mirrors `local()`.
func remoteLike() gcal.RemoteEvent {
	l := local()
	return gcal.RemoteEvent{ID: "g1", Title: l.Title, Location: l.Location, Notes: l.Notes, Start: l.Start, End: l.Start.Add(90 * time.Minute)}
}

func TestEchoOfOurOwnWriteIsUnchanged(t *testing.T) {
	// Writing to Google makes Google report the same event back; it must be a no-op.
	if a := Decide(local(), remoteLike()); a.Kind != Unchanged {
		t.Fatalf("%+v", a)
	}
}

func TestMovedEventRequestsReschedule(t *testing.T) {
	r := remoteLike()
	r.Start, r.End = r.Start.Add(3*time.Hour), r.End.Add(3*time.Hour)
	a := Decide(local(), r)
	if a.Kind != Update || !a.StartChanged || !a.Start.Equal(r.Start) || a.DurationMinutes != 90 {
		t.Fatalf("%+v", a)
	}
}

func TestTimeZoneRepresentationDoesNotCountAsAChange(t *testing.T) {
	r := remoteLike()
	r.Start, r.End = r.Start.UTC(), r.End.UTC() // same instant, different zone
	if a := Decide(local(), r); a.Kind != Unchanged {
		t.Fatalf("%+v", a)
	}
}

func TestTitleLocationNotesAndDurationChangesDoNotReschedule(t *testing.T) {
	r := remoteLike()
	r.Title, r.Location, r.Notes = "洗牙", "新診所", "改帶二代健保卡"
	r.End = r.Start.Add(30 * time.Minute)
	a := Decide(local(), r)
	if a.Kind != Update || a.StartChanged || a.Title != "洗牙" || a.Location != "新診所" || a.Notes != "改帶二代健保卡" || a.DurationMinutes != 30 {
		t.Fatalf("%+v", a)
	}
}

func TestDeletedInGoogleCancels(t *testing.T) {
	if a := Decide(local(), gcal.RemoteEvent{ID: "g1", Cancelled: true}); a.Kind != Cancel {
		t.Fatalf("%+v", a)
	}
}

func TestAlreadyCancelledOrAllDayAreIgnored(t *testing.T) {
	l := local()
	l.Status = "cancelled"
	if a := Decide(l, remoteLike()); a.Kind != Ignore {
		t.Fatalf("a cancelled event must stay cancelled: %+v", a)
	}
	r := remoteLike()
	r.AllDay, r.Start, r.End = true, time.Time{}, time.Time{}
	if a := Decide(local(), r); a.Kind != Ignore {
		t.Fatalf("all-day has no reminder time: %+v", a)
	}
}

func TestOddRemoteDataIsHandledSafely(t *testing.T) {
	r := remoteLike()
	r.Title = "   "
	r.End = r.Start.Add(-time.Hour) // end before start
	a := Decide(local(), r)
	if a.Kind != Unchanged {
		t.Fatalf("blank title and bad end must fall back to local values: %+v", a)
	}
	r = remoteLike()
	r.End = r.Start.Add(72 * time.Hour)
	if a := Decide(local(), r); a.DurationMinutes != 1440 {
		t.Fatalf("multi-day is capped: %+v", a)
	}
	r = remoteLike()
	r.Title = strings.Repeat("字", 300)
	if a := Decide(local(), r); len([]rune(a.Title)) != 200 {
		t.Fatalf("title must be clipped: %d", len([]rune(a.Title)))
	}
}

// ---- Syncer -----------------------------------------------------------------

type fakeRemote struct {
	calls  []string // the token of each call
	events []gcal.RemoteEvent
	next   string
	errs   []error
}

func (f *fakeRemote) ListChanges(_ context.Context, _ int64, token string, _ time.Time) ([]gcal.RemoteEvent, string, error) {
	f.calls = append(f.calls, token)
	if len(f.errs) > 0 {
		e := f.errs[0]
		f.errs = f.errs[1:]
		if e != nil {
			return nil, "", e
		}
	}
	return f.events, f.next, nil
}

type fakeStore struct {
	state      State
	saved      []State
	applied    []string
	applyErr   error
	reconciled [][]string
	kinds      map[string]Kind
}

func (f *fakeStore) UserIDs(context.Context) ([]int64, error)    { return []int64{1}, nil }
func (f *fakeStore) State(context.Context, int64) (State, error) { return f.state, nil }
func (f *fakeStore) SaveState(_ context.Context, _ int64, tok string, _ time.Time, e string) error {
	f.state = State{Token: tok, LastError: e}
	f.saved = append(f.saved, f.state)
	return nil
}
func (f *fakeStore) Apply(_ context.Context, _ int64, r gcal.RemoteEvent, _ time.Time) (Kind, error) {
	if f.applyErr != nil {
		return "", f.applyErr
	}
	f.applied = append(f.applied, r.ID)
	if k, ok := f.kinds[r.ID]; ok {
		return k, nil
	}
	return Unchanged, nil
}
func (f *fakeStore) ReconcileMissing(_ context.Context, _ int64, seen []string, _ time.Time) (int, error) {
	f.reconciled = append(f.reconciled, seen)
	return 0, nil
}

func newSyncer(r *fakeRemote, s *fakeStore) *Syncer {
	sy := New(r, s, 5*time.Minute)
	sy.Now = func() time.Time { return t0 }
	return sy
}

func TestTokenAdvancesOnlyAfterEverythingWasApplied(t *testing.T) {
	r := &fakeRemote{events: []gcal.RemoteEvent{{ID: "a"}, {ID: "b"}}, next: "T2"}
	s := &fakeStore{state: State{Token: "T1"}, applyErr: errors.New("db down")}
	if _, err := newSyncer(r, s).SyncUser(context.Background(), 1); err == nil {
		t.Fatal("expected the apply error")
	}
	if s.state.Token != "T1" || s.state.LastError != "apply_failed" {
		t.Fatalf("token must not advance after a failure: %+v", s.state)
	}
	s.applyErr = nil // next run repeats the same changes, then advances
	if _, err := newSyncer(r, s).SyncUser(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if s.state.Token != "T2" || s.state.LastError != "" || len(s.applied) != 2 {
		t.Fatalf("%+v %v", s.state, s.applied)
	}
}

func TestIncrementalSyncDoesNotReconcile(t *testing.T) {
	r := &fakeRemote{events: []gcal.RemoteEvent{{ID: "a"}}, next: "T2"}
	s := &fakeStore{state: State{Token: "T1"}}
	if _, err := newSyncer(r, s).SyncUser(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(s.reconciled) != 0 {
		t.Fatal("an incremental sync only sees changes; absence means nothing")
	}
}

func TestFirstSyncIsFullAndReconciles(t *testing.T) {
	r := &fakeRemote{events: []gcal.RemoteEvent{{ID: "a"}, {ID: "b"}}, next: "T1"}
	s := &fakeStore{}
	if _, err := newSyncer(r, s).SyncUser(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if r.calls[0] != "" || len(s.reconciled) != 1 || len(s.reconciled[0]) != 2 || s.state.Token != "T1" {
		t.Fatalf("%v %v %+v", r.calls, s.reconciled, s.state)
	}
}

func TestExpiredTokenFallsBackToOneFullResync(t *testing.T) {
	r := &fakeRemote{errs: []error{gcal.ErrSyncTokenInvalid, nil}, events: []gcal.RemoteEvent{{ID: "a"}}, next: "FRESH"}
	s := &fakeStore{state: State{Token: "OLD"}}
	if _, err := newSyncer(r, s).SyncUser(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 || r.calls[0] != "OLD" || r.calls[1] != "" {
		t.Fatalf("expected OLD then a full request: %v", r.calls)
	}
	if s.state.Token != "FRESH" || len(s.reconciled) != 1 {
		t.Fatalf("a full resync must reconcile deletions we may have missed: %+v %v", s.state, s.reconciled)
	}
}

func TestReauthIsRecordedAndKeepsToken(t *testing.T) {
	r := &fakeRemote{errs: []error{gcal.ErrReauthRequired}}
	s := &fakeStore{state: State{Token: "T1"}}
	_, err := newSyncer(r, s).SyncUser(context.Background(), 1)
	if !errors.Is(err, gcal.ErrReauthRequired) || s.state.Token != "T1" || s.state.LastError != "reauth_required" {
		t.Fatalf("%v %+v", err, s.state)
	}
}

func TestStatsCountEachOutcome(t *testing.T) {
	r := &fakeRemote{events: []gcal.RemoteEvent{{ID: "u"}, {ID: "c"}, {ID: "n"}, {ID: "i"}}, next: "T2"}
	s := &fakeStore{state: State{Token: "T1"}, kinds: map[string]Kind{"u": Update, "c": Cancel, "n": Unchanged, "i": Ignore}}
	st, err := newSyncer(r, s).SyncUser(context.Background(), 1)
	if err != nil || st.Updated != 1 || st.Cancelled != 1 || st.Unchanged != 1 || st.Ignored != 1 {
		t.Fatalf("%+v %v", st, err)
	}
}
