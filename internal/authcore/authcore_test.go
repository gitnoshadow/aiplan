package authcore

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestEmailAllowed(t *testing.T) {
	cases := []struct {
		email, allowed string
		want           bool
	}{
		{"me@example.com", "me@example.com", true},
		{"  ME@Example.COM ", "me@example.com", true},
		{"me@example.com", " Me@Example.com  ", true},
		{"other@example.com", "me@example.com", false},
		{"me@example.com.evil.com", "me@example.com", false},
		{"", "", false},
		{"", "me@example.com", false},
		{"me@example.com", "", false},
	}
	for _, c := range cases {
		if got := EmailAllowed(c.email, c.allowed); got != c.want {
			t.Errorf("EmailAllowed(%q,%q)=%v want %v", c.email, c.allowed, got, c.want)
		}
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	c := SessionCookie("tok", time.Now().Add(time.Hour), true)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("bad attributes: %+v", c)
	}
	if SessionCookie("tok", time.Now(), false).Secure {
		t.Fatal("Secure must follow the flag (off for local http)")
	}
}

func TestTokenHashing(t *testing.T) {
	tok, hash, err := NewToken()
	if err != nil || len(tok) < 40 || len(hash) != 32 {
		t.Fatalf("bad token: %v", err)
	}
	if string(HashToken(tok)) != string(hash) {
		t.Fatal("hash mismatch")
	}
	tok2, _, _ := NewToken()
	if tok == tok2 {
		t.Fatal("tokens must be unique")
	}
}

func TestTxSignVerify(t *testing.T) {
	secret := strings.Repeat("k", 32)
	now := time.Unix(1_700_000_000, 0)
	tx := Tx{State: "s", Nonce: "n", Verifier: "v", Purpose: "calendar", Expires: now.Add(TxTTL).Unix()}
	v, err := SignTx(secret, tx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyTx(secret, v, now)
	if err != nil || got != tx {
		t.Fatalf("verify failed: %v %+v", err, got)
	}
	if _, err := VerifyTx(secret, v, now.Add(TxTTL+time.Second)); err == nil {
		t.Fatal("expired tx must fail")
	}
	if _, err := VerifyTx("another-secret-another-secret-123", v, now); err == nil {
		t.Fatal("wrong secret must fail")
	}
	p, sig, _ := strings.Cut(v, ".")
	if _, err := VerifyTx(secret, p+"x."+sig, now); err == nil {
		t.Fatal("tampered payload must fail")
	}
	if _, err := VerifyTx(secret, "garbage", now); err == nil {
		t.Fatal("garbage must fail")
	}
}
