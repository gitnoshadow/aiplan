package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://x", "GOOGLE_CLIENT_ID": "id", "GOOGLE_CLIENT_SECRET": "s",
		"OAUTH_REDIRECT_URL": "https://a/auth/callback", "ALLOWED_EMAIL": "me@example.com",
		"SESSION_SECRET": strings.Repeat("a", 32), "APP_BASE_URL": "https://a/",
		"GEMINI_API_KEY": "gk", "LLM_MODEL": "some-model",
		"ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}
}

func TestLoadOK(t *testing.T) {
	c, err := load(env(base()))
	if err != nil || c.Port != "8080" || c.AppBaseURL != "https://a" || !c.SecureCookies() {
		t.Fatalf("unexpected: %+v %v", c, err)
	}
}

func TestLoadMissingAndWeakSecret(t *testing.T) {
	m := base()
	delete(m, "DATABASE_URL")
	if _, err := load(env(m)); err == nil {
		t.Fatal("expected missing error")
	}
	m = base()
	m["SESSION_SECRET"] = "short"
	if _, err := load(env(m)); err == nil {
		t.Fatal("expected weak secret error")
	}
}

func TestEncryptionKeyRequiredAndValidated(t *testing.T) {
	m := base()
	m["ENCRYPTION_KEY"] = "not-base64!"
	if _, err := load(env(m)); err == nil {
		t.Fatal("expected bad key error")
	}
	m["ENCRYPTION_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := load(env(m)); err == nil {
		t.Fatal("expected wrong-length error")
	}
	delete(m, "ENCRYPTION_KEY")
	if _, err := load(env(m)); err == nil {
		t.Fatal("expected missing error")
	}
}

func TestLLMSettingsRequired(t *testing.T) {
	for _, k := range []string{"GEMINI_API_KEY", "LLM_MODEL"} {
		m := base()
		delete(m, k)
		if _, err := load(env(m)); err == nil {
			t.Errorf("%s should be required", k)
		}
	}
}
