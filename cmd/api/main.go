// Command api is the job tracker HTTP server. It applies database migrations on
// start before listening, so a container deploy is self-contained.
//
// Phase 1 skeleton: configuration, migrations, and /healthz only. Route wiring
// moves to internal/server in phase 4.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/db"
)

func main() {
	migrateOnly := flag.Bool("migrate-only", false, "apply migrations and exit")
	flag.Parse()

	if err := run(*migrateOnly); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(migrateOnly bool) error {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Println("applying migrations")
	if err := db.Up(databaseURL); err != nil {
		return err
	}
	if migrateOnly {
		log.Println("migrations up to date")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.New(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           router(pool),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case sig := <-shutdown:
		log.Printf("received %s, shutting down", sig)
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		return srv.Shutdown(stopCtx)
	}
}

func router(pool *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// No RealIP: it is deprecated and IP-spoofable, and nothing here keys off
	// the client address (rate limiting is out of scope).
	r.Use(middleware.Recoverer)

	r.Get("/healthz", healthz(pool))
	return r
}

// healthz reports 503 when the database is unreachable. A health check that
// always returns ok is not a health check -- Render keys its deploy gating and
// restarts off this endpoint.
func healthz(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			log.Printf("healthz: ping failed: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// writeJSON is a placeholder; internal/httpx takes this over in phase 3.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write response: %v", err)
	}
}
