// Package auth wires Google OIDC login, the single-account allowlist and
// server-side sessions to HTTP handlers.
package auth

import (
	"context"
	"errors"
	"time"

	"voiceplan/internal/authcore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type User struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
}

var ErrNoSession = errors.New("no valid session")

type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, now: time.Now} }

// UpsertUser records a login for the (already allowlisted) account.
func (s *Store) UpsertUser(ctx context.Context, sub, email string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (google_sub, email) VALUES ($1, $2)
		ON CONFLICT (google_sub) DO UPDATE SET email = EXCLUDED.email, last_login_at = now()
		RETURNING id, email`, sub, email).Scan(&u.ID, &u.Email)
	return u, err
}

func (s *Store) CreateSession(ctx context.Context, userID int64) (token string, expires time.Time, err error) {
	token, hash, err := authcore.NewToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires = s.now().Add(authcore.SessionTTL)
	_, err = s.pool.Exec(ctx,
		`INSERT INTO sessions (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, hash, expires)
	return token, expires, err
}

// Lookup returns the user for a session token, or ErrNoSession if the token
// is unknown or expired.
func (s *Store) Lookup(ctx context.Context, token string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > $2`,
		authcore.HashToken(token), s.now()).Scan(&u.ID, &u.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNoSession
	}
	return u, err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, authcore.HashToken(token))
	return err
}

func (s *Store) PurgeExpired(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1`, s.now())
	return tag.RowsAffected(), err
}
