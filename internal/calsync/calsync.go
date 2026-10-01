// Package calsync keeps local events and their LINE reminders in step with
// changes made directly in Google Calendar. Decide is pure; Syncer drives the loop.
package calsync

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"voiceplan/internal/gcal"
)

// LocalEvent is the part of our stored event that sync compares against.
type LocalEvent struct {
	ID              int64
	Title           string
	Start           time.Time
	DurationMinutes int
	Location        string
	Notes           string
	Status          string // active | cancelled
}

type Kind string

const (
	Ignore    Kind = "ignored"   // not ours, already cancelled, or something we cannot schedule
	Unchanged Kind = "unchanged" // includes the echo of our own writes
	Update    Kind = "updated"
	Cancel    Kind = "cancelled"
)

type Action struct {
	Kind            Kind
	Title           string
	Start           time.Time
	DurationMinutes int
	Location        string
	Notes           string
	// StartChanged means pending reminders must be rescheduled.
	StartChanged bool
}

// Decide compares a remote event with our copy and says what to do.
func Decide(local LocalEvent, remote gcal.RemoteEvent) Action {
	if local.Status != "active" {
		return Action{Kind: Ignore}
	}
	if remote.Cancelled {
		return Action{Kind: Cancel}
	}
	// All-day events have no clock time, so no reminder time can be computed.
	if remote.AllDay || remote.Start.IsZero() {
		return Action{Kind: Ignore}
	}

	dur := local.DurationMinutes
	if remote.End.After(remote.Start) {
		dur = int(remote.End.Sub(remote.Start) / time.Minute)
	}
	if dur < 1 {
		dur = 1
	}
	if dur > 24*60 {
		dur = 24 * 60
	}
	title := clip(strings.TrimSpace(remote.Title), 200)
	if title == "" {
		title = local.Title // our titles are never empty
	}
	next := Action{
		Kind: Update, Title: title, Start: remote.Start, DurationMinutes: dur,
		Location: clip(strings.TrimSpace(remote.Location), 200),
		Notes:    clip(strings.TrimSpace(remote.Notes), 2000),
	}
	next.StartChanged = !remote.Start.Equal(local.Start)
	if !next.StartChanged && dur == local.DurationMinutes && title == local.Title &&
		next.Location == strings.TrimSpace(local.Location) && next.Notes == strings.TrimSpace(local.Notes) {
		return Action{Kind: Unchanged}
	}
	return next
}

func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// ---- the loop ---------------------------------------------------------------

type State struct {
	Token        string
	LastSyncedAt *time.Time
	LastError    string
}

type Remote interface {
	ListChanges(ctx context.Context, userID int64, syncToken string, now time.Time) ([]gcal.RemoteEvent, string, error)
}

type Store interface {
	UserIDs(ctx context.Context) ([]int64, error)
	State(ctx context.Context, userID int64) (State, error)
	SaveState(ctx context.Context, userID int64, token string, now time.Time, lastError string) error
	// Apply applies one remote change to the matching local event (and its pending reminders).
	Apply(ctx context.Context, userID int64, remote gcal.RemoteEvent, now time.Time) (Kind, error)
	// ReconcileMissing cancels local future events that a FULL sync did not return.
	ReconcileMissing(ctx context.Context, userID int64, seenIDs []string, now time.Time) (int, error)
}

type Stats struct{ Updated, Cancelled, Unchanged, Ignored, Reconciled int }

type Syncer struct {
	Remote   Remote
	Store    Store
	Interval time.Duration
	Now      func() time.Time
}

func New(remote Remote, store Store, interval time.Duration) *Syncer {
	return &Syncer{Remote: remote, Store: store, Interval: interval, Now: time.Now}
}

// Run syncs every Interval until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) {
	s.syncAll(ctx)
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncAll(ctx)
		}
	}
}

func (s *Syncer) syncAll(ctx context.Context) {
	ids, err := s.Store.UserIDs(ctx)
	if err != nil {
		log.Printf("calsync: users: %v", err)
		return
	}
	for _, id := range ids {
		uctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if _, err := s.SyncUser(uctx, id); err != nil &&
			!errors.Is(err, gcal.ErrNoCredentials) && !errors.Is(err, gcal.ErrReauthRequired) {
			log.Printf("calsync: user %d: %v", id, err)
		}
		cancel()
	}
}

// SyncUser fetches and applies changes. The new token is saved only after
// every change was applied, so a failure part-way just repeats (idempotent)
// work next time instead of losing changes.
func (s *Syncer) SyncUser(ctx context.Context, userID int64) (Stats, error) {
	var st Stats
	now := s.Now()
	state, err := s.Store.State(ctx, userID)
	if err != nil {
		return st, err
	}
	token := state.Token
	full := token == ""

	events, next, err := s.Remote.ListChanges(ctx, userID, token, now)
	if errors.Is(err, gcal.ErrSyncTokenInvalid) {
		full = true
		events, next, err = s.Remote.ListChanges(ctx, userID, "", now)
	}
	if err != nil {
		s.record(ctx, userID, state.Token, now, errCode(err))
		return st, err
	}

	seen := make([]string, 0, len(events))
	for _, ev := range events {
		seen = append(seen, ev.ID)
		kind, err := s.Store.Apply(ctx, userID, ev, now)
		if err != nil {
			s.record(ctx, userID, state.Token, now, "apply_failed")
			return st, err
		}
		switch kind {
		case Update:
			st.Updated++
		case Cancel:
			st.Cancelled++
		case Unchanged:
			st.Unchanged++
		default:
			st.Ignored++
		}
	}
	if full {
		n, err := s.Store.ReconcileMissing(ctx, userID, seen, now)
		if err != nil {
			s.record(ctx, userID, state.Token, now, "reconcile_failed")
			return st, err
		}
		st.Reconciled = n
	}
	if err := s.Store.SaveState(ctx, userID, next, now, ""); err != nil {
		return st, err
	}
	if st.Updated+st.Cancelled+st.Reconciled > 0 {
		log.Printf("calsync: user %d: %d updated, %d cancelled, %d reconciled", userID, st.Updated, st.Cancelled, st.Reconciled)
	}
	return st, nil
}

// record stores the outcome without moving the token forward.
func (s *Syncer) record(ctx context.Context, userID int64, token string, now time.Time, code string) {
	if err := s.Store.SaveState(ctx, userID, token, now, code); err != nil {
		log.Printf("calsync: save state: %v", err)
	}
}

func errCode(err error) string {
	switch {
	case errors.Is(err, gcal.ErrReauthRequired):
		return "reauth_required"
	case errors.Is(err, gcal.ErrNoCredentials):
		return "not_connected"
	}
	return "sync_failed"
}

// Status is what the UI shows.
type Status struct {
	LastSyncedAt    *time.Time `json:"last_synced_at"`
	LastError       string     `json:"last_error"`
	IntervalMinutes int        `json:"interval_minutes"`
}

func (s *Syncer) Status(ctx context.Context, userID int64) (Status, error) {
	st, err := s.Store.State(ctx, userID)
	if err != nil {
		return Status{}, err
	}
	return Status{LastSyncedAt: st.LastSyncedAt, LastError: st.LastError, IntervalMinutes: int(s.Interval / time.Minute)}, nil
}
