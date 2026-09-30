package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	Healthz(fakeDB{}).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("want 200 got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	Healthz(fakeDB{errors.New("down")}).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 503 {
		t.Fatalf("want 503 got %d", rec.Code)
	}
}

func TestSPAFallbackAndTraversal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("INDEX"), 0o644)
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "assets", "a.js"), []byte("JS"), 0o644)
	os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.txt"), []byte("SECRET"), 0o644)
	h := SPA(dir)

	get := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		return rec
	}
	if b := get("/some/client/route").Body.String(); b != "INDEX" {
		t.Fatalf("fallback body=%q", b)
	}
	r := get("/assets/a.js")
	if r.Body.String() != "JS" || r.Header().Get("Cache-Control") == "" {
		t.Fatalf("asset not served with cache header: %q", r.Body.String())
	}
	if b := get("/../secret.txt").Body.String(); b == "SECRET" {
		t.Fatal("path traversal leaked a file outside the static dir")
	}
}

func TestAPINotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	APINotFound(rec, httptest.NewRequest("GET", "/api/x", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("bad: %d %v", rec.Code, rec.Header())
	}
}
