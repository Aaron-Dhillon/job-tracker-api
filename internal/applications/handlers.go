package applications

import (
	"errors"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/auth"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/httpx"
	"github.com/Aaron-Dhillon/job-tracker-api/internal/workflow"
)

// Handlers serves the seven /applications routes.
type Handlers struct {
	repo *Repo
}

// NewHandlers wires the application routes to their repository.
func NewHandlers(repo *Repo) *Handlers { return &Handlers{repo: repo} }

// InvalidTransitionBody is the documented extension to the {"error": "..."}
// envelope (PRD §Endpoints): a 409 also carries the state the application is
// actually in and the moves legal from it, so a client can render the next
// steps without hardcoding the state machine.
//
// Allowed is always an array and never null -- workflow.Next returns an empty
// slice for a terminal state, which is the commonest 409 there is, since
// trying to move out of rejected or withdrawn is what users do.
type InvalidTransitionBody struct {
	Error   string           `json:"error"`
	From    workflow.State   `json:"from"`
	Allowed []workflow.State `json:"allowed"`
}

// List returns the caller's applications, or every application for an admin.
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	params, err := ParseListParams(r.URL.Query(), readScope(principal))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	found, err := h.repo.List(r.Context(), params)
	if err != nil {
		log.Printf("list applications: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, found)
}

// Create adds an application for the caller, upserting the company by name.
func (h *Handlers) Create(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var in CreateInput
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	if err := in.Validate(); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	app, err := h.repo.Create(r.Context(), principal.ID, in)
	if err != nil {
		log.Printf("create application: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, app)
}

// Get returns one application: the caller's own, or any for an admin.
func (h *Handlers) Get(w http.ResponseWriter, r *http.Request) {
	principal, id, ok := principalAndID(w, r)
	if !ok {
		return
	}

	app, err := h.repo.Get(r.Context(), id, readScope(principal))
	if err != nil {
		writeRepoError(w, err, "get application")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, app)
}

// Patch updates the editable fields. Ownership is required even for an admin:
// admin is read-only on other people's rows.
func (h *Handlers) Patch(w http.ResponseWriter, r *http.Request) {
	principal, id, ok := principalAndID(w, r)
	if !ok {
		return
	}

	var in PatchInput
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}
	if err := in.Validate(); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	app, err := h.repo.Update(r.Context(), id, principal.ID, in)
	if err != nil {
		writeRepoError(w, err, "update application")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, app)
}

// Delete removes an application the caller owns, cascading its history.
func (h *Handlers) Delete(w http.ResponseWriter, r *http.Request) {
	principal, id, ok := principalAndID(w, r)
	if !ok {
		return
	}

	if err := h.repo.Delete(r.Context(), id, principal.ID); err != nil {
		writeRepoError(w, err, "delete application")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Transition moves an application through the state machine.
//
// The two failure modes are deliberately different statuses. A target that is
// not a state at all is a malformed request: 400. A real state that cannot be
// reached from here is a conflict with the resource's current state: 409, and
// the body says what that state is and what would have been legal.
func (h *Handlers) Transition(w http.ResponseWriter, r *http.Request) {
	principal, id, ok := principalAndID(w, r)
	if !ok {
		return
	}

	var in TransitionInput
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteDecodeError(w, err)
		return
	}

	// Validated before the transaction opens, so a typo does not take a row
	// lock on the way to being rejected.
	if !workflow.Valid(in.To) {
		httpx.WriteError(w, http.StatusBadRequest, "unknown state")
		return
	}

	app, from, err := h.repo.Transition(r.Context(), id, principal.ID, in.To)
	switch {
	case errors.Is(err, workflow.ErrInvalidTransition):
		httpx.WriteJSON(w, http.StatusConflict, InvalidTransitionBody{
			Error:   "invalid transition",
			From:    from,
			Allowed: workflow.Next(from),
		})
		return
	case errors.Is(err, workflow.ErrUnknownState):
		// Unreachable in practice -- `to` was validated above and `from` comes
		// from a check-constrained column -- but mapping it to 400 rather than
		// letting it fall through to 500 keeps the two failure modes honest.
		httpx.WriteError(w, http.StatusBadRequest, "unknown state")
		return
	case err != nil:
		writeRepoError(w, err, "transition application")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, app)
}

// History returns the audit trail, under the same ownership rule as Get.
func (h *Handlers) History(w http.ResponseWriter, r *http.Request) {
	principal, id, ok := principalAndID(w, r)
	if !ok {
		return
	}

	found, err := h.repo.History(r.Context(), id, readScope(principal))
	if err != nil {
		writeRepoError(w, err, "list history")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, found)
}

// readScope returns the ownership filter for a read: nil for an admin, who may
// read any row, and the caller's id for everyone else.
//
// Writes never call this. An admin can read other people's applications but
// cannot modify them, so PATCH, DELETE and transition all pass principal.ID
// directly.
func readScope(p auth.Principal) *uuid.UUID {
	if p.IsAdmin() {
		return nil
	}
	id := p.ID
	return &id
}

// principalAndID pulls the authenticated caller and the {id} path parameter,
// answering the request itself if either is missing.
func principalAndID(w http.ResponseWriter, r *http.Request) (auth.Principal, uuid.UUID, bool) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return auth.Principal{}, uuid.Nil, false
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		// 404, not 400: a malformed id names no resource, and answering
		// differently for "not a uuid" than for "a uuid you don't own" would
		// tell a caller which shapes are worth probing.
		httpx.WriteError(w, http.StatusNotFound, "application not found")
		return auth.Principal{}, uuid.Nil, false
	}
	return principal, id, true
}

// writeRepoError maps a repository failure to a response. Everything that is
// not a clean not-found is logged and answered generically, so internal detail
// never reaches the client.
func writeRepoError(w http.ResponseWriter, err error, op string) {
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "application not found")
		return
	}
	log.Printf("%s: %v", op, err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal error")
}
