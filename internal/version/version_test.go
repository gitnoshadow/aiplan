package version

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestShortSHA(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "abc": "abc", " 0123456789abcdef \n": "0123456", "0123456": "0123456",
	} {
		if got := shortSHA(in); got != want {
			t.Errorf("shortSHA(%q)=%q want %q", in, got, want)
		}
	}
}

func TestHandler(t *testing.T) {
	t.Setenv("RAILWAY_GIT_COMMIT_SHA", "deadbeefcafe1234")
	Version, BuiltAt = "9.9.9", "2026-10-01T08:30:00Z"
	rec := httptest.NewRecorder()
	Handler(rec, httptest.NewRequest("GET", "/api/version", nil))
	var got Info
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "9.9.9" || got.Commit != "deadbee" || got.BuiltAt != "2026-10-01T08:30:00Z" {
		t.Fatalf("%+v", got)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("a cached version response would defeat the purpose")
	}
}
