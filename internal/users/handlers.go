package users

import (
	"errors"
	"log"
	"net/http"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/httpx"
)

// Handlers serves /auth/register, /auth/login, and /admin/users.
type Handlers struct {
	repo   *Repo
	issuer *auth.Issuer
}

// NewHandlers wires the user routes to their dependencies.
func NewHandlers(repo *Repo, issuer *auth.Issuer) *Handlers {
	return &Handlers{repo: repo, issuer: issuer}
}

// Register creates a user-role account.
//
// It answers 201 with the new user and no token: login is the next call, which
// keeps one code path for issuing tokens instead of two.
func (h *Handlers) Register(w http.ResponseWriter, r *http.Request) {
	var in Credentials
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}

	email, err := ValidateCredentials(in.Email, in.Password)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		// ValidateCredentials already enforced the 72-byte limit, so this is a
		// genuine failure rather than a bad request.
		log.Printf("register: hash password: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	user, err := h.repo.Create(r.Context(), email, hash, auth.RoleUser)
	switch {
	case errors.Is(err, ErrEmailTaken):
		httpx.WriteError(w, http.StatusConflict, "email already registered")
		return
	case err != nil:
		log.Printf("register: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, user)
}

// Login exchanges credentials for a 24h token.
//
// Every failure is the same 401 with the same body, and -- just as important
// -- the same cost: when no user matches, auth.DummyCheck burns the bcrypt
// time a real comparison would have taken. Identical bodies with a
// 250ms-versus-0ms split still answer "is this address registered?".
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	var in Credentials
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}

	// Deliberately not ValidateCredentials: a malformed email at login is an
	// authentication failure like any other, and a 400 here would confirm that
	// well-formed addresses get further.
	user, err := h.repo.GetByEmail(r.Context(), NormalizeEmail(in.Email))
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			log.Printf("login: %v", err)
		}
		auth.DummyCheck(in.Password)
		unauthorized(w)
		return
	}

	if err := auth.CheckPassword(user.PasswordHash, in.Password); err != nil {
		unauthorized(w)
		return
	}

	token, err := h.issuer.Issue(user.ID, user.Role)
	if err != nil {
		log.Printf("login: issue token: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, TokenResponse{Token: token})
}

// AdminListUsers returns every account. The route is mounted behind
// RequireRole("admin"); password hashes are excluded by User's json tag.
func (h *Handlers) AdminListUsers(w http.ResponseWriter, r *http.Request) {
	found, err := h.repo.List(r.Context())
	if err != nil {
		log.Printf("admin list users: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, found)
}

// unauthorized is the single login failure. It says nothing about which half
// of the credentials was wrong.
func unauthorized(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusUnauthorized, "invalid email or password")
}
