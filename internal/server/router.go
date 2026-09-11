// Package server builds the HTTP router.
//
// This file is not in the PRD's repo layout, which puts route wiring in
// cmd/api/main.go. It exists because package main cannot be imported, so an
// integration test would have to rebuild the routes itself -- and a test that
// wires its own router stops testing the one that ships the moment the two
// drift. Both main.go and the integration suite call New.
package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/applications"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/httpx"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/users"
)

// New builds the complete router: middleware, public routes, and the
// authenticated group.
func New(pool *pgxpool.Pool, issuer *auth.Issuer) http.Handler {
	userHandlers := users.NewHandlers(users.NewRepo(pool), issuer)
	appHandlers := applications.NewHandlers(applications.NewRepo(pool))

	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	// No RealIP: it is deprecated and trivially spoofed through a forged
	// X-Forwarded-For, and nothing here keys off the client address --
	// rate limiting is out of scope.
	r.Use(middleware.Recoverer)
	r.Use(httpx.EchoRequestID)

	// chi answers these in plain text by default, which would make "all JSON"
	// false for the two responses a client is most likely to hit by accident.
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
	})

	r.Get("/healthz", Healthz(pool))

	r.Route("/auth", func(r chi.Router) {
		r.Post("/register", userHandlers.Register)
		r.Post("/login", userHandlers.Login)
	})

	// Everything below requires a valid bearer token. Grouping it means a new
	// route cannot be added unauthenticated by forgetting a line -- the
	// default inside this block is "protected".
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth(issuer))

		r.Route("/applications", func(r chi.Router) {
			r.Get("/", appHandlers.List)
			r.Post("/", appHandlers.Create)
			r.Get("/{id}", appHandlers.Get)
			r.Patch("/{id}", appHandlers.Patch)
			r.Delete("/{id}", appHandlers.Delete)
			r.Post("/{id}/transition", appHandlers.Transition)
			r.Get("/{id}/history", appHandlers.History)
		})

		r.Route("/admin", func(r chi.Router) {
			r.Use(auth.RequireRole(auth.RoleAdmin))
			r.Get("/users", userHandlers.AdminListUsers)
		})
	})

	return r
}

// Healthz reports 503 when the database is unreachable.
//
// A health check that always returns ok is not a health check: Render gates
// deploys and restarts on this endpoint, so it has to fail when the service
// cannot actually serve.
func Healthz(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			log.Printf("healthz: ping failed: %v", err)
			httpx.WriteError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
