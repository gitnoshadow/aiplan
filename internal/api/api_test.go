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

	"voiceplan/internal/calsync"
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
	updates          int
	updateErr        error
	lastUpdate       plan.Event
}

func (f *fakeCal) UpdateEvent(_ context.Context, _ int64, _ string, ev plan.Event) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates++
	f.lastUpdate = ev
	return nil
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
	list   []EventRow
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
	return m.list, nil
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

type fakeSync struct {
	err   error
	calls int
}

func (f *fakeSync) Status(context.Context, int64) (calsync.Status, error) {
	return calsync.Status{IntervalMinutes: 5}, nil
}
func (f *fakeSync) SyncUser(context.Context, int64) (calsync.Stats, error) {
	f.calls++
	return calsync.Stats{Updated: 2, Cancelled: 1}, f.err
}

var syncFake = &fakeSync{}

func newServerFull(p fakeParser, cal *fakeCal, ev *memEvents, authed bool, c Contacts, r Reminders, tg TipsGenerator, g Groups, u Usage) *http.ServeMux {
	s := &Server{Sync: syncFake, Groups: g, Usage: u, MonthlyLimit: 200, AddFriendURL: "https://line.me/R/ti/p/@test", Contacts: c, Reminders: r, Tips: tg, Parser: p, Transcriber: fakeTr{}, Calendar: cal, Events: ev,
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

func TestSyncNowAndStatus(t *testing.T) {
	syncFake.err, syncFake.calls = nil, 0
	mux := newServer(fakeParser{}, &fakeCal{}, newMem(), true)
	rec := do(mux, "POST", "/api/sync/now", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"updated":2`) || !strings.Contains(rec.Body.String(), `"cancelled":1`) || syncFake.calls != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(mux, "GET", "/api/sync/status", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"interval_minutes":5`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestSyncNowErrorMapping(t *testing.T) {
	mux := newServer(fakeParser{}, &fakeCal{}, newMem(), true)
	for err, want := range map[error]int{gcal.ErrReauthRequired: 409, gcal.ErrNoCredentials: 409, errors.New("boom"): 502} {
		syncFake.err = err
		if rec := do(mux, "POST", "/api/sync/now", "", nil); rec.Code != want {
			t.Errorf("%v: %d", err, rec.Code)
		}
	}
	syncFake.err = nil
	if rec := do(newServer(fakeParser{}, &fakeCal{}, newMem(), false), "POST", "/api/sync/now", "", nil); rec.Code != 401 {
		t.Fatalf("requires login: %d", rec.Code)
	}
}

type countingTips struct{ calls int }

func (c *countingTips) Tips(context.Context, plan.Event, time.Time) (string, error) {
	c.calls++
	return "・x", nil
}

func TestPlainReminderCategoryGetsNoTipsCall(t *testing.T) {
	ct := &countingTips{}
	rem := &fakeReminders{res: ScheduleResult{Status: "scheduled", Recipients: 1}}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, &fakeContacts{}, rem, ct)
	body := withReminder("req-12345678", 60, 7)
	body["category"] = "reminder"
	if rec := do(mux, "POST", "/api/plans/confirm", "application/json", body); rec.Code != 201 || len(rem.reqs) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if ct.calls != 0 || rem.reqs[0].Tips != "" {
		t.Fatalf("a plain reminder must not call the model: calls=%d tips=%q", ct.calls, rem.reqs[0].Tips)
	}
	body = withReminder("req-87654321", 60, 7) // medical (see validConfirm)
	do(mux, "POST", "/api/plans/confirm", "application/json", body)
	if ct.calls != 1 {
		t.Fatalf("other categories still get tips: %d", ct.calls)
	}
}

func TestContactListShowsLineUserIDOnlyWhenBound(t *testing.T) {
	c := &fakeContacts{list: []Contact{
		{ID: 7, Name: "我自己", Status: "active", IsSelf: true, Enabled: true, LineUserID: "U0123456789abcdef0123456789abcdef"},
		{ID: 8, Name: "媽媽", Status: "pending", Enabled: true},
	}}
	mux := newServerR(fakeParser{}, &fakeCal{}, newMem(), true, c, nil, nil)
	rec := do(mux, "GET", "/api/contacts", "", nil)
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 2 {
		t.Fatalf("%v %s", err, rec.Body)
	}
	if out[0]["line_user_id"] != "U0123456789abcdef0123456789abcdef" {
		t.Fatalf("bound contact must expose its id: %v", out[0])
	}
	if _, present := out[1]["line_user_id"]; present {
		t.Fatalf("an unbound contact has no id to show: %v", out[1])
	}
	if rec := do(newServerR(fakeParser{}, &fakeCal{}, newMem(), false, c, nil, nil), "GET", "/api/contacts", "", nil); rec.Code != 401 || strings.Contains(rec.Body.String(), "U0123") {
		t.Fatalf("the id must never be served without a login: %d %s", rec.Code, rec.Body)
	}
}

func TestGroupDeliveries(t *testing.T) {
	got := GroupDeliveries([]DeliveryInfo{
		{EventID: 1, LeadMinutes: 1440, Name: "我自己", Status: "sent"},
		{EventID: 1, LeadMinutes: 60, Name: "我自己", Status: "pending", Late: true},
		{EventID: 1, LeadMinutes: 60, ContactID: 8, Name: "媽媽", Status: "sending"},
		{EventID: 1, LeadMinutes: 60, Name: "爸爸", Status: "failed", Reason: "quota_exceeded"},
		{EventID: 2, LeadMinutes: 30, Name: "我自己", Status: "sent", Reason: "ignored-for-sent"},
	})
	ev1 := got[1]
	if len(ev1) != 2 || ev1[0].LeadMinutes != 1440 || ev1[1].LeadMinutes != 60 {
		t.Fatalf("groups by lead time, in order: %+v", ev1)
	}
	if !ev1[1].Late || ev1[0].Late {
		t.Fatalf("late flag belongs to its own group: %+v", ev1)
	}
	r := ev1[1].Recipients
	if len(r) != 3 || r[0].Name != "我自己" || r[1].Status != "pending" || r[1].ContactID != 8 || r[2].Reason != "quota_exceeded" {
		t.Fatalf("%+v", r)
	}
	if r := got[2][0].Recipients[0]; r.Reason != "" {
		t.Fatalf("a reason is only meaningful for failed/skipped: %+v", r)
	}
	if len(GroupDeliveries(nil)) != 0 {
		t.Fatal("no deliveries, no groups")
	}
}

func TestEventListIncludesWhoAndHowEarly(t *testing.T) {
	ev := newMem()
	ev.list = []EventRow{
		{ID: 1, Title: "看牙醫", Status: "active", Reminders: []ReminderGroup{
			{LeadMinutes: 60, Recipients: []Recipient{{Name: "我自己", Status: "pending"}, {Name: "媽媽", Status: "sent"}}},
		}},
		{ID: 2, Title: "沒提醒", Status: "active"},
	}
	mux := newServer(fakeParser{}, &fakeCal{}, ev, true)
	rec := do(mux, "GET", "/api/events", "", nil)
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 2 {
		t.Fatalf("%v %s", err, rec.Body)
	}
	rem := out[0]["reminders"].([]any)[0].(map[string]any)
	if rem["lead_minutes"].(float64) != 60 || len(rem["recipients"].([]any)) != 2 {
		t.Fatalf("%v", rem)
	}
	if _, has := out[1]["reminders"]; has {
		t.Fatal("an event without reminders must not carry the field")
	}
}

type fakeEditor struct {
	reqs []EditReq
	res  EditResult
	err  error
}

func (f *fakeEditor) Apply(_ context.Context, r EditReq) (EditResult, error) {
	f.reqs = append(f.reqs, r)
	return f.res, f.err
}

// editServer wires an event that already exists in "Google" (g-1) and a fake editor.
func editServer(cal *fakeCal, ed *fakeEditor, tg TipsGenerator, authed bool) (*http.ServeMux, *memEvents) {
	ev := newMem()
	ev.rows[1] = &EventRow{ID: 1, Title: "看牙醫", Status: "active", GoogleEventID: "g-1"}
	mux := newServerR(fakeParser{}, cal, ev, authed, &fakeContacts{}, &fakeReminders{}, tg)
	// newServerR builds its own Server; rebuild with the editor attached.
	s := &Server{Editor: ed, Sync: syncFake, Groups: &fakeGroups{}, Usage: fakeUsage{}, Contacts: &fakeContacts{},
		Reminders: &fakeReminders{}, Tips: tg, Parser: fakeParser{}, Transcriber: fakeTr{}, Calendar: cal, Events: ev,
		UserID: func(context.Context) (int64, bool) { return 1, authed }, Now: func() time.Time { return fixedNow }}
	mux = http.NewServeMux()
	s.Register(mux, func(h http.Handler) http.Handler { return h })
	return mux, ev
}

func editBody(reminder any) map[string]any {
	return map[string]any{"title": "改期看牙醫", "start_time": "2026-10-08T15:00:00+08:00", "duration_minutes": 45,
		"location": "新診所", "notes": "帶二代健保卡", "category": "medical", "reminder": reminder}
}

func TestEditUpdatesGoogleThenAppliesEverythingTogether(t *testing.T) {
	cal, ed := &fakeCal{}, &fakeEditor{res: EditResult{Status: "scheduled", Recipients: 2}}
	mux, _ := editServer(cal, ed, fakeTips{out: "・帶健保卡"}, true)
	rec := do(mux, "PATCH", "/api/events/1", "application/json",
		editBody(map[string]any{"lead_minutes": 30, "contact_ids": []int64{7, 100}}))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"reminder":"scheduled"`) || !strings.Contains(rec.Body.String(), `"reminder_recipients":2`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if cal.updates != 1 || cal.lastUpdate.Title != "改期看牙醫" || cal.lastUpdate.DurationMinutes != 45 || cal.lastUpdate.Location != "新診所" {
		t.Fatalf("Google must get the new content: %+v", cal.lastUpdate)
	}
	if len(ed.reqs) != 1 {
		t.Fatalf("%d", len(ed.reqs))
	}
	q := ed.reqs[0]
	if q.EventID != 1 || q.Reminder == nil || q.Reminder.LeadMinutes != 30 || len(q.Reminder.ContactIDs) != 2 ||
		q.Tips == nil || *q.Tips != "・帶健保卡" || q.Event.Notes != "帶二代健保卡" {
		t.Fatalf("%+v", q)
	}
}

func TestEditWithoutRemindersClearsThem(t *testing.T) {
	for _, body := range []any{nil, map[string]any{"lead_minutes": 60, "contact_ids": []int64{}}, map[string]any{"lead_minutes": 0, "contact_ids": []int64{7}}} {
		ed := &fakeEditor{res: EditResult{Status: "cleared"}}
		mux, _ := editServer(&fakeCal{}, ed, fakeTips{out: "x"}, true)
		rec := do(mux, "PATCH", "/api/events/1", "application/json", editBody(body))
		if rec.Code != 200 || len(ed.reqs) != 1 || ed.reqs[0].Reminder != nil || ed.reqs[0].Tips != nil {
			t.Fatalf("%v -> %d %+v", body, rec.Code, ed.reqs)
		}
	}
}

func TestEditRejectsBadInputBeforeTouchingAnything(t *testing.T) {
	cal, ed := &fakeCal{}, &fakeEditor{}
	mux, _ := editServer(cal, ed, nil, true)
	bad := editBody(nil)
	bad["title"] = ""
	if rec := do(mux, "PATCH", "/api/events/1", "application/json", bad); rec.Code != 400 {
		t.Fatalf("blank title: %d", rec.Code)
	}
	if rec := do(mux, "PATCH", "/api/events/1", "application/json", editBody(map[string]any{"lead_minutes": 999999, "contact_ids": []int64{7}})); rec.Code != 400 {
		t.Fatalf("absurd lead: %d", rec.Code)
	}
	if rec := do(mux, "PATCH", "/api/events/abc", "application/json", editBody(nil)); rec.Code != 400 {
		t.Fatalf("bad id: %d", rec.Code)
	}
	if cal.updates != 0 || len(ed.reqs) != 0 {
		t.Fatal("nothing may change for invalid input")
	}
}

func TestEditUnknownEventAndLogin(t *testing.T) {
	cal, ed := &fakeCal{}, &fakeEditor{}
	mux, _ := editServer(cal, ed, nil, true)
	if rec := do(mux, "PATCH", "/api/events/99", "application/json", editBody(nil)); rec.Code != 404 || cal.updates != 0 {
		t.Fatalf("%d", rec.Code)
	}
	mux, _ = editServer(cal, ed, nil, false)
	if rec := do(mux, "PATCH", "/api/events/1", "application/json", editBody(nil)); rec.Code != 401 || cal.updates != 0 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestEditStopsWhenGoogleRefuses(t *testing.T) {
	for err, want := range map[error]string{
		gcal.ErrReauthRequired: "reauth_required", gcal.ErrEventGone: "event_gone", errors.New("boom"): "calendar_failed",
	} {
		cal, ed := &fakeCal{updateErr: err}, &fakeEditor{}
		mux, _ := editServer(cal, ed, fakeTips{out: "x"}, true)
		rec := do(mux, "PATCH", "/api/events/1", "application/json", editBody(map[string]any{"lead_minutes": 60, "contact_ids": []int64{7}}))
		if !strings.Contains(rec.Body.String(), want) || len(ed.reqs) != 0 {
			t.Errorf("%v: %d %s (editor calls=%d)", err, rec.Code, rec.Body, len(ed.reqs))
		}
	}
}

func TestEditKeepsStoredAdviceWhenTheModelFails(t *testing.T) {
	ed := &fakeEditor{res: EditResult{Status: "scheduled", Recipients: 1}}
	mux, _ := editServer(&fakeCal{}, ed, fakeTips{err: errors.New("llm down")}, true)
	rec := do(mux, "PATCH", "/api/events/1", "application/json", editBody(map[string]any{"lead_minutes": 60, "contact_ids": []int64{7}}))
	if rec.Code != 200 || ed.reqs[0].Tips != nil {
		t.Fatalf("a nil Tips means keep what is stored: %d %+v", rec.Code, ed.reqs)
	}
}

func TestEditPlainReminderCategoryAndPastEventsGetNoTipsCall(t *testing.T) {
	ct := &countingTips{}
	ed := &fakeEditor{}
	mux, _ := editServer(&fakeCal{}, ed, ct, true)
	b := editBody(map[string]any{"lead_minutes": 60, "contact_ids": []int64{7}})
	b["category"] = "reminder"
	do(mux, "PATCH", "/api/events/1", "application/json", b)
	b = editBody(map[string]any{"lead_minutes": 60, "contact_ids": []int64{7}})
	b["start_time"] = "2026-09-01T09:00:00+08:00" // before the fixed "now"
	do(mux, "PATCH", "/api/events/1", "application/json", b)
	if ct.calls != 0 {
		t.Fatalf("no advice call expected: %d", ct.calls)
	}
}

func TestEditLocalFailureAfterGoogleSuccessIsReportedAsPartial(t *testing.T) {
	cal, ed := &fakeCal{}, &fakeEditor{err: errors.New("db down")}
	mux, _ := editServer(cal, ed, nil, true)
	rec := do(mux, "PATCH", "/api/events/1", "application/json", editBody(nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "partial_update") || cal.updates != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
