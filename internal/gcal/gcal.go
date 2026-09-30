// Package gcal talks to Google Calendar over REST using the standard library.
// Refresh tokens are stored encrypted; access tokens are fetched on demand.
package gcal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"voiceplan/internal/crypto"
	"voiceplan/internal/plan"
)

const (
	CalendarName    = "計畫助理"
	DefaultAPIBase  = "https://www.googleapis.com/calendar/v3"
	DefaultTokenURL = "https://oauth2.googleapis.com/token"
)

var (
	ErrNoCredentials  = errors.New("google calendar not connected")
	ErrReauthRequired = errors.New("google authorization expired; reauthorization required")
)

// Creds is the stored authorisation for one user.
type Creds struct {
	RefreshTokenEnc []byte
	CalendarID      string
	NeedsReauth     bool
}

// CredStore persists credentials. Implemented by the data package.
type CredStore interface {
	Get(ctx context.Context, userID int64) (Creds, error)                 // ErrNoCredentials if absent
	Save(ctx context.Context, userID int64, refreshTokenEnc []byte) error // upsert; clears reauth flag; keeps calendar id
	SetCalendarID(ctx context.Context, userID int64, calendarID string) error
	MarkReauth(ctx context.Context, userID int64) error
}

type Service struct {
	Store        CredStore
	Key          []byte // 32-byte AES key
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	APIBase      string
	TokenURL     string
}

func NewService(store CredStore, key []byte, clientID, clientSecret string) *Service {
	return &Service{Store: store, Key: key, ClientID: clientID, ClientSecret: clientSecret,
		HTTP: &http.Client{Timeout: 20 * time.Second}, APIBase: DefaultAPIBase, TokenURL: DefaultTokenURL}
}

// Connect stores the (encrypted) refresh token and makes sure the dedicated
// calendar exists. Re-authorisation keeps the existing calendar.
func (s *Service) Connect(ctx context.Context, userID int64, refreshToken string) error {
	enc, err := crypto.Encrypt(s.Key, []byte(refreshToken))
	if err != nil {
		return err
	}
	if err := s.Store.Save(ctx, userID, enc); err != nil {
		return err
	}
	_, err = s.calendarID(ctx, userID)
	return err
}

// Status reports whether the account is connected and whether it needs reauth.
func (s *Service) Status(ctx context.Context, userID int64) (connected, needsReauth bool, err error) {
	c, err := s.Store.Get(ctx, userID)
	if errors.Is(err, ErrNoCredentials) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, c.NeedsReauth, nil
}

type tokenResp struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
}

func (s *Service) accessToken(ctx context.Context, userID int64) (string, Creds, error) {
	c, err := s.Store.Get(ctx, userID)
	if err != nil {
		return "", c, err
	}
	if c.NeedsReauth {
		return "", c, ErrReauthRequired
	}
	rt, err := crypto.Decrypt(s.Key, c.RefreshTokenEnc)
	if err != nil {
		return "", c, fmt.Errorf("decrypt refresh token: %w", err)
	}
	form := url.Values{
		"client_id": {s.ClientID}, "client_secret": {s.ClientSecret},
		"refresh_token": {string(rt)}, "grant_type": {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", c, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", c, fmt.Errorf("token refresh: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr tokenResp
	_ = json.Unmarshal(raw, &tr)
	if resp.StatusCode != http.StatusOK {
		if tr.Error == "invalid_grant" {
			// Never fail silently: flag it so the UI can ask for reauthorisation.
			if err := s.Store.MarkReauth(ctx, userID); err != nil {
				return "", c, err
			}
			return "", c, ErrReauthRequired
		}
		return "", c, fmt.Errorf("token refresh: status %d (%s)", resp.StatusCode, tr.Error)
	}
	if tr.AccessToken == "" {
		return "", c, errors.New("token refresh: empty access token")
	}
	return tr.AccessToken, c, nil
}

func (s *Service) do(ctx context.Context, token, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.APIBase+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return resp.StatusCode, raw, nil
}

// apiErr builds an error from a failed API response. It includes Google's
// machine-readable reason (e.g. accessNotConfigured) but never the message,
// which could echo event content.
func apiErr(op string, status int, raw []byte) error {
	var e struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && len(e.Error.Errors) > 0 && e.Error.Errors[0].Reason != "" {
		return fmt.Errorf("%s: status %d (%s)", op, status, e.Error.Errors[0].Reason)
	}
	return fmt.Errorf("%s: status %d", op, status)
}

// calendarID returns the dedicated calendar, creating it on first use.
func (s *Service) calendarID(ctx context.Context, userID int64) (string, error) {
	token, c, err := s.accessToken(ctx, userID)
	if err != nil {
		return "", err
	}
	if c.CalendarID != "" {
		return c.CalendarID, nil
	}
	status, raw, err := s.do(ctx, token, http.MethodPost, "/calendars",
		map[string]string{"summary": CalendarName, "timeZone": plan.TimeZoneName})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", apiErr("create calendar", status, raw)
	}
	var cal struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &cal); err != nil || cal.ID == "" {
		return "", errors.New("create calendar: bad response")
	}
	if err := s.Store.SetCalendarID(ctx, userID, cal.ID); err != nil {
		return "", err
	}
	return cal.ID, nil
}

// InsertEvent writes the event to the dedicated calendar and returns its Google ID.
func (s *Service) InsertEvent(ctx context.Context, userID int64, ev plan.Event) (string, error) {
	calID, err := s.calendarID(ctx, userID)
	if err != nil {
		return "", err
	}
	token, _, err := s.accessToken(ctx, userID)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"summary": ev.Title,
		"start":   map[string]string{"dateTime": ev.Start.In(plan.Taipei).Format(time.RFC3339), "timeZone": plan.TimeZoneName},
		"end":     map[string]string{"dateTime": ev.End().In(plan.Taipei).Format(time.RFC3339), "timeZone": plan.TimeZoneName},
	}
	if ev.Location != "" {
		body["location"] = ev.Location
	}
	if ev.Notes != "" {
		body["description"] = ev.Notes
	}
	status, raw, err := s.do(ctx, token, http.MethodPost, "/calendars/"+url.PathEscape(calID)+"/events", body)
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized {
		return "", ErrReauthRequired
	}
	if status != http.StatusOK {
		return "", apiErr("insert event", status, raw)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return "", errors.New("insert event: bad response")
	}
	return out.ID, nil
}

// DeleteEvent removes the event; an event that is already gone counts as success.
func (s *Service) DeleteEvent(ctx context.Context, userID int64, eventID string) error {
	calID, err := s.calendarID(ctx, userID)
	if err != nil {
		return err
	}
	token, _, err := s.accessToken(ctx, userID)
	if err != nil {
		return err
	}
	status, draw, err := s.do(ctx, token, http.MethodDelete,
		"/calendars/"+url.PathEscape(calID)+"/events/"+url.PathEscape(eventID), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound, http.StatusGone:
		return nil
	case http.StatusUnauthorized:
		return ErrReauthRequired
	}
	return apiErr("delete event", status, draw)
}
