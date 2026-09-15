package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// Repo is every SQL statement in the router.
//
// Read methods run on the query_only handle and write methods on the serialised
// one. That split is not a convention here — the read handle physically cannot
// write — so a read method that grew a mutation would fail at runtime on its
// first call rather than passing review.
//
// Repo executes; it decides nothing. Whether a job MAY be claimed, charged or
// retried is router.Service's judgement, and keeping that out of here is what
// stops the business rules existing in two places.
type Repo struct {
	read  *sql.DB
	write *sql.DB
}

// NewRepo returns a Repo over the two handles.
func NewRepo(db *DB) *Repo { return &Repo{read: db.Read, write: db.Write} }

// ---------- reads ----------

const userColumns = `id, email, role, credits, buffer_limit, priority, job_ttl_secs, active, created_at`

func scanUser(row interface{ Scan(...any) error }) (core.User, error) {
	var (
		u      core.User
		active int
		create int64
	)
	err := row.Scan(&u.ID, &u.Email, &u.Role, &u.Credits, &u.BufferLimit,
		&u.Priority, &u.JobTTLSecs, &active, &create)
	if errors.Is(err, sql.ErrNoRows) {
		return core.User{}, core.ErrNotFound
	}
	if err != nil {
		return core.User{}, err
	}
	u.Active = active == 1
	u.CreatedAt = time.Unix(create, 0).UTC()
	return u, nil
}

// UserByID loads one user.
func (r *Repo) UserByID(ctx context.Context, id string) (core.User, error) {
	return scanUser(r.read.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// UserByEmail loads one user by their normalised email.
func (r *Repo) UserByEmail(ctx context.Context, email string) (core.User, error) {
	return scanUser(r.read.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = ?`, email))
}

// PasswordHashByEmail returns a user's id and password hash.
//
// ⚠ A PURPOSE-BUILT READ, and deliberately not a field on core.User. The hash is
// absent from userColumns, so it cannot reach a handler that renders a user
// table simply by someone adding a column to a template — there is nowhere on
// the struct for it to sit. This query is the only path that loads it, and its
// only caller is password verification.
//
// An unknown email returns core.ErrNotFound with an EMPTY hash, never a
// partially populated result a caller might verify against.
func (r *Repo) PasswordHashByEmail(ctx context.Context, email string) (userID, hash string, err error) {
	err = r.read.QueryRowContext(ctx,
		`SELECT id, password_hash FROM users WHERE email = ?`, email).Scan(&userID, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", core.ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	return userID, hash, nil
}

// ListUsers returns every user, newest first. uuidv7 ids sort by creation, so
// no ORDER BY created_at is needed.
func (r *Repo) ListUsers(ctx context.Context) ([]core.User, error) {
	rows, err := r.read.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TokenByHash loads a token by the hex SHA-256 of its plaintext.
//
// It returns core.ErrNotFound for an unknown hash; the caller maps unknown,
// revoked and inactive-user to one indistinguishable error so the response
// cannot be used to enumerate tokens.
func (r *Repo) TokenByHash(ctx context.Context, hash string) (core.Token, error) {
	var (
		t        core.Token
		revoked  int
		created  int64
		lastSeen sql.NullInt64
	)
	err := r.read.QueryRowContext(ctx,
		`SELECT id, user_id, role, hash, label, revoked, created_at, last_seen_at
		 FROM tokens WHERE hash = ?`, hash).
		Scan(&t.ID, &t.UserID, &t.Role, &t.Hash, &t.Label, &revoked, &created, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Token{}, core.ErrNotFound
	}
	if err != nil {
		return core.Token{}, err
	}
	t.Revoked = revoked == 1
	t.CreatedAt = time.Unix(created, 0).UTC()
	if lastSeen.Valid {
		t.LastSeenAt = time.Unix(lastSeen.Int64, 0).UTC()
	}
	return t, nil
}

// ListTokens returns a user's tokens. The hash is included because it is not a
// secret — the plaintext is, and that is not stored anywhere.
func (r *Repo) ListTokens(ctx context.Context, userID string) ([]core.Token, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT id, user_id, role, hash, label, revoked, created_at, last_seen_at
		 FROM tokens WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Token
	for rows.Next() {
		var (
			t        core.Token
			revoked  int
			created  int64
			lastSeen sql.NullInt64
		)
		if err := rows.Scan(&t.ID, &t.UserID, &t.Role, &t.Hash, &t.Label,
			&revoked, &created, &lastSeen); err != nil {
			return nil, err
		}
		t.Revoked = revoked == 1
		t.CreatedAt = time.Unix(created, 0).UTC()
		if lastSeen.Valid {
			t.LastSeenAt = time.Unix(lastSeen.Int64, 0).UTC()
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const jobColumns = `id, user_id, filename, size_byte, label, pipeline, stage, params,
	has_blob, state, attempts, units, accrued_credits, worker_id, lease_expires_at,
	last_error, queued_at, expires_at, created_at, updated_at`

func scanJob(row interface{ Scan(...any) error }) (core.Job, error) {
	var (
		j        core.Job
		pipeline string
		params   string
		hasBlob  int
		lease    sql.NullInt64
		expires  sql.NullInt64
		queued   int64
		created  int64
		updated  int64
	)
	err := row.Scan(&j.ID, &j.UserID, &j.Filename, &j.SizeByte, &j.Label, &pipeline,
		&j.Stage, &params, &hasBlob, &j.State, &j.Attempts, &j.Units, &j.AccruedCredits,
		&j.WorkerID, &lease, &j.LastError, &queued, &expires, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Job{}, core.ErrNotFound
	}
	if err != nil {
		return core.Job{}, err
	}
	if err := json.Unmarshal([]byte(pipeline), &j.Pipeline); err != nil {
		return core.Job{}, fmt.Errorf("job %s: decoding pipeline: %w", j.ID, err)
	}
	if err := json.Unmarshal([]byte(params), &j.Params); err != nil {
		return core.Job{}, fmt.Errorf("job %s: decoding params: %w", j.ID, err)
	}
	j.HasBlob = hasBlob == 1
	if lease.Valid {
		j.LeaseExpiresAt = time.Unix(lease.Int64, 0).UTC()
	}
	if expires.Valid {
		j.ExpiresAt = time.Unix(expires.Int64, 0).UTC()
	}
	j.QueuedAt = time.Unix(queued, 0).UTC()
	j.CreatedAt = time.Unix(created, 0).UTC()
	j.UpdatedAt = time.Unix(updated, 0).UTC()
	return j, nil
}

// JobByID loads one job.
func (r *Repo) JobByID(ctx context.Context, id string) (core.Job, error) {
	return scanJob(r.read.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
}

// JobsByUserAndState lists a user's jobs in one state, oldest first.
func (r *Repo) JobsByUserAndState(ctx context.Context, userID string, state core.JobState) ([]core.Job, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE user_id = ? AND state = ? ORDER BY id ASC`,
		userID, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectJobs(rows)
}

// ListJobs returns the most recent jobs across all users, for the dashboard.
func (r *Repo) ListJobs(ctx context.Context, limit int) ([]core.Job, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectJobs(rows)
}

func collectJobs(rows *sql.Rows) ([]core.Job, error) {
	var out []core.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// CountInFlight counts a user's jobs that occupy their buffer.
//
// In-flight is queued + processing + done: a finished-but-uncollected job still
// holds a slot, because its result occupies memory and its blob occupies disk
// until the customer takes it.
func (r *Repo) CountInFlight(ctx context.Context, userID string) (int, error) {
	var n int
	err := r.read.QueryRowContext(ctx,
		`SELECT count(*) FROM jobs WHERE user_id = ? AND state IN ('queued','processing','done')`,
		userID).Scan(&n)
	return n, err
}

// QueueDepthByLabel returns the number of queued jobs per label.
func (r *Repo) QueueDepthByLabel(ctx context.Context) (map[string]int, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT label, count(*) FROM jobs WHERE state = 'queued' GROUP BY label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var label string
		var n int
		if err := rows.Scan(&label, &n); err != nil {
			return nil, err
		}
		out[label] = n
	}
	return out, rows.Err()
}

// OldestQueuedByLabel returns, per label, when the oldest queued job was queued.
//
// This is the stall signal: depth can sit low while one job is stuck forever, so
// depth alone cannot distinguish a healthy short queue from a wedged one.
func (r *Repo) OldestQueuedByLabel(ctx context.Context) (map[string]time.Time, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT label, min(queued_at) FROM jobs WHERE state = 'queued' GROUP BY label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var label string
		var oldest int64
		if err := rows.Scan(&label, &oldest); err != nil {
			return nil, err
		}
		out[label] = time.Unix(oldest, 0).UTC()
	}
	return out, rows.Err()
}

// CountJobsByState returns a census of job states, for the dashboard and metrics.
func (r *Repo) CountJobsByState(ctx context.Context) (map[core.JobState]int, error) {
	rows, err := r.read.QueryContext(ctx, `SELECT state, count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[core.JobState]int{}
	for rows.Next() {
		var s core.JobState
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// Ledger returns a user's credit movements, newest first.
func (r *Repo) Ledger(ctx context.Context, userID string, limit int) ([]core.LedgerEntry, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT id, user_id, job_id, delta, reason, created_at
		 FROM credit_entries WHERE user_id = ? ORDER BY id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.LedgerEntry
	for rows.Next() {
		var (
			e       core.LedgerEntry
			jobID   sql.NullString
			created int64
		)
		if err := rows.Scan(&e.ID, &e.UserID, &jobID, &e.Delta, &e.Reason, &created); err != nil {
			return nil, err
		}
		e.JobID = jobID.String
		e.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// RateForLabel returns the credits charged per output unit for a service.
//
// A label with no row costs 1. That default matters: returning 0 for an
// unconfigured service would make every new service silently free, and nobody
// would notice until the bill did not arrive.
func (r *Repo) RateForLabel(ctx context.Context, label string) (int, error) {
	var rate int
	err := r.read.QueryRowContext(ctx,
		`SELECT credits_per_unit FROM service_rates WHERE label = ?`, label).Scan(&rate)
	if errors.Is(err, sql.ErrNoRows) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	return rate, nil
}

// ListRates returns every configured service rate.
func (r *Repo) ListRates(ctx context.Context) (map[string]int, error) {
	rows, err := r.read.QueryContext(ctx, `SELECT label, credits_per_unit FROM service_rates`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var label string
		var rate int
		if err := rows.Scan(&label, &rate); err != nil {
			return nil, err
		}
		out[label] = rate
	}
	return out, rows.Err()
}
