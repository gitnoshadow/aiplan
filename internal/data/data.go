// Package data holds the Postgres implementations of the stores used by the
// gcal and api packages.
package data

import (
	"context"
	"errors"
	"time"

	"voiceplan/internal/api"
	"voiceplan/internal/gcal"
	"voiceplan/internal/plan"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- Google credentials ----------------------------------------------------

type Creds struct{ pool *pgxpool.Pool }

func NewCreds(pool *pgxpool.Pool) *Creds { return &Creds{pool} }

func (c *Creds) Get(ctx context.Context, userID int64) (gcal.Creds, error) {
	var out gcal.Creds
	var status string
	err := c.pool.QueryRow(ctx,
		`SELECT refresh_token_enc, calendar_id, status FROM google_credentials WHERE user_id = $1`, userID).
		Scan(&out.RefreshTokenEnc, &out.CalendarID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return gcal.Creds{}, gcal.ErrNoCredentials
	}
	out.NeedsReauth = status == "reauth_needed"
	return out, err
}

func (c *Creds) Save(ctx context.Context, userID int64, enc []byte) error {
	_, err := c.pool.Exec(ctx, `
		INSERT INTO google_credentials (user_id, refresh_token_enc) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE
		SET refresh_token_enc = EXCLUDED.refresh_token_enc, status = 'ok', updated_at = now()`, userID, enc)
	return err
}

func (c *Creds) SetCalendarID(ctx context.Context, userID int64, id string) error {
	_, err := c.pool.Exec(ctx,
		`UPDATE google_credentials SET calendar_id = $2, updated_at = now() WHERE user_id = $1`, userID, id)
	return err
}

func (c *Creds) MarkReauth(ctx context.Context, userID int64) error {
	_, err := c.pool.Exec(ctx,
		`UPDATE google_credentials SET status = 'reauth_needed', updated_at = now() WHERE user_id = $1`, userID)
	return err
}

// ---- Events ----------------------------------------------------------------

type Events struct{ pool *pgxpool.Pool }

func NewEvents(pool *pgxpool.Pool) *Events { return &Events{pool} }

const cols = `id, title, start_at, duration_minutes, location, notes, category, status, google_event_id`

func scan(row pgx.Row) (api.EventRow, error) {
	var r api.EventRow
	err := row.Scan(&r.ID, &r.Title, &r.StartUTC, &r.DurationMinutes, &r.Location, &r.Notes, &r.Category, &r.Status, &r.GoogleEventID)
	r.StartTime = r.StartUTC.In(plan.Taipei).Format(time.RFC3339)
	return r, err
}

func (e *Events) Reserve(ctx context.Context, userID int64, requestID string, ev plan.Event) (api.EventRow, bool, error) {
	row, err := scan(e.pool.QueryRow(ctx, `
		INSERT INTO events (user_id, request_id, title, start_at, duration_minutes, location, notes, category)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (user_id, request_id) DO NOTHING
		RETURNING `+cols, userID, requestID, ev.Title, ev.Start.UTC(), ev.DurationMinutes, ev.Location, ev.Notes, ev.Category))
	if err == nil {
		return row, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return api.EventRow{}, false, err
	}
	row, err = scan(e.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM events WHERE user_id = $1 AND request_id = $2`, userID, requestID))
	return row, false, err
}

func (e *Events) Complete(ctx context.Context, id int64, googleEventID string) error {
	_, err := e.pool.Exec(ctx,
		`UPDATE events SET status = 'active', google_event_id = $2 WHERE id = $1`, id, googleEventID)
	return err
}

func (e *Events) Abandon(ctx context.Context, id int64) error {
	_, err := e.pool.Exec(ctx, `DELETE FROM events WHERE id = $1 AND status = 'pending'`, id)
	return err
}

func (e *Events) List(ctx context.Context, userID int64, from time.Time, limit int) ([]api.EventRow, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT `+cols+` FROM events WHERE user_id = $1 AND status = 'active' AND start_at >= $2
		 ORDER BY start_at ASC LIMIT $3`, userID, from.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.EventRow
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (e *Events) Get(ctx context.Context, userID, id int64) (api.EventRow, error) {
	return scan(e.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM events WHERE id = $1 AND user_id = $2 AND status = 'active'`, id, userID))
}

func (e *Events) Delete(ctx context.Context, userID, id int64) error {
	_, err := e.pool.Exec(ctx, `DELETE FROM events WHERE id = $1 AND user_id = $2`, id, userID)
	return err
}
