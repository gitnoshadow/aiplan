package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"voiceplan/internal/api"
	"voiceplan/internal/auth"
	"voiceplan/internal/calsync"
	"voiceplan/internal/config"
	"voiceplan/internal/data"
	"voiceplan/internal/db"
	"voiceplan/internal/gcal"
	"voiceplan/internal/gemini"
	"voiceplan/internal/httpx"
	"voiceplan/internal/line"
	"voiceplan/internal/scheduler"
	"voiceplan/internal/version"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := auth.NewStore(pool)
	calSvc := gcal.NewService(data.NewCreds(pool), cfg.EncryptionKey, cfg.GoogleClientID, cfg.GoogleClientSecret)
	authH, err := auth.NewHandler(ctx, cfg, store, calSvc)
	if err != nil {
		return err
	}
	llm := gemini.New(cfg.GeminiAPIKey, cfg.LLMModel)
	llm.FallbackModel = cfg.LLMFallbackModel
	lineClient := line.NewClient(cfg.LineAccessToken)
	contacts := data.NewContacts(pool)
	syncer := calsync.New(calSvc, data.NewSync(pool), cfg.CalendarSyncEvery)
	apiSrv := &api.Server{
		Sync:   syncer,
		Groups: data.NewGroups(pool), Usage: data.NewUsage(pool),
		MonthlyLimit: cfg.LineMonthlyLimit, AddFriendURL: cfg.LineAddFriendURL,
		Contacts: contacts, Reminders: data.NewReminders(pool), Tips: llm,
		Parser: llm, Transcriber: llm, Calendar: calSvc, Events: data.NewEvents(pool),
		UserID: func(ctx context.Context) (int64, bool) {
			u, ok := auth.UserFrom(ctx)
			return u.ID, ok
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpx.Healthz(pool))
	mux.HandleFunc("GET /api/version", version.Handler)
	authH.Register(mux)
	apiSrv.Register(mux, authH.Require)
	// Not behind login: LINE calls this, and every request is checked by signature.
	mux.Handle("POST /line/webhook", line.NewWebhook(cfg.LineChannelSecret, contacts, lineClient))
	mux.HandleFunc("/api/", httpx.APINotFound)
	mux.Handle("/", httpx.SPA(cfg.StaticDir))

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httpx.Recover(httpx.Logging(httpx.SecurityHeaders(mux))),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Housekeeping: drop expired sessions hourly.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := store.PurgeExpired(ctx); err != nil {
					log.Printf("purge sessions: %v", err)
				} else if n > 0 {
					log.Printf("purged %d expired sessions", n)
				}
			}
		}
	}()

	// Single app instance = single scheduler. deliveries' unique key and the
	// LINE retry key protect against duplicates even across restarts.
	go syncer.Run(ctx)
	sched := scheduler.New(data.NewQueue(pool), lineClient)
	sched.MonthlyLimit = cfg.LineMonthlyLimit
	go sched.Run(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Printf("listening on :%s (version %s, commit %s)", cfg.Port, version.Version, version.Get().Commit)

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}
