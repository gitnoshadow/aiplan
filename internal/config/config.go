// Package config loads and validates environment variables.
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port               string
	DatabaseURL        string
	GoogleClientID     string
	GoogleClientSecret string
	OAuthRedirectURL   string
	AllowedEmail       string
	SessionSecret      string
	EncryptionKey      []byte // 32 bytes; encrypts stored Google refresh tokens
	GeminiAPIKey       string
	LLMModel           string
	LLMFallbackModel   string // optional
	LineChannelSecret  string
	LineAccessToken    string
	LineMonthlyLimit   int    // free plan: 200
	LineAddFriendURL   string // optional; used in invitations
	CalendarSyncEvery  time.Duration
	AppBaseURL         string
	StaticDir          string
}

func Load() (*Config, error) { return load(os.Getenv) }

func load(get func(string) string) (*Config, error) {
	c := &Config{
		Port:               orDefault(get("PORT"), "8080"),
		DatabaseURL:        get("DATABASE_URL"),
		GoogleClientID:     get("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: get("GOOGLE_CLIENT_SECRET"),
		OAuthRedirectURL:   get("OAUTH_REDIRECT_URL"),
		AllowedEmail:       strings.TrimSpace(get("ALLOWED_EMAIL")),
		SessionSecret:      get("SESSION_SECRET"),
		AppBaseURL:         strings.TrimRight(get("APP_BASE_URL"), "/"),
		StaticDir:          orDefault(get("STATIC_DIR"), "web/dist"),
		GeminiAPIKey:       get("GEMINI_API_KEY"),
		LLMModel:           strings.TrimSpace(get("LLM_MODEL")),
		LLMFallbackModel:   strings.TrimSpace(get("LLM_FALLBACK_MODEL")),
		LineChannelSecret:  strings.TrimSpace(get("LINE_CHANNEL_SECRET")),
		LineAccessToken:    strings.TrimSpace(get("LINE_CHANNEL_ACCESS_TOKEN")),
	}
	c.LineMonthlyLimit = 200
	if v := strings.TrimSpace(get("LINE_MONTHLY_LIMIT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("LINE_MONTHLY_LIMIT must be a non-negative integer (0 = do not enforce)")
		}
		c.LineMonthlyLimit = n
	}
	c.LineAddFriendURL = strings.TrimSpace(get("LINE_ADD_FRIEND_URL"))
	c.CalendarSyncEvery = 5 * time.Minute
	if v := strings.TrimSpace(get("CALENDAR_SYNC_MINUTES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 60 {
			return nil, fmt.Errorf("CALENDAR_SYNC_MINUTES must be an integer from 1 to 60")
		}
		c.CalendarSyncEvery = time.Duration(n) * time.Minute
	}
	required := map[string]string{
		"DATABASE_URL": c.DatabaseURL, "GOOGLE_CLIENT_ID": c.GoogleClientID,
		"GOOGLE_CLIENT_SECRET": c.GoogleClientSecret, "OAUTH_REDIRECT_URL": c.OAuthRedirectURL,
		"ALLOWED_EMAIL": c.AllowedEmail, "SESSION_SECRET": c.SessionSecret, "APP_BASE_URL": c.AppBaseURL,
		"GEMINI_API_KEY": c.GeminiAPIKey, "LLM_MODEL": c.LLMModel,
		"LINE_CHANNEL_SECRET": c.LineChannelSecret, "LINE_CHANNEL_ACCESS_TOKEN": c.LineAccessToken, "ENCRYPTION_KEY": get("ENCRYPTION_KEY"),
	}
	var missing []string
	for k, v := range required {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if len(c.SessionSecret) < 32 {
		return nil, fmt.Errorf("SESSION_SECRET must be at least 32 characters")
	}
	b, err := base64.StdEncoding.DecodeString(get("ENCRYPTION_KEY"))
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("ENCRYPTION_KEY must be base64 of exactly 32 bytes")
	}
	c.EncryptionKey = b
	return c, nil
}

// SecureCookies is true when the app is served over HTTPS.
func (c *Config) SecureCookies() bool { return strings.HasPrefix(c.AppBaseURL, "https://") }

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
