// Package version exposes which build is running, so a deploy can be verified
// from the app itself. Version and BuiltAt are stamped in at build time
// (-ldflags -X); the commit comes from Railway at run time.
package version

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

var (
	Version = "dev"
	BuiltAt = "" // UTC, RFC 3339
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"built_at"`
}

// Get reads the commit from the environment each call so tests can set it.
func Get() Info {
	return Info{Version: Version, Commit: shortSHA(os.Getenv("RAILWAY_GIT_COMMIT_SHA")), BuiltAt: BuiltAt}
}

func shortSHA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// Handler serves the version as JSON. It is public on purpose (nothing secret
// in it) so the login screen can show it too. Never cached, or it would lie.
func Handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(Get())
}
