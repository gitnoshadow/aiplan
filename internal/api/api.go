// Package api implements the JSON endpoints for parsing, confirming and
// listing plans. It depends only on interfaces so it can be tested offline.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"voiceplan/internal/gcal"
	"voiceplan/internal/plan"
)

type Parser interface {
	ParsePlan(ctx context.Context, text string, now time.Time) (plan.Raw, error)
}
type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte, mime string) (string, error)
}
type Calendar interface {
	InsertEvent(ctx context.Context, userID int64, ev plan.Event) (string, error)
	DeleteEvent(ctx context.Context, userID int64, eventID string) error
	Status(ctx context.Context, userID int64) (connected, needsReauth bool, err error)
}

// EventRow is a stored event as returned to the client.
type EventRow struct {
	ID              int64     `json:"id"`
	Title           string    `json:"title"`
	StartTime       string    `json:"start_time"`
	DurationMinutes int       `json:"duration_minutes"`
	Location        string    `json:"location"`
	Notes           string    `json:"notes"`
	Category        string    `json:"category"`
	Status          string    `json:"status"`
	GoogleEventID   string    `json:"-"`
	StartUTC        time.Time `json:"-"`
}

type Events interface {
	// Reserve inserts a pending row; created=false returns the existing row for the same request_id.
	Reserve(ctx context.Context, userID int64, requestID string, ev plan.Event) (row EventRow, created bool, err error)
	Complete(ctx context.Context, id int64, googleEventID string) error
	Abandon(ctx context.Context, id int64) error
	List(ctx context.Context, userID int64, from time.Time, limit int) ([]EventRow, error)
	Get(ctx context.Context, userID, id int64) (EventRow, error)
	Delete(ctx context.Context, userID, id int64) error
}

type Server struct {
	Parser      Parser
	Transcriber Transcriber
	Calendar    Calendar
	Events      Events
	UserID      func(ctx context.Context) (int64, bool)
	Now         func() time.Time
}

// Register mounts routes; wrap is the auth middleware.
func (s *Server) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	h := func(f http.HandlerFunc) http.Handler { return wrap(f) }
	mux.Handle("GET /api/google/status", h(s.googleStatus))
	mux.Handle("POST /api/plans/parse", h(s.parse))
	mux.Handle("POST /api/plans/confirm", h(s.confirm))
	mux.Handle("POST /api/transcribe", h(s.transcribe))
	mux.Handle("GET /api/events", h(s.listEvents))
	mux.Handle("DELETE /api/events/{id}", h(s.deleteEvent))
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) user(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := s.UserID(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
	}
	return id, ok
}

func decode(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func (s *Server) googleStatus(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	connected, needs, err := s.Calendar.Status(r.Context(), uid)
	if err != nil {
		log.Printf("google status: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, 200, map[string]bool{"connected": connected, "needs_reauth": needs})
}

func (s *Server) parse(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.user(w, r); !ok {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if !decode(w, r, 16<<10, &in) {
		return
	}
	text := strings.TrimSpace(in.Text)
	if n := utf8.RuneCountInString(text); n == 0 || n > 1000 {
		writeErr(w, http.StatusBadRequest, "text_length")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	raw, err := s.Parser.ParsePlan(ctx, text, s.now())
	if err != nil {
		log.Printf("parse failed: %v", err) // error text carries no user content
		writeErr(w, http.StatusBadGateway, "parse_failed")
		return
	}
	writeJSON(w, 200, plan.Normalize(raw, s.now()))
}

func (s *Server) transcribe(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.user(w, r); !ok {
		return
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	if ct != "audio/wav" && ct != "audio/x-wav" {
		writeErr(w, http.StatusUnsupportedMediaType, "unsupported_audio")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	audio, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "audio_too_large")
		return
	}
	if len(audio) < 1000 {
		writeErr(w, http.StatusBadRequest, "audio_too_short")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	text, err := s.Transcriber.Transcribe(ctx, audio, "audio/wav")
	if err != nil {
		log.Printf("transcribe failed: %v", err)
		writeErr(w, http.StatusBadGateway, "transcribe_failed")
		return
	}
	writeJSON(w, 200, map[string]string{"text": text})
}

type confirmReq struct {
	RequestID string `json:"request_id"`
	plan.ConfirmInput
}

// confirm is the only path that writes to Google Calendar, and only for an
// explicit user confirmation.
func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	var in confirmReq
	if !decode(w, r, 32<<10, &in) {
		return
	}
	if n := len(in.RequestID); n < 8 || n > 64 {
		writeErr(w, http.StatusBadRequest, "request_id")
		return
	}
	ev, err := in.ConfirmInput.Validate()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_plan")
		return
	}

	row, created, err := s.Events.Reserve(r.Context(), uid, in.RequestID, ev)
	if err != nil {
		log.Printf("reserve event: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if !created { // double tap / retry: never create a second Google event
		if row.Status == "active" {
			writeJSON(w, http.StatusOK, row)
			return
		}
		writeErr(w, http.StatusConflict, "in_progress")
		return
	}

	gid, err := s.Calendar.InsertEvent(r.Context(), uid, ev)
	if err != nil {
		if aerr := s.Events.Abandon(context.WithoutCancel(r.Context()), row.ID); aerr != nil {
			log.Printf("abandon event %d: %v", row.ID, aerr)
		}
		switch {
		case errors.Is(err, gcal.ErrNoCredentials):
			writeErr(w, http.StatusConflict, "calendar_not_connected")
		case errors.Is(err, gcal.ErrReauthRequired):
			writeErr(w, http.StatusConflict, "reauth_required")
		default:
			log.Printf("insert event: %v", err)
			writeErr(w, http.StatusBadGateway, "calendar_failed")
		}
		return
	}
	if err := s.Events.Complete(context.WithoutCancel(r.Context()), row.ID, gid); err != nil {
		log.Printf("complete event %d: %v", row.ID, err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	row.Status = "active"
	writeJSON(w, http.StatusCreated, row)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	rows, err := s.Events.List(r.Context(), uid, s.now().Add(-2*time.Hour), 50)
	if err != nil {
		log.Printf("list events: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if rows == nil {
		rows = []EventRow{}
	}
	writeJSON(w, 200, rows)
}

func (s *Server) deleteEvent(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.user(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_id")
		return
	}
	row, err := s.Events.Get(r.Context(), uid, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if row.GoogleEventID != "" {
		if err := s.Calendar.DeleteEvent(r.Context(), uid, row.GoogleEventID); err != nil {
			if errors.Is(err, gcal.ErrReauthRequired) {
				writeErr(w, http.StatusConflict, "reauth_required")
				return
			}
			log.Printf("delete google event: %v", err)
			writeErr(w, http.StatusBadGateway, "calendar_failed")
			return
		}
	}
	if err := s.Events.Delete(r.Context(), uid, id); err != nil {
		log.Printf("delete event row: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
