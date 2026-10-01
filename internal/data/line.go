package data

import (
	"context"
	"errors"
	"time"

	"voiceplan/internal/api"
	"voiceplan/internal/line"
	"voiceplan/internal/plan"
	"voiceplan/internal/remind"
	"voiceplan/internal/scheduler"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- Contacts + binding (implements api.Contacts and line.Binder) ----------

type Contacts struct{ pool *pgxpool.Pool }

func NewContacts(pool *pgxpool.Pool) *Contacts { return &Contacts{pool} }

func (c *Contacts) List(ctx context.Context, userID int64) ([]api.Contact, error) {
	rows, err := c.pool.Query(ctx,
		`SELECT id, name, status, is_self, enabled FROM contacts WHERE user_id = $1 ORDER BY is_self DESC, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Contact
	for rows.Next() {
		var x api.Contact
		if err := rows.Scan(&x.ID, &x.Name, &x.Status, &x.IsSelf, &x.Enabled); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (c *Contacts) EnsureSelf(ctx context.Context, userID int64) (api.Contact, error) {
	var x api.Contact
	err := c.pool.QueryRow(ctx, `
		INSERT INTO contacts (user_id, name, is_self) VALUES ($1, '我自己', true)
		ON CONFLICT (user_id) WHERE is_self DO UPDATE SET name = contacts.name
		RETURNING id, name, status, is_self, enabled`, userID).Scan(&x.ID, &x.Name, &x.Status, &x.IsSelf, &x.Enabled)
	return x, err
}

func (c *Contacts) Get(ctx context.Context, userID, id int64) (api.Contact, error) {
	var x api.Contact
	err := c.pool.QueryRow(ctx,
		`SELECT id, name, status, is_self, enabled FROM contacts WHERE id = $1 AND user_id = $2`, id, userID).
		Scan(&x.ID, &x.Name, &x.Status, &x.IsSelf, &x.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Contact{}, api.ErrNotFound
	}
	return x, err
}

func (c *Contacts) Create(ctx context.Context, userID int64, name string) (api.Contact, error) {
	var x api.Contact
	err := c.pool.QueryRow(ctx, `
		INSERT INTO contacts (user_id, name) VALUES ($1, $2)
		RETURNING id, name, status, is_self, enabled`, userID, name).
		Scan(&x.ID, &x.Name, &x.Status, &x.IsSelf, &x.Enabled)
	return x, err
}

// Update changes only the fields that are non-nil.
func (c *Contacts) Update(ctx context.Context, userID, id int64, name *string, enabled *bool) error {
	tag, err := c.pool.Exec(ctx, `
		UPDATE contacts SET name = COALESCE($3, name), enabled = COALESCE($4, enabled)
		WHERE id = $1 AND user_id = $2`, id, userID, name, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return api.ErrNotFound
	}
	return nil
}

// Delete removes the contact. Foreign keys cascade to its binding codes,
// group memberships and delivery records, so its LINE ID and history are gone.
func (c *Contacts) Delete(ctx context.Context, userID, id int64) error {
	tag, err := c.pool.Exec(ctx, `DELETE FROM contacts WHERE id = $1 AND user_id = $2 AND NOT is_self`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return api.ErrNotFound
	}
	return nil
}

// CreateBindingCode replaces any unused code for the contact with a new one.
func (c *Contacts) CreateBindingCode(ctx context.Context, contactID int64, hash []byte, expires time.Time) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM binding_codes WHERE contact_id = $1 AND used_at IS NULL`, contactID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO binding_codes (contact_id, code_hash, expires_at) VALUES ($1, $2, $3)`,
		contactID, hash, expires); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Bind consumes a valid one-time code (line.Binder).
func (c *Contacts) Bind(ctx context.Context, hash []byte, lineUserID string, now time.Time) (string, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var codeID, contactID int64
	var name string
	err = tx.QueryRow(ctx, `
		SELECT b.id, b.contact_id, c.name FROM binding_codes b JOIN contacts c ON c.id = b.contact_id
		WHERE b.code_hash = $1 AND b.used_at IS NULL AND b.expires_at > $2
		FOR UPDATE OF b`, hash, now).Scan(&codeID, &contactID, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", line.ErrCodeInvalid
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE contacts SET line_user_id = $2, status = 'active' WHERE id = $1`, contactID, lineUserID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return "", line.ErrAlreadyBound
		}
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE binding_codes SET used_at = $2 WHERE id = $1`, codeID, now); err != nil {
		return "", err
	}
	return name, tx.Commit(ctx)
}

// SetBlocked handles unfollow (blocked) and follow (active again). Contacts that
// were never bound (no line_user_id) are unaffected.
func (c *Contacts) SetBlocked(ctx context.Context, lineUserID string, blocked bool) error {
	if blocked {
		_, err := c.pool.Exec(ctx, `UPDATE contacts SET status = 'blocked' WHERE line_user_id = $1`, lineUserID)
		return err
	}
	_, err := c.pool.Exec(ctx,
		`UPDATE contacts SET status = 'active' WHERE line_user_id = $1 AND status = 'blocked'`, lineUserID)
	return err
}

// ---- Reminders (implements api.Reminders) ----------------------------------

type Reminders struct{ pool *pgxpool.Pool }

func NewReminders(pool *pgxpool.Pool) *Reminders { return &Reminders{pool} }

// Schedule stores the rule and one delivery per active recipient. Recipients
// that are not the user's own active contacts are silently ignored.
func (r *Reminders) Schedule(ctx context.Context, q api.ScheduleReq) (api.ScheduleResult, error) {
	due, late, ok := remind.Schedule(q.Start, q.LeadMinutes, q.Now)
	if !ok {
		return api.ScheduleResult{Status: "event_started"}, nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return api.ScheduleResult{}, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx,
		`SELECT id FROM contacts WHERE user_id = $1 AND status = 'active' AND enabled AND id = ANY($2)`, q.UserID, q.ContactIDs)
	if err != nil {
		return api.ScheduleResult{}, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return api.ScheduleResult{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return api.ScheduleResult{}, err
	}
	if len(ids) == 0 {
		return api.ScheduleResult{Status: "none"}, nil
	}

	var ruleID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO reminder_rules (event_id, lead_minutes, tips) VALUES ($1, $2, $3)
		ON CONFLICT (event_id, lead_minutes) DO UPDATE SET tips = EXCLUDED.tips
		RETURNING id`, q.EventID, q.LeadMinutes, q.Tips).Scan(&ruleID); err != nil {
		return api.ScheduleResult{}, err
	}
	for _, cid := range ids {
		if _, err := tx.Exec(ctx, `
			INSERT INTO deliveries (event_id, rule_id, contact_id, lead_minutes, due_at, late, next_attempt_at)
			VALUES ($1, $2, $3, $4, $5, $6, $5)
			ON CONFLICT (event_id, contact_id, lead_minutes) DO NOTHING`,
			q.EventID, ruleID, cid, q.LeadMinutes, due.UTC(), late); err != nil {
			return api.ScheduleResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return api.ScheduleResult{}, err
	}
	return api.ScheduleResult{Status: "scheduled", Recipients: len(ids), Late: late}, nil
}

// ---- Delivery queue (implements scheduler.Store) ---------------------------

type Queue struct{ pool *pgxpool.Pool }

func NewQueue(pool *pgxpool.Pool) *Queue { return &Queue{pool} }

func (q *Queue) Claim(ctx context.Context, now time.Time, limit int) ([]scheduler.Due, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id FROM deliveries WHERE status = 'pending' AND next_attempt_at <= $1
		ORDER BY next_attempt_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE deliveries SET status = 'sending', claimed_at = $2 WHERE id = ANY($1)`, ids, now.UTC()); err != nil {
		return nil, err
	}

	rows, err = tx.Query(ctx, `
		SELECT d.id, d.contact_id, d.attempts, d.late, e.start_at, e.title, e.location, e.notes, r.tips,
		       COALESCE(c.line_user_id, ''), c.status, c.enabled, c.is_self
		FROM deliveries d
		JOIN events e ON e.id = d.event_id
		JOIN reminder_rules r ON r.id = d.rule_id
		JOIN contacts c ON c.id = d.contact_id
		WHERE d.id = ANY($1) ORDER BY d.next_attempt_at`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheduler.Due
	for rows.Next() {
		var d scheduler.Due
		if err := rows.Scan(&d.ID, &d.ContactID, &d.Attempts, &d.Late, &d.EventStart, &d.Title,
			&d.Location, &d.Notes, &d.Tips, &d.LineUserID, &d.ContactStatus, &d.Enabled, &d.IsSelf); err != nil {
			return nil, err
		}
		d.EventStart = d.EventStart.In(plan.Taipei)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, tx.Commit(ctx)
}

func (q *Queue) MarkSent(ctx context.Context, id int64, now time.Time) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE deliveries SET status = 'sent', sent_at = $2, attempts = attempts + 1, last_error = '' WHERE id = $1`,
		id, now.UTC()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO line_usage (month, sent) VALUES ($1, 1)
		ON CONFLICT (month) DO UPDATE SET sent = line_usage.sent + 1`, now.In(plan.Taipei).Format("2006-01")); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (q *Queue) MarkSkipped(ctx context.Context, id int64, reason string) error {
	_, err := q.pool.Exec(ctx, `UPDATE deliveries SET status = 'skipped', last_error = $2 WHERE id = $1`, id, reason)
	return err
}

func (q *Queue) MarkFailed(ctx context.Context, id int64, attempts int, reason string) error {
	_, err := q.pool.Exec(ctx,
		`UPDATE deliveries SET status = 'failed', attempts = $2, last_error = $3 WHERE id = $1`, id, attempts, reason)
	return err
}

func (q *Queue) Retry(ctx context.Context, id int64, attempts int, next time.Time, reason string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE deliveries SET status = 'pending', attempts = $2, next_attempt_at = $3, last_error = $4 WHERE id = $1`,
		id, attempts, next.UTC(), reason)
	return err
}

func (q *Queue) RecoverStuck(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := q.pool.Exec(ctx,
		`UPDATE deliveries SET status = 'pending' WHERE status = 'sending' AND claimed_at < $1`, olderThan.UTC())
	return tag.RowsAffected(), err
}

func (q *Queue) MarkContactInvalid(ctx context.Context, contactID int64) error {
	_, err := q.pool.Exec(ctx, `UPDATE contacts SET status = 'blocked' WHERE id = $1 AND status = 'active'`, contactID)
	return err
}

func (q *Queue) MonthUsage(ctx context.Context, month string) (int, error) {
	return monthUsage(ctx, q.pool, month)
}

// ---- Usage (implements api.Usage) --------------------------------------------

type Usage struct{ pool *pgxpool.Pool }

func NewUsage(pool *pgxpool.Pool) *Usage { return &Usage{pool} }

func (u *Usage) Month(ctx context.Context, month string) (int, error) {
	return monthUsage(ctx, u.pool, month)
}

func monthUsage(ctx context.Context, pool *pgxpool.Pool, month string) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT sent FROM line_usage WHERE month = $1`, month).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// ---- Groups (implements api.Groups) -------------------------------------------

type Groups struct{ pool *pgxpool.Pool }

func NewGroups(pool *pgxpool.Pool) *Groups { return &Groups{pool} }

func (g *Groups) List(ctx context.Context, userID int64) ([]api.Group, error) {
	rows, err := g.pool.Query(ctx, `
		SELECT g.id, g.name,
		       COALESCE(array_agg(m.contact_id ORDER BY m.contact_id) FILTER (WHERE m.contact_id IS NOT NULL), '{}')
		FROM contact_groups g LEFT JOIN contact_group_members m ON m.group_id = g.id
		WHERE g.user_id = $1 GROUP BY g.id ORDER BY g.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Group
	for rows.Next() {
		var x api.Group
		if err := rows.Scan(&x.ID, &x.Name, &x.MemberIDs); err != nil {
			return nil, err
		}
		if x.MemberIDs == nil {
			x.MemberIDs = []int64{}
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Save creates (id = 0) or updates a group. Members that are not the user's own
// contacts are silently dropped by the INSERT ... SELECT.
func (g *Groups) Save(ctx context.Context, userID, id int64, name string, memberIDs []int64) (api.Group, error) {
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return api.Group{}, err
	}
	defer tx.Rollback(ctx)

	if id == 0 {
		err = tx.QueryRow(ctx, `INSERT INTO contact_groups (user_id, name) VALUES ($1, $2) RETURNING id`, userID, name).Scan(&id)
	} else {
		var tag pgconn.CommandTag
		tag, err = tx.Exec(ctx, `UPDATE contact_groups SET name = $3 WHERE id = $1 AND user_id = $2`, id, userID, name)
		if err == nil && tag.RowsAffected() == 0 {
			return api.Group{}, api.ErrNotFound
		}
	}
	if err != nil {
		return api.Group{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM contact_group_members WHERE group_id = $1`, id); err != nil {
		return api.Group{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO contact_group_members (group_id, contact_id)
		SELECT $1, id FROM contacts WHERE user_id = $2 AND id = ANY($3)
		ON CONFLICT DO NOTHING`, id, userID, memberIDs); err != nil {
		return api.Group{}, err
	}
	rows, err := tx.Query(ctx, `SELECT contact_id FROM contact_group_members WHERE group_id = $1 ORDER BY contact_id`, id)
	if err != nil {
		return api.Group{}, err
	}
	out := api.Group{ID: id, Name: name, MemberIDs: []int64{}}
	for rows.Next() {
		var cid int64
		if err := rows.Scan(&cid); err != nil {
			rows.Close()
			return api.Group{}, err
		}
		out.MemberIDs = append(out.MemberIDs, cid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return api.Group{}, err
	}
	return out, tx.Commit(ctx)
}

func (g *Groups) Delete(ctx context.Context, userID, id int64) error {
	_, err := g.pool.Exec(ctx, `DELETE FROM contact_groups WHERE id = $1 AND user_id = $2`, id, userID)
	return err
}
