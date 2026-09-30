// Package line implements the LINE Messaging API pieces we need: webhook
// signature verification, push/reply, and one-time binding codes.
package line

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.line.me"

// VerifySignature checks X-Line-Signature: base64(HMAC-SHA256(channelSecret, body)).
func VerifySignature(secret string, body []byte, signature string) bool {
	if secret == "" || signature == "" {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	want := base64.StdEncoding.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}

// StatusError is a non-2xx answer from LINE. It never carries the body.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("line: unexpected status %d", e.Code) }

// Permanent is true for errors that retrying cannot fix (bad user, bad token).
// 429 (rate/quota) and 5xx are transient.
func (e *StatusError) Permanent() bool {
	return e.Code >= 400 && e.Code < 500 && e.Code != http.StatusRequestTimeout && e.Code != http.StatusTooManyRequests
}

type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

func NewClient(token string) *Client {
	return &Client{Token: token, BaseURL: DefaultBaseURL, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) post(ctx context.Context, path string, body any, retryKey string) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if retryKey != "" {
		req.Header.Set("X-Line-Retry-Key", retryKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("line request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	// 409 means this retry key was already accepted: the message was delivered.
	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusConflict {
		return nil
	}
	return &StatusError{Code: resp.StatusCode}
}

type textMessage struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func text(s string) []textMessage {
	r := []rune(s)
	if len(r) > 4900 { // LINE's limit is 5000 characters
		s = string(r[:4900])
	}
	return []textMessage{{Type: "text", Text: s}}
}

// Push sends one text message. The same retryKey never produces a duplicate.
func (c *Client) Push(ctx context.Context, to, msg, retryKey string) error {
	return c.post(ctx, "/v2/bot/message/push", map[string]any{"to": to, "messages": text(msg)}, retryKey)
}

// Reply answers a webhook event; replies do not use the monthly push quota.
func (c *Client) Reply(ctx context.Context, replyToken, msg string) error {
	return c.post(ctx, "/v2/bot/message/reply", map[string]any{"replyToken": replyToken, "messages": text(msg)}, "")
}

// ---- binding codes ---------------------------------------------------------

// Alphabet without look-alikes (no 0/O, 1/I/L).
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
const CodeLength = 8

// NewBindingCode returns a random code and the hash to store (never store the code).
func NewBindingCode() (code string, hash []byte, err error) {
	b := make([]byte, CodeLength)
	max := byte(256 - 256%len(codeAlphabet)) // rejection sampling: no modulo bias
	for i := 0; i < CodeLength; {
		var r [1]byte
		if _, err = rand.Read(r[:]); err != nil {
			return "", nil, err
		}
		if r[0] >= max {
			continue
		}
		b[i] = codeAlphabet[int(r[0])%len(codeAlphabet)]
		i++
	}
	code = string(b)
	return code, HashCode(code), nil
}

func HashCode(code string) []byte {
	h := sha256.Sum256([]byte("binding:" + code))
	return h[:]
}

// NormalizeCode uppercases and strips spaces/dashes; ok=false if it cannot be a code.
func NormalizeCode(s string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == ' ' || r == '-' || r == '\u3000':
		case strings.ContainsRune(codeAlphabet, r):
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	if b.Len() != CodeLength {
		return "", false
	}
	return b.String(), true
}
