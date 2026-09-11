package users

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// uniqueViolation is SQLSTATE 23505. Catching the constraint violation rather
// than pre-checking with a SELECT is what makes registration safe under
// concurrency: two simultaneous signups for the same address cannot both see
// "not taken" and then both insert.
const uniqueViolation = "23505"

var (
	// ErrEmailTaken means the unique index on users.email rejected the insert.
	ErrEmailTaken = errors.New("email already registered")

	// ErrNotFound means no user matched. Login must not distinguish this from
	// a wrong password in anything it returns.
	ErrNotFound = errors.New("user not found")
)

// columns is the select list shared by every read, so a column added to one
// query cannot go missing from another.
const columns = `id, email, password_hash, role, created_at`

// Repo reads and writes the users table.
type Repo struct {
	db *pgxpool.Pool
}

// NewRepo returns a Repo backed by pool.
func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{db: pool} }

// Create inserts a user. The caller supplies an already-hashed password; this
// package never takes a plaintext password to the database layer.
func (r *Repo) Create(ctx context.Context, email, passwordHash, role string) (User, error) {
	const q = `insert into users (email, password_hash, role) values ($1, $2, $3) returning ` + columns

	user, err := scanOne(r.db.QueryRow(ctx, q, email, passwordHash, role))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// Upsert inserts a user or refreshes an existing one's hash and role. Used
// only by EnsureAdmin; registration goes through Create so a duplicate is a
// 409 rather than a silent password reset.
func (r *Repo) Upsert(ctx context.Context, email, passwordHash, role string) (User, error) {
	const q = `
		insert into users (email, password_hash, role)
		values ($1, $2, $3)
		on conflict (email) do update
		   set password_hash = excluded.password_hash,
		       role          = excluded.role
		returning ` + columns

	user, err := scanOne(r.db.QueryRow(ctx, q, email, passwordHash, role))
	if err != nil {
		return User{}, fmt.Errorf("upsert user: %w", err)
	}
	return user, nil
}

// GetByEmail looks a user up by normalized address.
func (r *Repo) GetByEmail(ctx context.Context, email string) (User, error) {
	const q = `select ` + columns + ` from users where email = $1`

	user, err := scanOne(r.db.QueryRow(ctx, q, email))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("get user by email: %w", err)
	}
	return user, nil
}

// GetByID looks a user up by primary key.
func (r *Repo) GetByID(ctx context.Context, id uuid.UUID) (User, error) {
	const q = `select ` + columns + ` from users where id = $1`

	user, err := scanOne(r.db.QueryRow(ctx, q, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("get user by id: %w", err)
	}
	return user, nil
}

// List returns every user, oldest first. Unpaginated by decision: the admin
// view is a demo surface and pagination metadata is out of scope.
func (r *Repo) List(ctx context.Context) ([]User, error) {
	const q = `select ` + columns + ` from users order by created_at, id`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	// Non-nil so an empty table marshals as [] rather than null.
	found := make([]User, 0)
	for rows.Next() {
		user, err := scanOne(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		found = append(found, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return found, nil
}

// scanner is the overlap between pgx.Row and pgx.Rows, so one scan function
// serves both the single-row and the iterating reads.
type scanner interface {
	Scan(dest ...any) error
}

func scanOne(row scanner) (User, error) {
	var user User
	err := row.Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Role, &user.CreatedAt)
	return user, err
}
