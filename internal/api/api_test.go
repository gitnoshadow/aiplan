package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voiceplan/internal/gcal"
	"voiceplan/internal/plan"
)

var fixedNow = time.Date(2026, 9, 30, 10, 0, 0, 0, plan.Taipei)

type fakeParser struct {
	raw plan.Raw
	err error
}

func (f fakeParser) ParsePlan(context.Context, string, time.Time) (plan.Raw, error) {
	return f.raw, f.err
}

type fakeTr struct{}

func (fakeTr) Transcribe(_ context.Context, a []byte, mime string) (string, error) {
	return "明天開會", nil
}

type fakeCal struct {
	inserts, deletes int
	insertErr        error
}

func (f *fakeCal) InsertEvent(context.Context, int64, plan.Event) (string, error) {
	if f.insertErr != nil {
		return "", f.insertErr
	}
	f.inserts++
	return "g-1", nil
}
func (f *fakeCal) DeleteEvent(context.Context, int64, string) error  { f.deletes++; return nil }
func (f *fakeCal) Status(context.Context, int64) (bool, bool, error) { return true, false, nil }

type memEvents struct {
	rows   map[int64]*EventRow
	byReq  map[string]int64
	nextID int64
}

func newMem() *memEvents { return &memEvents{rows: map[int64]*EventRow{}, byReq: map[string]int64{}} }
func (m *memEvents) Reserve(_ context.Context, _ int64, req string, ev plan.Event) (EventRow, bool, error) {
	if id, ok := m.byReq[req]; ok {
		return *m.rows[id], false, nil
	}
	m.nextID++
	r := &EventRow{ID: m.nextID, Title: ev.Title, Status: "pending"}
	m.rows[r.ID], m.byReq[req] = r, r.ID
	return *r, true, nil
}
func (m *memEvents) Complete(_ context.Context, id int64, g string) error {
	m.rows[id].Status, m.rows[id].GoogleEventID = "active", g
	return nil
}
func (m *memEvents) Abandon(_ context.Context, id int64) error {
	for k, v := range m.byReq {
		if v == id {
			delete(m.byReq, k)
		}
	}
	delete(m.rows, id)
	return nil
}
func (m *memEvents) List(context.Context, int64, time.Time, int) ([]EventRow, error) {
	return nil, nil
}
func (m *memEvents) Get(_ context.Context, _ int64, id int64) (EventRow, error) {
	if r, ok := m.rows[id]; ok {
		return *r, nil
	}
	return EventRow{}, errors.New("nf")
}
func (m *memEvents) Delete(_ context.Context, _ int64, id int64) error {
	delete(m.rows, id)
	return nil
}

type fakeContacts struct {
	list  []Contact
	codes [][]byte
}

func (f *fakeContacts) List(context.Context, int64) ([]Contact, error) { return f.list, nil }
func (f *fakeContacts) EnsureSelf(context.Context, int64) (Contact, error) {
	return Contact{ID: 7, Name: "我自己", Status: "pending", IsSelf: true}, nil
}
func (f *fakeContacts) CreateBindingCode(_ context.Context, _ int64, h []byte, exp time.Time) error {
	f.codes = append(f.codes, h)
	return nil
}

type fakeReminders struct {
	reqs []ScheduleReq
	res  ScheduleResult
	err  error
}

func (f *fakeReminders) Schedule(_ context.Context, q ScheduleReq) (ScheduleResult, error) {
	f.reqs = append(f.reqs, q)
	return f.res, f.err
}

type fakeTips struct {
	out string
	err error
}

func (f fakeTips) Tips(context.Context, plan.Event, time.Time) (string, error) { return f.out, f.err }

func newServer(p fakeParser, cal *fakeCal, ev *memEvents, authed bool) *http.ServeMux {
	return newServerR(p, cal, ev, authed, &fakeContacts{}, &fakeReminders{res: ScheduleResult{Status: "scheduled", Recipients: 1}}, fakeTips{out: "・帶健保卡"})
}

func newServerR(p fakeParser, cal *fakeCal, ev *memEvents, authed bool, c Contacts, r Reminders, tg TipsGenerator) *http.ServeMux {
	s := &Server{Contacts: c, Reminders: r, Tips: tg, Parser: p, Transcriber: fakeTr{}, Calendar: cal, Events: ev,
		UserID: func(context.Context) (int64, bool) { return 1, authed },
		Now:    func() time.Time { return fixedNow }}
	mux := http.NewServeMux()
	s.Register(mux, func(h http.Handler) http.Handler { return h })
	return mux
}

func do(mux http.Handler, method, path, ctype string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	switch b := body.(type) {
	case []byte:
		buf.Write(b)
	case nil:
	default:
		_ = json.NewEncoder(&buf).Encode(b)
	}
	req := httptest.NewRequest(method, path, &buf)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestParseDoesNotTouchCalendar(t *testing.T) {
	cal := &fakeCal{}
	start := "2026-10-07T14:00:00+08:00"
	mux := newServer(fakeParser{raw: plan.Raw{Title: "看牙醫", StartTime: &start, Category: "medical"}}, cal, newMem(), true)
	rec := do(mux, "POST", "/api/plans/parse", "application/json", map[string]string{"text": "下週三下午兩點看牙醫"})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var res plan.Result
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.DurationMinutes != 90 || !res.DurationIsDefault || res.StartTime != start {
		t.Fatalf("%+v", res)
	}
	if cal.inserts != 0 {
		t.Fatal("parsing must never write to the calendar")
	}
}

func TestParseValidationAndFailure(t *testing.T) {
	mux := newServer(fakeParser{err: errors.New("boom")}, &fakeCal{}, newMem(), true)
	if rec := do(mux, "POST", "/api/plans/parse", "application/json", map[string]string{"text": "  "}); rec.Code != 400 {
		t.Fatalf("empty text: %d", rec.Code)
	}
	if rec := do(mux, "POST", "/api/plans/parse", "application/json", map[string]string{"text": strings.Repeat("字", 1001)}); rec.Code != 400 {
		t.Fatalf("long text: %d", rec.Code)
	}
	if rec := do(mux, "POST", "/api/plans/parse", "application/json", map[string]string{"text": "hi"}); rec.Code != 502 {
		t.Fatalf("llm failure: %d", rec.Code)
	}
}

type busyErr struct{}

func (busyErr) Error() string { return "busy" }
func (busyErr) Busy() bool    { return true }

func TestParseReportsBusyDistinctly(t *testing.T) {
	mux := newServer(fakeParser{err: busyErr{}}, &fakeCal{}, newMem(), true)
	rec := do(mux, "POST", "/api/plans/parse", "application/json", map[string]string{"text": "hi"})
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "llm_busy") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func validConfirm(req string) map[string]any {
	return map[string]any{"request_id": req, "title": "看牙醫", "start_time": "2026-10-07T14:00:00+08:00",
		"duration_minutes": 90, "category": "medical"}
}

func TestConfirmWritesOnceAndIsIdempotent(t *testing.T) {
	cal, ev := &fakeCal{}, newMem()
	mux := newServer(fakeParser{}, cal, ev, true)
	if rec := do(mux, "POST", "/api/plans/confirm", "application/json", validConfirm("req-12345678")); rec.Code != 201 {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	rec := do(mux, "POST", "/api/plans/confirm", "application/json", validConfirm("req-12345678"))
	if rec.Code != 200 || cal.inserts != 1 {
		t.Fatalf("retry must not create a second Google event: code=%d inserts=%d", rec.Code, cal.inserts)
	}
}

func TestConfirmRejectsInvalidBeforeAnyWrite(t *testing.T) {
	cal, ev := &fakeCal{}, newMem()
	mux := newServer(fakeParser{}, cal, ev, true)
	bad := validConfirm("req-12345678")
	bad["start_time"] = ""
	if rec := do(mux, "POST", "/api/plans/confirm", "application/json", bad); rec.Code != 400 {
		t.Fatalf("missing start: %d", rec.Code)
	}
	if rec := do(mux, "POST", "/api/plans/confirm", "application/json", validConfirm("x")); rec.Code != 400 {
		t.Fatalf("short request id: %d", rec.Code)
	}
	if cal.inserts != 0 || len(ev.rows) != 0 {
		t.Fatal("nothing may be written for invalid input")
	}
}

func TestConfirmReauthAndNotConnectedRollBack(t *testing.T) {
	for err, want := range map[error]string{gcal.ErrReauthRequired: "reauth_required", gcal.ErrNoCredentials: "calendar_not_connected"} {
		cal, ev := &fakeCal{insertErr: err}, newMem()
		mux := newServer(fakeParser{}, cal, ev, true)
		rec := do(mux, "POST", "/api/plans/confirm", "application/json", validConfirm("req-12345678"))
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%v: %d %s", err, rec.Code, rec.Body)
		}
		if len(ev.rows) != 0 {
			t.Fatal("pending row must be removed so the user can retry after fixing auth")
		}
	}
}

func TestDeleteRemovesGoogleThenRow(t *testing.T) {
	cal, ev := &fakeCal{}, newMem()
	mux := newServer(fakeParser{}, cal, ev, true)
	do(mux, "POST", "/api/plans/confirm", "application/json", validConfirm("req-12345678"))
	if rec := do(mux, "DELETE", "/api/events/1", "", nil); rec.Code != 204 {
		t.Fatalf("%d", rec.Code)
	}
	if cal.deletes != 1 || len(ev.rows) != 0 {
		t.Fatal("expected Google delete + row delete")
	}
	if rec := do(mux, "DELETE", "/api/events/99", "", nil); rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestTranscribeChecks(t *testing.T) {
	mux := newServer(fakeParser{}, &fakeCal{}, newMem(), true)
	if rec := do(mux, "POST", "/api/transcribe", "audio/webm", make([]byte, 5000)); rec.Code != 415 {
		t.Fatalf("wrong type: %d", rec.Code)
	}
	if rec := do(mux, "POST", "/api/transcribe", "audio/wav", make([]byte, 10)); rec.Code != 400 {
		t.Fatalf("too short: %d", rec.Code)
	}
	if rec := do(mux, "POST", "/api/transcribe", "audio/wav", make([]byte, 5<<20)); rec.Code != 413 {
		t.Fatalf("too large: %d", rec.Code)
	}
	rec := do(mux, "POST", "/api/transcribe", "audio/wav", make([]byte, 5000))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "明天開會") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestUnauthenticatedUserRejected(t *testing.T) {
	mux := newServer(fakeParser{}, &fakeCal{}, newMem(), false)
	if rec := do(mux, "POST", "/api/plans/confirm", "application/json", validConfirm("req-12345678")); rec.Code != 401 {
		t.Fatalf("%d", rec.Code)
	}
}

func withReminder(req string, lead int, ids ...int64) map[string]any {
	m := validConfirm(req)
	m["reminder"] = map[string]any{"lead_minutes": lead, "contact_ids": ids}
	return m
}

func TestConfirmSchedulesReminderWithTips(t *testing.T) {
	rem := &fakeReminders{res: ScheduleResult{Status: "scheduled", Recipients: 1}}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, &fakeContacts{}, rem, fakeTips{out: "・帶健保卡"})
	rec := do(mux, "POST", "/api/plans/confirm", "application/json", withReminder("req-12345678", 60, 7))
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"reminder":"scheduled"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(rem.reqs) != 1 {
		t.Fatalf("scheduled %d times", len(rem.reqs))
	}
	q := rem.reqs[0]
	if q.LeadMinutes != 60 || q.Tips != "・帶健保卡" || len(q.ContactIDs) != 1 || q.ContactIDs[0] != 7 {
		t.Fatalf("%+v", q)
	}
}

func TestReminderIsOptInAndNeverScheduledByRetry(t *testing.T) {
	rem := &fakeReminders{res: ScheduleResult{Status: "scheduled"}}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, &fakeContacts{}, rem, fakeTips{})
	// No recipients selected (the default): nothing is scheduled.
	do(mux, "POST", "/api/plans/confirm", "application/json", validConfirm("req-aaaaaaaa"))
	do(mux, "POST", "/api/plans/confirm", "application/json", withReminder("req-bbbbbbbb", 60))
	do(mux, "POST", "/api/plans/confirm", "application/json", withReminder("req-cccccccc", 0, 7))
	if len(rem.reqs) != 0 {
		t.Fatalf("reminders must be opt-in: %d scheduled", len(rem.reqs))
	}
	// A double-tap on a request that already succeeded does not schedule again.
	do(mux, "POST", "/api/plans/confirm", "application/json", withReminder("req-dddddddd", 60, 7))
	do(mux, "POST", "/api/plans/confirm", "application/json", withReminder("req-dddddddd", 60, 7))
	if len(rem.reqs) != 1 {
		t.Fatalf("retry rescheduled: %d", len(rem.reqs))
	}
}

func TestTipsFailureStillSchedules(t *testing.T) {
	rem := &fakeReminders{res: ScheduleResult{Status: "scheduled"}}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, &fakeContacts{}, rem, fakeTips{err: errors.New("llm down")})
	rec := do(mux, "POST", "/api/plans/confirm", "application/json", withReminder("req-12345678", 60, 7))
	if rec.Code != 201 || len(rem.reqs) != 1 || rem.reqs[0].Tips != "" {
		t.Fatalf("%d %+v", rec.Code, rem.reqs)
	}
}

func TestReminderFailureDoesNotFailConfirmation(t *testing.T) {
	cal := &fakeCal{}
	rem := &fakeReminders{err: errors.New("db down")}
	mux := newServerR(fakeParser{}, cal, newMem(), true, &fakeContacts{}, rem, fakeTips{})
	rec := do(mux, "POST", "/api/plans/confirm", "application/json", withReminder("req-12345678", 60, 7))
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"reminder":"failed"`) || cal.inserts != 1 {
		t.Fatalf("event is already in Google Calendar, so confirm must still succeed: %d %s", rec.Code, rec.Body)
	}
}

func TestLateReminderIsReported(t *testing.T) {
	rem := &fakeReminders{res: ScheduleResult{Status: "scheduled", Late: true}}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, &fakeContacts{}, rem, fakeTips{})
	rec := do(mux, "POST", "/api/plans/confirm", "application/json", withReminder("req-12345678", 60, 7))
	if !strings.Contains(rec.Body.String(), `"reminder_late":true`) {
		t.Fatalf("%s", rec.Body)
	}
}

func TestBindingEndpointStoresOnlyAHash(t *testing.T) {
	c := &fakeContacts{}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, c, &fakeReminders{}, nil)
	rec := do(mux, "POST", "/api/line/bind", "", nil)
	var out struct {
		Code string `json:"code"`
		Mins int    `json:"expires_in_minutes"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || len(out.Code) != 8 || out.Mins != 10 || len(c.codes) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if bytes.Contains(c.codes[0], []byte(out.Code)) || len(c.codes[0]) != 32 {
		t.Fatal("the plain code must never be stored")
	}
	if rec := do(newServerR(fakeParser{}, &fakeCal{}, newMem(), false, c, nil, nil), "POST", "/api/line/bind", "", nil); rec.Code != 401 {
		t.Fatalf("binding requires login: %d", rec.Code)
	}
}

func TestListContacts(t *testing.T) {
	c := &fakeContacts{list: []Contact{{ID: 7, Name: "我自己", Status: "active", IsSelf: true}}}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, c, nil, nil)
	rec := do(mux, "GET", "/api/contacts", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"active"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
