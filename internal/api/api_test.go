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
	list    []Contact
	codes   [][]byte
	deleted []int64
	updated map[int64]string
}

func (f *fakeContacts) List(context.Context, int64) ([]Contact, error) { return f.list, nil }
func (f *fakeContacts) EnsureSelf(context.Context, int64) (Contact, error) {
	return Contact{ID: 7, Name: "我自己", Status: "pending", IsSelf: true, Enabled: true}, nil
}
func (f *fakeContacts) Get(_ context.Context, _ int64, id int64) (Contact, error) {
	if id == 7 {
		return Contact{ID: 7, Name: "我自己", IsSelf: true, Enabled: true}, nil
	}
	for _, c := range f.list {
		if c.ID == id {
			return c, nil
		}
	}
	return Contact{}, ErrNotFound
}
func (f *fakeContacts) Create(_ context.Context, _ int64, name string) (Contact, error) {
	c := Contact{ID: int64(100 + len(f.list)), Name: name, Status: "pending", Enabled: true}
	f.list = append(f.list, c)
	return c, nil
}
func (f *fakeContacts) Update(_ context.Context, _ int64, id int64, name *string, _ *bool) error {
	if _, err := f.Get(context.Background(), 1, id); err != nil {
		return err
	}
	if f.updated == nil {
		f.updated = map[int64]string{}
	}
	if name != nil {
		f.updated[id] = *name
	}
	return nil
}
func (f *fakeContacts) Delete(_ context.Context, _ int64, id int64) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeContacts) CreateBindingCode(_ context.Context, _ int64, h []byte, exp time.Time) error {
	f.codes = append(f.codes, h)
	return nil
}

type fakeGroups struct {
	saved []Group
}

func (f *fakeGroups) List(context.Context, int64) ([]Group, error) { return f.saved, nil }
func (f *fakeGroups) Save(_ context.Context, _ int64, id int64, name string, m []int64) (Group, error) {
	g := Group{ID: id, Name: name, MemberIDs: m}
	if id == 0 {
		g.ID = int64(len(f.saved) + 1)
	}
	f.saved = append(f.saved, g)
	return g, nil
}
func (f *fakeGroups) Delete(context.Context, int64, int64) error { return nil }

type fakeUsage struct{ n int }

func (f fakeUsage) Month(context.Context, string) (int, error) { return f.n, nil }

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
	return newServerFull(p, cal, ev, authed, c, r, tg, &fakeGroups{}, fakeUsage{})
}

func newServerFull(p fakeParser, cal *fakeCal, ev *memEvents, authed bool, c Contacts, r Reminders, tg TipsGenerator, g Groups, u Usage) *http.ServeMux {
	s := &Server{Groups: g, Usage: u, MonthlyLimit: 200, AddFriendURL: "https://line.me/R/ti/p/@test", Contacts: c, Reminders: r, Tips: tg, Parser: p, Transcriber: fakeTr{}, Calendar: cal, Events: ev,
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

func TestCreateRenameDeleteContact(t *testing.T) {
	c := &fakeContacts{}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, c, nil, nil)
	rec := do(mux, "POST", "/api/contacts", "application/json", map[string]string{"name": " 媽媽 "})
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"name":"媽媽"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(mux, "POST", "/api/contacts", "application/json", map[string]string{"name": "  "}); rec.Code != 400 {
		t.Fatalf("blank name: %d", rec.Code)
	}
	if rec := do(mux, "POST", "/api/contacts", "application/json", map[string]string{"name": strings.Repeat("字", 31)}); rec.Code != 400 {
		t.Fatalf("long name: %d", rec.Code)
	}
	if rec := do(mux, "PATCH", "/api/contacts/100", "application/json", map[string]any{"name": "老媽", "enabled": false}); rec.Code != 204 || c.updated[100] != "老媽" {
		t.Fatalf("%d %v", rec.Code, c.updated)
	}
	if rec := do(mux, "PATCH", "/api/contacts/999", "application/json", map[string]any{"name": "x"}); rec.Code != 404 {
		t.Fatalf("someone else's contact: %d", rec.Code)
	}
	if rec := do(mux, "DELETE", "/api/contacts/100", "", nil); rec.Code != 204 || len(c.deleted) != 1 {
		t.Fatalf("%d %v", rec.Code, c.deleted)
	}
}

func TestCannotDeleteSelfOrForeignContact(t *testing.T) {
	c := &fakeContacts{}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, c, nil, nil)
	if rec := do(mux, "DELETE", "/api/contacts/7", "", nil); rec.Code != 409 || len(c.deleted) != 0 {
		t.Fatalf("self: %d", rec.Code)
	}
	if rec := do(mux, "DELETE", "/api/contacts/999", "", nil); rec.Code != 404 || len(c.deleted) != 0 {
		t.Fatalf("foreign: %d", rec.Code)
	}
}

func TestContactLimit(t *testing.T) {
	c := &fakeContacts{list: make([]Contact, maxContacts)}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, c, nil, nil)
	if rec := do(mux, "POST", "/api/contacts", "application/json", map[string]string{"name": "x"}); rec.Code != 409 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestInviteContainsLinkAndCodeAndStoresOnlyHash(t *testing.T) {
	c := &fakeContacts{list: []Contact{{ID: 100, Name: "媽媽", Enabled: true}}}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, c, nil, nil)
	rec := do(mux, "POST", "/api/contacts/100/invite", "", nil)
	var out struct {
		Code, Text string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || len(out.Code) != 8 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(out.Text, "https://line.me/R/ti/p/@test") || !strings.Contains(out.Text, out.Code) {
		t.Fatalf("invite text incomplete: %s", out.Text)
	}
	if len(c.codes) != 1 || bytes.Contains(c.codes[0], []byte(out.Code)) {
		t.Fatal("plain code must not be stored")
	}
	if rec := do(mux, "POST", "/api/contacts/999/invite", "", nil); rec.Code != 404 {
		t.Fatalf("foreign contact: %d", rec.Code)
	}
}

func TestInviteTextWithoutLinkStillUsable(t *testing.T) {
	got := InviteText("", "ABCDEFGH", 10)
	if !strings.Contains(got, "ABCDEFGH") || strings.Contains(got, "http") {
		t.Fatalf("%s", got)
	}
}

func TestGroups(t *testing.T) {
	g := &fakeGroups{}
	mux := newServerFull(fakeParser{}, &fakeCal{}, newMem(), true, &fakeContacts{}, nil, nil, g, fakeUsage{})
	rec := do(mux, "POST", "/api/groups", "application/json", map[string]any{"name": "家人", "member_ids": []int64{7, 100}})
	if rec.Code != 200 || len(g.saved) != 1 || len(g.saved[0].MemberIDs) != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(mux, "PUT", "/api/groups/1", "application/json", map[string]any{"name": "全家", "member_ids": []int64{7}}); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := do(mux, "POST", "/api/groups", "application/json", map[string]any{"name": " ", "member_ids": []int64{}}); rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := do(mux, "DELETE", "/api/groups/1", "", nil); rec.Code != 204 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestUsageLevels(t *testing.T) {
	for n, want := range map[int]string{10: "ok", 165: "warn", 195: "critical", 200: "full"} {
		mux := newServerFull(fakeParser{}, &fakeCal{}, newMem(), true, &fakeContacts{}, nil, nil, &fakeGroups{}, fakeUsage{n: n})
		rec := do(mux, "GET", "/api/line/usage", "", nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"level":"`+want+`"`) || !strings.Contains(rec.Body.String(), `"month":"2026-09"`) {
			t.Errorf("sent=%d: %d %s", n, rec.Code, rec.Body)
		}
	}
}

func TestConfirmReportsHowManyMessagesItWillUse(t *testing.T) {
	rem := &fakeReminders{res: ScheduleResult{Status: "scheduled", Recipients: 3}}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, &fakeContacts{}, rem, fakeTips{})
	rec := do(mux, "POST", "/api/plans/confirm", "application/json", withReminder("req-12345678", 60, 7, 100, 101))
	if !strings.Contains(rec.Body.String(), `"reminder_recipients":3`) {
		t.Fatalf("%s", rec.Body)
	}
}
