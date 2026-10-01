package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"voiceplan/internal/calsync"
	"voiceplan/internal/gcal"
	"voiceplan/internal/plan"
	"voiceplan/internal/remind"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sync implements calsync.Store.
type Sync struct{ pool *pgxpool.Pool }

func NewSync(pool *pgxpool.Pool) *Sync { return &Sync{pool} }

// UserIDs lists users whose Google authorisation is healthy.
func (s *Sync) UserIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT user_id FROM google_credentials WHERE status = 'ok'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Sync) State(ctx context.Context, userID int64) (calsync.State, error) {
	var st calsync.State
	err := s.pool.QueryRow(ctx,
		`SELECT sync_token, last_synced_at, last_error FROM calendar_sync WHERE user_id = $1`, userID).
		Scan(&st.Token, &st.LastSyncedAt, &st.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return calsync.State{}, nil
	}
	return st, err
}

// SaveState stores the token and outcome. last_synced_at only moves on success.
func (s *Sync) SaveState(ctx context.Context, userID int64, token string, now time.Time, lastError string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO calendar_sync (user_id, sync_token, last_synced_at, last_error)
		VALUES ($1, $2, CASE WHEN $4 = '' THEN $3::timestamptz END, $4)
		ON CONFLICT (user_id) DO UPDATE SET
			sync_token = EXCLUDED.sync_token,
			last_synced_at = CASE WHEN $4 = '' THEN $3::timestamptz ELSE calendar_sync.last_synced_at END,
			last_error = $4, updated_at = now()`, userID, token, now.UTC(), lastError)
	return err
}

// Apply applies one remote change atomically: the event row and its pending reminders.
func (s *Sync) Apply(ctx context.Context, userID int64, remote gcal.RemoteEvent, now time.Time) (calsync.Kind, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var l calsync.LocalEvent
	err = tx.QueryRow(ctx, `
		SELECT id, title, start_at, duration_minutes, location, notes, status
		FROM events WHERE user_id = $1 AND google_event_id = $2 FOR UPDATE`, userID, remote.ID).
		Scan(&l.ID, &l.Title, &l.Start, &l.DurationMinutes, &l.Location, &l.Notes, &l.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return calsync.Ignore, nil // not created by this app
	}
	if err != nil {
		return "", err
	}
	l.Start = l.Start.In(plan.Taipei)

	act := calsync.Decide(l, remote)
	switch act.Kind {
	case calsync.Cancel:
		if err := cancelEvent(ctx, tx, l.ID); err != nil {
			return "", err
		}
	case calsync.Update:
		if _, err := tx.Exec(ctx, `
			UPDATE events SET title = $2, start_at = $3, duration_minutes = $4, location = $5, notes = $6
			WHERE id = $1`, l.ID, act.Title, act.Start.UTC(), act.DurationMinutes, act.Location, act.Notes); err != nil {
			return "", err
		}
		if act.StartChanged {
			if err := reschedulePending(ctx, tx, l.ID, act.Start, now); err != nil {
				return "", err
			}
		}
	}
	return act.Kind, tx.Commit(ctx)
}

func cancelEvent(ctx context.Context, tx pgx.Tx, eventID int64) error {
	if _, err := tx.Exec(ctx, `UPDATE events SET status = 'cancelled' WHERE id = $1`, eventID); err != nil {
		return err
	}
	// Only reminders that have not gone out yet; sent ones stay as history.
	_, err := tx.Exec(ctx,
		`UPDATE deliveries SET status = 'skipped', last_error = 'event_deleted' WHERE event_id = $1 AND status = 'pending'`, eventID)
	return err
}

// reschedulePending moves every not-yet-sent reminder to the new start time,
// using the same rules as when the reminder was first created.
func reschedulePending(ctx context.Context, tx pgx.Tx, eventID int64, newStart, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id, lead_minutes FROM deliveries WHERE event_id = $1 AND status = 'pending'`, eventID)
	if err != nil {
		return err
	}
	type pend struct {
		id   int64
		lead int
	}
	var list []pend
	for rows.Next() {
		var p pend
		if err := rows.Scan(&p.id, &p.lead); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range list {
		due, late, ok := remind.Schedule(newStart, p.lead, now)
		if !ok { // moved to a time that already started
			if _, err := tx.Exec(ctx, `UPDATE deliveries SET status = 'skipped', last_error = 'event_started' WHERE id = $1`, p.id); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET due_at = $2, next_attempt_at = $2, late = $3 WHERE id = $1`, p.id, due.UTC(), late); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileMissing handles deletions we may have missed (after a full resync):
// future local events that Google no longer lists are treated as deleted.
func (s *Sync) ReconcileMissing(ctx context.Context, userID int64, seenIDs []string, now time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, google_event_id FROM events
		WHERE user_id = $1 AND status = 'active' AND google_event_id <> '' AND start_at >= $2`,
		userID, now.Add(-24*time.Hour).UTC())
	if err != nil {
		return 0, err
	}
	seen := make(map[string]bool, len(seenIDs))
	for _, id := range seenIDs {
		seen[id] = true
	}
	var candidates, missing []int64
	for rows.Next() {
		var id int64
		var gid string
		if err := rows.Scan(&id, &gid); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, id)
		if !seen[gid] {
			missing = append(missing, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	// A listing that contains none of several known events is more likely an API
	// hiccup than a user deleting everything: refuse to cancel on that evidence.
	if len(candidates) >= 3 && len(missing) == len(candidates) {
		return 0, fmt.Errorf("full sync returned none of %d known events; not cancelling", len(candidates))
	}
	for _, id := range missing {
		if err := cancelEvent(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	return len(missing), tx.Commit(ctx)
}
