// Package applications owns the application model, its repository, the search
// query builder, and the seven /applications handlers.
package applications

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/workflow"
)

// DateLayout is the wire format for applied_on. The column is a `date`, not a
// timestamp, so sending RFC3339 would invent a time and a zone that the
// database does not store and cannot round-trip.
const DateLayout = "2006-01-02"

// Date is a calendar date that marshals as "YYYY-MM-DD".
type Date time.Time

// MarshalJSON renders the date as a quoted YYYY-MM-DD string.
func (d Date) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(d).Format(DateLayout) + `"`), nil
}

// UnmarshalJSON parses a quoted YYYY-MM-DD string, rejecting anything else --
// including a full timestamp, which would otherwise silently lose its time
// component on the way into a date column.
func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("applied_on must be a %q string", DateLayout)
	}
	parsed, err := time.Parse(DateLayout, s)
	if err != nil {
		return fmt.Errorf("applied_on must be a %q string", DateLayout)
	}
	*d = Date(parsed)
	return nil
}

// Time returns the underlying time.Time, for passing to pgx.
func (d Date) Time() time.Time { return time.Time(d) }

// String renders the date in wire format.
func (d Date) String() string { return time.Time(d).Format(DateLayout) }

// Application is a row of the applications table joined to its company name.
//
// company_name is returned alongside company_id because a client showing a
// list should not have to make a second call per row to render it; companies
// are normalized in storage, denormalized in the response.
type Application struct {
	ID          uuid.UUID      `json:"id"`
	UserID      uuid.UUID      `json:"user_id"`
	CompanyID   uuid.UUID      `json:"company_id"`
	CompanyName string         `json:"company_name"`
	RoleTitle   string         `json:"role_title"`
	Location    *string        `json:"location"`
	Notes       *string        `json:"notes"`
	Status      workflow.State `json:"status"`
	AppliedOn   Date           `json:"applied_on"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// Transition is a row of the status_transitions audit table.
type Transition struct {
	ID            int64          `json:"id"`
	ApplicationID uuid.UUID      `json:"application_id"`
	FromStatus    workflow.State `json:"from_status"`
	ToStatus      workflow.State `json:"to_status"`
	ChangedBy     uuid.UUID      `json:"changed_by"`
	ChangedAt     time.Time      `json:"changed_at"`
}

// CreateInput is the body of POST /applications.
type CreateInput struct {
	CompanyName string  `json:"company_name"`
	RoleTitle   string  `json:"role_title"`
	Location    *string `json:"location"`
	Notes       *string `json:"notes"`
	AppliedOn   *Date   `json:"applied_on"`
}

// Validate normalizes and checks a create body.
func (in *CreateInput) Validate() error {
	in.CompanyName = strings.TrimSpace(in.CompanyName)
	in.RoleTitle = strings.TrimSpace(in.RoleTitle)

	switch {
	case in.CompanyName == "":
		return fmt.Errorf("company_name is required")
	case in.RoleTitle == "":
		return fmt.Errorf("role_title is required")
	case len(in.CompanyName) > 200:
		return fmt.Errorf("company_name must not exceed 200 characters")
	case len(in.RoleTitle) > 200:
		return fmt.Errorf("role_title must not exceed 200 characters")
	}
	return nil
}

// Optional distinguishes a JSON field that was absent from one that was sent
// as null -- the difference between "leave this column alone" and "clear it".
//
// A plain *string cannot express that: both arrive as nil. PATCH needs the
// distinction, because clearing `notes` is a thing a client should be able to
// do without a separate endpoint.
type Optional[T any] struct {
	Set   bool // the key appeared in the body
	Value *T   // nil when the value was explicitly null
}

// UnmarshalJSON records that the field was present, then decodes it. It is
// never called for an absent key, which is exactly what makes Set meaningful.
func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// PatchInput is the body of PATCH /applications/{id}.
//
// Status is declared even though it is never applied: it has to be a known
// field so an attempt to set it gets a 400 naming the transition endpoint,
// rather than httpx's generic "unknown field status". Transitions are
// state-machine-checked and audited; a PATCH would be neither.
type PatchInput struct {
	RoleTitle Optional[string] `json:"role_title"`
	Location  Optional[string] `json:"location"`
	Notes     Optional[string] `json:"notes"`
	AppliedOn Optional[Date]   `json:"applied_on"`
	Status    json.RawMessage  `json:"status"`
}

// Validate checks a patch body and reports whether it changes anything.
func (in *PatchInput) Validate() error {
	if len(in.Status) > 0 {
		return fmt.Errorf("status cannot be changed here; use POST /applications/{id}/transition")
	}

	if in.RoleTitle.Set {
		if in.RoleTitle.Value == nil {
			return fmt.Errorf("role_title cannot be null")
		}
		trimmed := strings.TrimSpace(*in.RoleTitle.Value)
		if trimmed == "" {
			return fmt.Errorf("role_title cannot be empty")
		}
		if len(trimmed) > 200 {
			return fmt.Errorf("role_title must not exceed 200 characters")
		}
		in.RoleTitle.Value = &trimmed
	}

	if in.AppliedOn.Set && in.AppliedOn.Value == nil {
		return fmt.Errorf("applied_on cannot be null")
	}

	if !in.RoleTitle.Set && !in.Location.Set && !in.Notes.Set && !in.AppliedOn.Set {
		return fmt.Errorf("no fields to update")
	}
	return nil
}

// TransitionInput is the body of POST /applications/{id}/transition.
type TransitionInput struct {
	To workflow.State `json:"to"`
}
