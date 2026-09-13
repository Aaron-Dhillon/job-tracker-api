// Command api is the job tracker HTTP server. It applies database migrations
// on start before listening, so a container deploy is self-contained and
// Render needs no separate migration step.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/db"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/server"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/users"
)

func main() {
	migrateOnly := flag.Bool("migrate-only", false, "apply migrations and exit")
	seedAdmin := flag.Bool("seed-admin", false, "upsert the admin from ADMIN_EMAIL/ADMIN_PASSWORD and exit")
	flag.Parse()

	if err := run(*migrateOnly, *seedAdmin); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(migrateOnly, seedAdmin bool) error {
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

	// Built before the pool so a short or missing JWT_SECRET fails at startup
	// with a clear message, rather than on the first login attempt.
	issuer, err := auth.NewIssuer(os.Getenv("JWT_SECRET"), auth.DefaultTTL)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.New(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := ensureAdmin(ctx, pool, seedAdmin); err != nil {
		return err
	}
	if seedAdmin {
		return nil
	}

	return serve(server.New(pool, issuer), port)
}

// ensureAdmin upserts the seed admin when ADMIN_EMAIL and ADMIN_PASSWORD are
// both set.
//
// On startup an unset pair is simply nothing to do -- the API is perfectly
// usable without an admin. Under -seed-admin it is a failure, because creating
// that account is the only thing the command was asked to do, and exiting 0
// having done nothing is how a missing admin goes unnoticed until a demo.
func ensureAdmin(ctx context.Context, pool *pgxpool.Pool, required bool) error {
	email, password := os.Getenv("ADMIN_EMAIL"), os.Getenv("ADMIN_PASSWORD")

	if email == "" || password == "" {
		if required {
			return users.ErrAdminNotConfigured
		}
		return nil
	}

	admin, err := users.EnsureAdmin(ctx, users.NewRepo(pool), email, password)
	if err != nil {
		return err
	}
	log.Printf("admin account ready: %s", admin.Email)
	return nil
}

// serve runs the HTTP server until SIGTERM, then drains in-flight requests.
// Render sends SIGTERM on every deploy, so without this each release would cut
// off whatever was mid-flight.
func serve(handler http.Handler, port string) error {
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
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
