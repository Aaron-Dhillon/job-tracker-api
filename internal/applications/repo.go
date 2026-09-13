package applications

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/workflow"
)

// ErrNotFound means no application matched the id -- or matched the id but
// belongs to someone else. The two are deliberately the same error, because
// the handler turns both into 404: a 403 would confirm that the id exists.
var ErrNotFound = errors.New("application not found")

// Repo reads and writes applications, companies, and the transition audit log.
type Repo struct {
	db *pgxpool.Pool
}

// NewRepo returns a Repo backed by pool.
func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{db: pool} }

// Create upserts the company and inserts the application in one transaction,
// so a failed insert cannot leave an orphan company row behind.
func (r *Repo) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Application, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Application{}, fmt.Errorf("create application: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// do update rather than do nothing: `do nothing` returns no row on
	// conflict, so RETURNING would come back empty for every company after the
	// first. Assigning the name to itself is a no-op that makes the row visible
	// to RETURNING.
	const upsertCompany = `
		insert into companies (name) values ($1)
		on conflict (name) do update set name = excluded.name
		returning id, name`

	var companyID uuid.UUID
	var companyName string
	if err := tx.QueryRow(ctx, upsertCompany, strings.TrimSpace(in.CompanyName)).Scan(&companyID, &companyName); err != nil {
		return Application{}, fmt.Errorf("upsert company: %w", err)
	}

	// applied_on is passed as NULL when absent so the column default
	// (current_date) applies, rather than the zero time.
	var appliedOn *time.Time
	if in.AppliedOn != nil {
		t := in.AppliedOn.Time()
		appliedOn = &t
	}

	const insertApp = `
		insert into applications (user_id, company_id, role_title, location, notes, applied_on)
		values ($1, $2, $3, $4, $5, coalesce($6, current_date))
		returning id, user_id, company_id, role_title, location, notes, status, applied_on, created_at, updated_at`

	app, err := scanApp(tx.QueryRow(ctx, insertApp,
		userID, companyID, strings.TrimSpace(in.RoleTitle), in.Location, in.Notes, appliedOn))
	if err != nil {
		return Application{}, fmt.Errorf("insert application: %w", err)
	}
	app.CompanyName = companyName

	if err := tx.Commit(ctx); err != nil {
		return Application{}, fmt.Errorf("create application: %w", err)
	}
	return app, nil
}

// Get returns one application. A non-nil ownerID restricts the read to that
// user's rows; nil means unrestricted, which is how an admin reads any row.
func (r *Repo) Get(ctx context.Context, id uuid.UUID, ownerID *uuid.UUID) (Application, error) {
	q := `select` + selectColumns + `
		from applications a
		join companies c on c.id = a.company_id
		where a.id = $1`
	params := []any{id}

	if ownerID != nil {
		q += ` and a.user_id = $2`
		params = append(params, *ownerID)
	}

	app, err := scanAppWithCompany(r.db.QueryRow(ctx, q, params...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Application{}, ErrNotFound
		}
		return Application{}, fmt.Errorf("get application: %w", err)
	}
	return app, nil
}

// List runs the query BuildListQuery assembles for p.
func (r *Repo) List(ctx context.Context, p ListParams) ([]Application, error) {
	q, params := BuildListQuery(p)

	rows, err := r.db.Query(ctx, q, params...)
	if err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	defer rows.Close()

	// Non-nil so no results marshal as [] rather than null.
	found := make([]Application, 0)
	for rows.Next() {
		app, err := scanAppWithCompany(rows)
		if err != nil {
			return nil, fmt.Errorf("scan application: %w", err)
		}
		found = append(found, app)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	return found, nil
}

// Update applies a patch to an application the caller owns.
//
// Ownership is part of the WHERE rather than a prior SELECT: a check-then-write
// pair can be raced, and this way a non-owner's update simply matches no row.
// updated_at is set explicitly -- there is no trigger, by choice, because a
// trigger is a second place to look when a timestamp is wrong.
func (r *Repo) Update(ctx context.Context, id, ownerID uuid.UUID, in PatchInput) (Application, error) {
	var (
		a    args
		sets []string
	)

	if in.RoleTitle.Set {
		sets = append(sets, "role_title = "+a.add(*in.RoleTitle.Value))
	}
	if in.Location.Set {
		sets = append(sets, "location = "+a.add(in.Location.Value))
	}
	if in.Notes.Set {
		sets = append(sets, "notes = "+a.add(in.Notes.Value))
	}
	if in.AppliedOn.Set {
		sets = append(sets, "applied_on = "+a.add(in.AppliedOn.Value.Time()))
	}
	if len(sets) == 0 {
		return Application{}, fmt.Errorf("update application: no fields to set")
	}
	sets = append(sets, "updated_at = now()")

	q := `update applications set ` + strings.Join(sets, ", ") +
		` where id = ` + a.add(id) + ` and user_id = ` + a.add(ownerID) +
		` returning id, user_id, company_id, role_title, location, notes, status, applied_on, created_at, updated_at`

	app, err := scanApp(r.db.QueryRow(ctx, q, a.values...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Application{}, ErrNotFound
		}
		return Application{}, fmt.Errorf("update application: %w", err)
	}

	if err := r.db.QueryRow(ctx, `select name from companies where id = $1`, app.CompanyID).Scan(&app.CompanyName); err != nil {
		return Application{}, fmt.Errorf("update application: read company: %w", err)
	}
	return app, nil
}

// Delete removes an application the caller owns. status_transitions rows go
// with it through the foreign key's on delete cascade.
func (r *Repo) Delete(ctx context.Context, id, ownerID uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `delete from applications where id = $1 and user_id = $2`, id, ownerID)
	if err != nil {
		return fmt.Errorf("delete application: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Transition moves an application to a new state and records the move.
//
// The whole operation is one transaction, and it opens by locking the row with
// SELECT ... FOR UPDATE. Without that lock two concurrent requests can both
// read status 'applied', both pass workflow.Transition, and both write -- one
// overwriting the other's status and leaving two audit rows claiming to start
// from 'applied'. The lock makes the second request read the first's result
// and be rejected by the state machine, which is the correct outcome.
//
// The returned State is the status the application was in, which the handler
// needs to build the 409 body for a rejected move.
func (r *Repo) Transition(ctx context.Context, id, ownerID uuid.UUID, to workflow.State) (Application, workflow.State, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Application{}, "", fmt.Errorf("transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var from workflow.State
	err = tx.QueryRow(ctx,
		`select status from applications where id = $1 and user_id = $2 for update`,
		id, ownerID).Scan(&from)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Application{}, "", ErrNotFound
		}
		return Application{}, "", fmt.Errorf("transition: lock application: %w", err)
	}

	// Checked before any write, so a rejected move touches nothing and leaves
	// no audit row.
	if err := workflow.Transition(from, to); err != nil {
		return Application{}, from, err
	}

	const update = `
		update applications set status = $1, updated_at = now()
		where id = $2
		returning id, user_id, company_id, role_title, location, notes, status, applied_on, created_at, updated_at`

	app, err := scanApp(tx.QueryRow(ctx, update, string(to), id))
	if err != nil {
		return Application{}, from, fmt.Errorf("transition: update status: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`insert into status_transitions (application_id, from_status, to_status, changed_by) values ($1, $2, $3, $4)`,
		id, string(from), string(to), ownerID); err != nil {
		return Application{}, from, fmt.Errorf("transition: record audit row: %w", err)
	}

	if err := tx.QueryRow(ctx, `select name from companies where id = $1`, app.CompanyID).Scan(&app.CompanyName); err != nil {
		return Application{}, from, fmt.Errorf("transition: read company: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Application{}, from, fmt.Errorf("transition: %w", err)
	}
	return app, from, nil
}

// History returns the audit trail for an application, oldest first. ownerID
// follows the same rule as Get: nil for an admin, the caller otherwise.
func (r *Repo) History(ctx context.Context, id uuid.UUID, ownerID *uuid.UUID) ([]Transition, error) {
	// The existence check runs first so a missing application and one owned by
	// someone else both yield 404, rather than an empty history that would
	// confirm the id exists.
	if _, err := r.Get(ctx, id, ownerID); err != nil {
		return nil, err
	}

	const q = `
		select id, application_id, from_status, to_status, changed_by, changed_at
		from status_transitions
		where application_id = $1
		order by changed_at, id`

	rows, err := r.db.Query(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("list history: %w", err)
	}
	defer rows.Close()

	found := make([]Transition, 0)
	for rows.Next() {
		var t Transition
		if err := rows.Scan(&t.ID, &t.ApplicationID, &t.FromStatus, &t.ToStatus, &t.ChangedBy, &t.ChangedAt); err != nil {
			return nil, fmt.Errorf("scan transition: %w", err)
		}
		found = append(found, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list history: %w", err)
	}
	return found, nil
}

// scanner is the overlap between pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanApp reads the application columns without the joined company name.
func scanApp(row scanner) (Application, error) {
	var (
		app       Application
		appliedOn time.Time
	)
	err := row.Scan(&app.ID, &app.UserID, &app.CompanyID, &app.RoleTitle,
		&app.Location, &app.Notes, &app.Status, &appliedOn, &app.CreatedAt, &app.UpdatedAt)
	app.AppliedOn = Date(appliedOn)
	return app, err
}

// scanAppWithCompany reads the selectColumns projection, company name included.
func scanAppWithCompany(row scanner) (Application, error) {
	var (
		app       Application
		appliedOn time.Time
	)
	err := row.Scan(&app.ID, &app.UserID, &app.CompanyID, &app.CompanyName, &app.RoleTitle,
		&app.Location, &app.Notes, &app.Status, &appliedOn, &app.CreatedAt, &app.UpdatedAt)
	app.AppliedOn = Date(appliedOn)
	return app, err
}
