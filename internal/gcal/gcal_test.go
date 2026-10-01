package gcal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voiceplan/internal/crypto"
	"voiceplan/internal/plan"
)

type memStore struct {
	c     *Creds
	saved int
}

func (m *memStore) Get(context.Context, int64) (Creds, error) {
	if m.c == nil {
		return Creds{}, ErrNoCredentials
	}
	return *m.c, nil
}
func (m *memStore) Save(_ context.Context, _ int64, enc []byte) error {
	m.saved++
	if m.c == nil {
		m.c = &Creds{}
	}
	m.c.RefreshTokenEnc, m.c.NeedsReauth = enc, false
	return nil
}
func (m *memStore) SetCalendarID(_ context.Context, _ int64, id string) error {
	m.c.CalendarID = id
	return nil
}
func (m *memStore) MarkReauth(context.Context, int64) error { m.c.NeedsReauth = true; return nil }

var key = []byte(strings.Repeat("k", 32))

type fake struct {
	tokenStatus int
	tokenBody   string
	calls       []string
	eventBody   map[string]any
	deleteCode  int
}

func setup(t *testing.T, f *fake) (*Service, *memStore) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/token":
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), "refresh_token=RT") {
				t.Errorf("refresh token not sent/decrypted: %s", b)
			}
			w.WriteHeader(f.tokenStatus)
			_, _ = w.Write([]byte(f.tokenBody))
		case r.Method == "POST" && r.URL.Path == "/cal/calendars":
			_, _ = w.Write([]byte(`{"id":"cal-1"}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/events"):
			if r.Header.Get("Authorization") != "Bearer AT" {
				t.Error("missing bearer token")
			}
			_ = json.NewDecoder(r.Body).Decode(&f.eventBody)
			_, _ = w.Write([]byte(`{"id":"evt-1"}`))
		case r.Method == "DELETE":
			w.WriteHeader(f.deleteCode)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	enc, _ := crypto.Encrypt(key, []byte("RT"))
	st := &memStore{c: &Creds{RefreshTokenEnc: enc}}
	s := NewService(st, key, "cid", "sec")
	s.APIBase, s.TokenURL = srv.URL+"/cal", srv.URL+"/token"
	return s, st
}

func TestInsertCreatesCalendarOnceAndWritesEvent(t *testing.T) {
	f := &fake{tokenStatus: 200, tokenBody: `{"access_token":"AT"}`}
	s, st := setup(t, f)
	ev := plan.Event{Title: "看牙醫", Start: time.Date(2026, 10, 7, 14, 0, 0, 0, plan.Taipei), DurationMinutes: 90, Location: "診所"}
	id, err := s.InsertEvent(context.Background(), 1, ev)
	if err != nil || id != "evt-1" {
		t.Fatalf("%q %v", id, err)
	}
	if st.c.CalendarID != "cal-1" {
		t.Fatal("dedicated calendar id not stored")
	}
	start := f.eventBody["start"].(map[string]any)
	end := f.eventBody["end"].(map[string]any)
	if start["dateTime"] != "2026-10-07T14:00:00+08:00" || end["dateTime"] != "2026-10-07T15:30:00+08:00" || start["timeZone"] != "Asia/Taipei" {
		t.Fatalf("bad times: %v %v", start, end)
	}
	if f.eventBody["summary"] != "看牙醫" || f.eventBody["location"] != "診所" {
		t.Fatalf("bad body: %v", f.eventBody)
	}
	// Second insert must reuse the stored calendar (no second POST /calendars).
	before := 0
	for _, c := range f.calls {
		if c == "POST /cal/calendars" {
			before++
		}
	}
	if _, err := s.InsertEvent(context.Background(), 1, ev); err != nil {
		t.Fatal(err)
	}
	after := 0
	for _, c := range f.calls {
		if c == "POST /cal/calendars" {
			after++
		}
	}
	if before != 1 || after != 1 {
		t.Fatalf("calendar created %d/%d times", before, after)
	}
}

func TestInvalidGrantFlagsReauthAndBlocksFurtherCalls(t *testing.T) {
	f := &fake{tokenStatus: 400, tokenBody: `{"error":"invalid_grant"}`}
	s, st := setup(t, f)
	_, err := s.InsertEvent(context.Background(), 1, plan.Event{Title: "x", Start: time.Now(), DurationMinutes: 30})
	if !errors.Is(err, ErrReauthRequired) || !st.c.NeedsReauth {
		t.Fatalf("expected reauth flag, err=%v flag=%v", err, st.c.NeedsReauth)
	}
	n := len(f.calls)
	if _, err := s.InsertEvent(context.Background(), 1, plan.Event{Title: "x", Start: time.Now(), DurationMinutes: 30}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("%v", err)
	}
	if len(f.calls) != n {
		t.Fatal("sync must be paused: no network calls while reauth is pending")
	}
	conn, needs, _ := s.Status(context.Background(), 1)
	if !conn || !needs {
		t.Fatal("status should report connected + needs reauth")
	}
}

func TestNoCredentials(t *testing.T) {
	f := &fake{}
	s, st := setup(t, f)
	st.c = nil
	if _, err := s.InsertEvent(context.Background(), 1, plan.Event{}); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("%v", err)
	}
	if c, _, _ := s.Status(context.Background(), 1); c {
		t.Fatal("should be disconnected")
	}
}

func TestDeleteTreatsMissingEventAsSuccess(t *testing.T) {
	for _, code := range []int{204, 404, 410} {
		f := &fake{tokenStatus: 200, tokenBody: `{"access_token":"AT"}`, deleteCode: code}
		s, st := setup(t, f)
		st.c.CalendarID = "cal-1"
		if err := s.DeleteEvent(context.Background(), 1, "evt-1"); err != nil {
			t.Errorf("code %d: %v", code, err)
		}
	}
	f := &fake{tokenStatus: 200, tokenBody: `{"access_token":"AT"}`, deleteCode: 500}
	s, st := setup(t, f)
	st.c.CalendarID = "cal-1"
	if err := s.DeleteEvent(context.Background(), 1, "evt-1"); err == nil {
		t.Fatal("500 must surface as an error")
	}
}

func TestConnectEncryptsToken(t *testing.T) {
	f := &fake{tokenStatus: 200, tokenBody: `{"access_token":"AT"}`}
	s, st := setup(t, f)
	st.c = nil
	if err := s.Connect(context.Background(), 1, "RT"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(st.c.RefreshTokenEnc), "RT") {
		t.Fatal("refresh token stored in plaintext")
	}
	if pt, err := crypto.Decrypt(key, st.c.RefreshTokenEnc); err != nil || string(pt) != "RT" {
		t.Fatalf("round trip: %v", err)
	}
	if st.c.CalendarID != "cal-1" {
		t.Fatal("calendar should be created on connect")
	}
}

func TestAPIErrorIncludesReasonButNotMessage(t *testing.T) {
	err := apiErr("create calendar", 403, []byte(`{"error":{"errors":[{"reason":"insufficientPermissions","message":"secret event title"}]}}`))
	if err.Error() != "create calendar: status 403 (insufficientPermissions)" {
		t.Fatalf("%v", err)
	}
	if got := apiErr("x", 500, []byte("not json")).Error(); got != "x: status 500" {
		t.Fatalf("%s", got)
	}
}

func listServer(t *testing.T, handler func(q map[string][]string) (int, string)) (*Service, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_, _ = w.Write([]byte(`{"access_token":"AT"}`))
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/events"):
			queries = append(queries, r.URL.RawQuery)
			code, body := handler(r.URL.Query())
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	enc, _ := crypto.Encrypt(key, []byte("RT"))
	st := &memStore{c: &Creds{RefreshTokenEnc: enc, CalendarID: "cal-1"}}
	s := NewService(st, key, "cid", "sec")
	s.APIBase, s.TokenURL = srv.URL+"/cal", srv.URL+"/token"
	return s, &queries
}

var syncNow = time.Date(2026, 10, 1, 10, 0, 0, 0, plan.Taipei)

func TestFullSyncUsesTimeMinAndExpandsRecurrence(t *testing.T) {
	s, qs := listServer(t, func(map[string][]string) (int, string) {
		return 200, `{"items":[
		  {"id":"a","status":"confirmed","summary":"看牙醫","location":"診所","description":"帶健保卡",
		   "start":{"dateTime":"2026-10-07T14:00:00+08:00"},"end":{"dateTime":"2026-10-07T15:30:00+08:00"}},
		  {"id":"b","status":"cancelled"},
		  {"id":"c","summary":"全天","start":{"date":"2026-10-08"},"end":{"date":"2026-10-09"}}],
		 "nextSyncToken":"TOK1"}`
	})
	evs, tok, err := s.ListChanges(context.Background(), 1, "", syncNow)
	if err != nil || tok != "TOK1" || len(evs) != 3 {
		t.Fatalf("%v %q %d", err, tok, len(evs))
	}
	q := (*qs)[0]
	for _, want := range []string{"singleEvents=true", "showDeleted=true", "timeMin=2026-09-30T02%3A00%3A00Z"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %s", q, want)
		}
	}
	if strings.Contains(q, "syncToken") {
		t.Error("a full sync must not send a syncToken")
	}
	a := evs[0]
	if a.Title != "看牙醫" || a.Location != "診所" || a.Notes != "帶健保卡" || a.End.Sub(a.Start) != 90*time.Minute {
		t.Fatalf("%+v", a)
	}
	if !evs[1].Cancelled || !evs[2].AllDay {
		t.Fatalf("cancelled/all-day not detected: %+v %+v", evs[1], evs[2])
	}
}

func TestIncrementalSyncSendsTokenAndNotTimeMin(t *testing.T) {
	s, qs := listServer(t, func(map[string][]string) (int, string) { return 200, `{"items":[],"nextSyncToken":"TOK2"}` })
	_, tok, err := s.ListChanges(context.Background(), 1, "TOK1", syncNow)
	if err != nil || tok != "TOK2" {
		t.Fatalf("%v %q", err, tok)
	}
	q := (*qs)[0]
	if !strings.Contains(q, "syncToken=TOK1") || strings.Contains(q, "timeMin") {
		t.Fatalf("%s", q)
	}
}

func TestSyncFollowsPages(t *testing.T) {
	s, qs := listServer(t, func(q map[string][]string) (int, string) {
		if len(q["pageToken"]) == 0 {
			return 200, `{"items":[{"id":"a","summary":"x","start":{"dateTime":"2026-10-07T14:00:00+08:00"},"end":{"dateTime":"2026-10-07T15:00:00+08:00"}}],"nextPageToken":"P2"}`
		}
		return 200, `{"items":[{"id":"b","summary":"y","start":{"dateTime":"2026-10-08T14:00:00+08:00"},"end":{"dateTime":"2026-10-08T15:00:00+08:00"}}],"nextSyncToken":"T"}`
	})
	evs, tok, err := s.ListChanges(context.Background(), 1, "", syncNow)
	if err != nil || len(evs) != 2 || tok != "T" || len(*qs) != 2 {
		t.Fatalf("%v %d %q %d", err, len(evs), tok, len(*qs))
	}
}

func TestSyncErrorMapping(t *testing.T) {
	for code, want := range map[int]error{410: ErrSyncTokenInvalid, 401: ErrReauthRequired} {
		s, _ := listServer(t, func(map[string][]string) (int, string) { return code, `{}` })
		if _, _, err := s.ListChanges(context.Background(), 1, "OLD", syncNow); !errors.Is(err, want) {
			t.Errorf("%d: %v", code, err)
		}
	}
	s, _ := listServer(t, func(map[string][]string) (int, string) {
		return 403, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`
	})
	if _, _, err := s.ListChanges(context.Background(), 1, "", syncNow); err == nil || !strings.Contains(err.Error(), "rateLimitExceeded") {
		t.Fatalf("%v", err)
	}
	s, _ = listServer(t, func(map[string][]string) (int, string) { return 200, `{"items":[]}` })
	if _, _, err := s.ListChanges(context.Background(), 1, "", syncNow); err == nil {
		t.Fatal("a response without a sync token must be an error, never a silent success")
	}
}

func TestSyncWithoutCalendarMeansNotConnected(t *testing.T) {
	s, _ := listServer(t, func(map[string][]string) (int, string) { return 200, `{}` })
	s.Store.(*memStore).c.CalendarID = ""
	if _, _, err := s.ListChanges(context.Background(), 1, "", syncNow); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("%v", err)
	}
}
