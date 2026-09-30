// Package authcore holds the dependency-free auth logic (allowlist, signed
// cookies, session tokens) so it can be unit-tested in isolation.
package authcore

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	SessionCookieName = "vpa_session"
	TxCookieName      = "vpa_oauth_tx"
	SessionTTL        = 30 * 24 * time.Hour
	TxTTL             = 10 * time.Minute
)

// EmailAllowed reports whether email matches the single allowed account.
// Comparison ignores case and surrounding whitespace; empty never matches.
func EmailAllowed(email, allowed string) bool {
	e, a := strings.ToLower(strings.TrimSpace(email)), strings.ToLower(strings.TrimSpace(allowed))
	return e != "" && a != "" && subtle.ConstantTimeCompare([]byte(e), []byte(a)) == 1
}

// NewToken returns a random URL-safe token and its SHA-256 hash (stored in DB).
func NewToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// SessionCookie builds the session cookie: HttpOnly, SameSite=Lax, Secure on HTTPS.
func SessionCookie(token string, expires time.Time, secure bool) *http.Cookie {
	return &http.Cookie{
		Name: SessionCookieName, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	}
}

func ClearCookie(name, path string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	}
}

// Tx is the short-lived OAuth transaction state kept in a signed cookie.
type Tx struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Purpose  string `json:"p,omitempty"` // "" = login, "calendar" = incremental calendar authorisation
	Expires  int64  `json:"e"`           // unix seconds
}

func SignTx(secret string, tx Tx) (string, error) {
	payload, err := json.Marshal(tx)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + mac(secret, p), nil
}

var ErrBadTx = errors.New("invalid or expired oauth transaction")

func VerifyTx(secret, value string, now time.Time) (Tx, error) {
	var tx Tx
	p, sig, ok := strings.Cut(value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(mac(secret, p))) {
		return tx, ErrBadTx
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || json.Unmarshal(raw, &tx) != nil || now.Unix() > tx.Expires {
		return Tx{}, ErrBadTx
	}
	return tx, nil
}

func mac(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
