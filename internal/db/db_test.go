package db

import "testing"

func TestToPgx5URL(t *testing.T) {
	for in, want := range map[string]string{
		"postgresql://u:p@h:5432/d": "pgx5://u:p@h:5432/d",
		"postgres://u:p@h/d":        "pgx5://u:p@h/d",
		"pgx5://already":            "pgx5://already",
	} {
		if got := toPgx5URL(in); got != want {
			t.Errorf("toPgx5URL(%q)=%q want %q", in, got, want)
		}
	}
}
