package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"voiceplan/internal/db"
)

// Integration test: runs only when TEST_DATABASE_URL points at a scratch database.
func TestSessionLifecycle(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := db.Migrate(url); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	s := NewStore(pool)
	u, err := s.UpsertUser(ctx, "sub-test", "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Lookup(ctx, tok); err != nil || got.Email != "me@example.com" {
		t.Fatalf("lookup failed: %v", err)
	}
	// Advance the clock past the TTL: the session must no longer be valid.
	s.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if _, err := s.Lookup(ctx, tok); err != ErrNoSession {
		t.Fatalf("expected ErrNoSession after expiry, got %v", err)
	}
	if n, err := s.PurgeExpired(ctx); err != nil || n < 1 {
		t.Fatalf("purge: n=%d err=%v", n, err)
	}
}
