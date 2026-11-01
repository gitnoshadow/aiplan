package data

import (
	"context"
	"errors"

	"voiceplan/internal/api"
	"voiceplan/internal/remind"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Editor implements api.Editor: it changes an event and brings its reminders in
// line with what the user now wants, all in one transaction.
type Editor struct{ pool *pgxpool.Pool }

func NewEditor(pool *pgxpool.Pool) *Editor { return &Editor{pool} }

func (e *Editor) Apply(ctx context.Context, q api.EditReq) (api.EditResult, error) {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return api.EditResult{}, err
	}
	defer tx.Rollback(ctx)

	// Lock the row: the calendar sync also takes this lock before touching an event.
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM events WHERE id = $1 AND user_id = $2 FOR UPDATE`,
		q.EventID, q.UserID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "active") {
		return api.EditResult{}, api.ErrNotFound
	}
	if err != nil {
		return api.EditResult{}, err
	}
	ev := q.Event
	if _, err := tx.Exec(ctx, `
		UPDATE events SET title = $2, start_at = $3, duration_minutes = $4, location = $5, notes = $6, category = $7
		WHERE id = $1`, q.EventID, ev.Title, ev.Start.UTC(), ev.DurationMinutes, ev.Location, ev.Notes, ev.Category); err != nil {
		return api.EditResult{}, err
	}

	existing, err := loadExisting(ctx, tx, q.EventID)
	if err != nil {
		return api.EditResult{}, err
	}
	want, err := loadWanted(ctx, tx, q)
	if err != nil {
		return api.EditResult{}, err
	}
	plan := remind.Reconcile(existing, want, ev.Start, q.Now)

	if len(plan.Delete) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM deliveries WHERE id = ANY($1) AND status = 'pending'`, plan.Delete); err != nil {
			return api.EditResult{}, err
		}
	}
	if len(plan.Skip) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET status = 'skipped', last_error = 'event_started' WHERE id = ANY($1) AND status = 'pending'`, plan.Skip); err != nil {
			return api.EditResult{}, err
		}
	}

	hasRecipients := want.LeadMinutes > 0 && len(want.ContactIDs) > 0
	late := false
	if hasRecipients && !plan.Started {
		var ruleID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO reminder_rules (event_id, lead_minutes, tips) VALUES ($1, $2, COALESCE($3, ''))
			ON CONFLICT (event_id, lead_minutes) DO UPDATE SET tips = COALESCE($3, reminder_rules.tips)
			RETURNING id`, q.EventID, want.LeadMinutes, q.Tips).Scan(&ruleID); err != nil {
			return api.EditResult{}, err
		}
		for _, m := range plan.Reschedule {
			late = late || m.Late
			if _, err := tx.Exec(ctx,
				`UPDATE deliveries SET due_at = $2, next_attempt_at = $2, late = $3 WHERE id = $1 AND status = 'pending'`,
				m.ID, m.Due.UTC(), m.Late); err != nil {
				return api.EditResult{}, err
			}
		}
		for _, m := range plan.Revive {
			late = late || m.Late
			if _, err := tx.Exec(ctx, `
				UPDATE deliveries SET status = 'pending', attempts = 0, last_error = '', sent_at = NULL,
				       due_at = $2, next_attempt_at = $2, late = $3, rule_id = $4
				WHERE id = $1 AND status IN ('failed', 'skipped')`, m.ID, m.Due.UTC(), m.Late, ruleID); err != nil {
				return api.EditResult{}, err
			}
		}
		for _, n := range plan.Create {
			late = late || n.Late
			if _, err := tx.Exec(ctx, `
				INSERT INTO deliveries (event_id, rule_id, contact_id, lead_minutes, due_at, late, next_attempt_at)
				VALUES ($1, $2, $3, $4, $5, $6, $5)
				ON CONFLICT (event_id, contact_id, lead_minutes) DO NOTHING`,
				q.EventID, ruleID, n.ContactID, want.LeadMinutes, n.Due.UTC(), n.Late); err != nil {
				return api.EditResult{}, err
			}
		}
	}

	// A rule left with no deliveries at all (e.g. the old lead time) is just clutter.
	if _, err := tx.Exec(ctx, `
		DELETE FROM reminder_rules r WHERE r.event_id = $1
		AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.rule_id = r.id)`, q.EventID); err != nil {
		return api.EditResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return api.EditResult{}, err
	}

	res := api.EditResult{Recipients: len(want.ContactIDs), Late: late, AlreadySent: plan.AlreadySent}
	switch {
	case !hasRecipients && len(plan.Delete) > 0:
		res.Status = "cleared"
	case !hasRecipients:
		res.Status = "none"
	case plan.Started:
		res.Status = "event_started"
	default:
		res.Status = "scheduled"
	}
	return res, nil
}

func loadExisting(ctx context.Context, tx pgx.Tx, eventID int64) ([]remind.Existing, error) {
	rows, err := tx.Query(ctx, `SELECT id, contact_id, lead_minutes, status FROM deliveries WHERE event_id = $1`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []remind.Existing
	for rows.Next() {
		var x remind.Existing
		if err := rows.Scan(&x.ID, &x.ContactID, &x.LeadMinutes, &x.Status); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// loadWanted keeps only recipients who are the user's own, active and not paused.
func loadWanted(ctx context.Context, tx pgx.Tx, q api.EditReq) (remind.Desired, error) {
	if q.Reminder == nil || q.Reminder.LeadMinutes <= 0 || len(q.Reminder.ContactIDs) == 0 {
		return remind.Desired{}, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT id FROM contacts WHERE user_id = $1 AND status = 'active' AND enabled AND id = ANY($2) ORDER BY id`,
		q.UserID, q.Reminder.ContactIDs)
	if err != nil {
		return remind.Desired{}, err
	}
	defer rows.Close()
	want := remind.Desired{LeadMinutes: q.Reminder.LeadMinutes}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return remind.Desired{}, err
		}
		want.ContactIDs = append(want.ContactIDs, id)
	}
	return want, rows.Err()
}
