package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// ---------- writes ----------
//
// Every method here runs on the serialised write handle. None of them decides
// anything: router.Service owns the rules and these execute them.

// CreateUser inserts a user.
//
// A duplicate email surfaces as core.ErrConflict, matched from the driver's
// constraint CODE rather than from its message — message matching breaks on a
// driver upgrade and cannot tell one unique index from another.
func (r *Repo) CreateUser(ctx context.Context, u core.User) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO users (id, email, role, credits, buffer_limit, priority, job_ttl_secs, active, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Email, u.Role, u.Credits, u.BufferLimit, u.Priority, u.JobTTLSecs,
		boolToInt(u.Active), u.CreatedAt.Unix())
	return mapConstraint(err)
}

// UpdateUser writes the admin-settable knobs. It never touches credits, which
// move only through the ledger transaction in DeliverJob.
func (r *Repo) UpdateUser(ctx context.Context, u core.User) error {
	res, err := r.write.ExecContext(ctx,
		`UPDATE users SET buffer_limit = ?, priority = ?, job_ttl_secs = ?, active = ?
		 WHERE id = ?`,
		u.BufferLimit, u.Priority, u.JobTTLSecs, boolToInt(u.Active), u.ID)
	return affectedOne(res, err)
}

// AddCredits grants or removes credits OUTSIDE a delivery, recording the reason.
//
// It writes the balance and the ledger row in one transaction, for the same
// reason DeliverJob does: a balance that disagrees with its ledger is a
// reconciliation problem nobody can settle afterwards.
func (r *Repo) AddCredits(ctx context.Context, userID string, delta int, reason string, now time.Time) error {
	tx, err := r.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET credits = credits + ? WHERE id = ?`, delta, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO credit_entries (id, user_id, job_id, delta, reason, created_at)
		 VALUES (?,?,NULL,?,?,?)`,
		core.NewID(), userID, delta, reason, now.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateToken inserts a token row. Only the hash is stored.
func (r *Repo) CreateToken(ctx context.Context, t core.Token) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO tokens (id, user_id, role, hash, label, revoked, created_at)
		 VALUES (?,?,?,?,?,0,?)`,
		t.ID, t.UserID, t.Role, t.Hash, t.Label, t.CreatedAt.Unix())
	return mapConstraint(err)
}

// RevokeToken marks a token unusable. The row is kept so an audit can still see
// that it existed and when it was used.
func (r *Repo) RevokeToken(ctx context.Context, id string) error {
	res, err := r.write.ExecContext(ctx, `UPDATE tokens SET revoked = 1 WHERE id = ?`, id)
	return affectedOne(res, err)
}

// TouchToken records last use. Best-effort: a failure here must never fail the
// request that triggered it.
func (r *Repo) TouchToken(ctx context.Context, id string, now time.Time) {
	_, _ = r.write.ExecContext(ctx, `UPDATE tokens SET last_seen_at = ? WHERE id = ?`, now.Unix(), id)
}

// CreateJob inserts a queued job.
func (r *Repo) CreateJob(ctx context.Context, j core.Job) error {
	pipeline, err := json.Marshal(j.Pipeline)
	if err != nil {
		return err
	}
	params, err := json.Marshal(j.Params)
	if err != nil {
		return err
	}
	var expires any
	if !j.ExpiresAt.IsZero() {
		expires = j.ExpiresAt.Unix()
	}
	_, err = r.write.ExecContext(ctx,
		`INSERT INTO jobs (id, user_id, filename, size_byte, label, pipeline, stage, params,
		                   has_blob, raw, state, attempts, units, accrued_credits, worker_id,
		                   lease_expires_at, last_error, queued_at, expires_at, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,0,0,0,'',NULL,'',?,?,?,?)`,
		j.ID, j.UserID, j.Filename, j.SizeByte, j.Label, string(pipeline), j.Stage,
		string(params), boolToInt(j.HasBlob), boolToInt(j.Raw), core.JobQueued,
		j.QueuedAt.Unix(), expires, j.CreatedAt.Unix(), j.UpdatedAt.Unix())
	return mapConstraint(err)
}

// ClaimOneQueued atomically leases the highest-priority claimable job for one
// label, and returns it.
//
// It is ONE statement on purpose. A read-then-write claim — select a candidate,
// then update it — lets two workers select the same row and both believe they
// won; the single UPDATE ... WHERE id = (SELECT ...) RETURNING makes the
// selection and the lease the same atomic act, so first-claim-wins is a property
// of SQLite rather than of our timing.
//
// The ORDER BY is the queue policy in one expression:
//
//	effective priority = users.priority + (now - queued_at) / agingStep
//
// Higher first, then job id ascending — and because ids are uuidv7, that tail is
// FIFO within a tier without a sequence column. Aging is what stops a
// low-priority customer starving forever under sustained VIP load.
//
// ⚠ now is computed ONCE by the caller and bound, never strftime('now'), which
// SQLite would re-evaluate per row and make the ordering unstable mid-sort.
//
// The deadline predicate sits in the same WHERE as the selection, so a job
// cannot expire in a window between being chosen and being leased.
func (r *Repo) ClaimOneQueued(
	ctx context.Context,
	label, workerID string,
	now, leaseUntil time.Time,
	agingStep time.Duration,
) (core.Job, error) {
	step := int64(agingStep.Seconds())
	if step < 1 {
		// Guard against a zero step dividing by zero and, worse, against a
		// sub-second step making the aging term dominate priority entirely.
		step = 1
	}

	var id string
	err := r.write.QueryRowContext(ctx,
		`UPDATE jobs SET state = 'processing', worker_id = ?, lease_expires_at = ?, updated_at = ?
		 WHERE id = (
		     SELECT j.id FROM jobs j JOIN users u ON u.id = j.user_id
		     WHERE j.state = 'queued'
		       AND j.label = ?
		       AND (j.expires_at IS NULL OR j.expires_at > ?)
		     ORDER BY (u.priority + (? - j.queued_at) / ?) DESC, j.id ASC
		     LIMIT 1
		 )
		 RETURNING id`,
		workerID, leaseUntil.Unix(), now.Unix(),
		label, now.Unix(),
		now.Unix(), step,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Job{}, core.ErrNotFound
	}
	if err != nil {
		return core.Job{}, err
	}
	return r.JobByID(ctx, id)
}

// CompleteJob marks the FINAL stage done and records its output size and cost.
func (r *Repo) CompleteJob(ctx context.Context, jobID string, units, accrue int, now time.Time) error {
	res, err := r.write.ExecContext(ctx,
		`UPDATE jobs SET state = 'done', units = ?, accrued_credits = accrued_credits + ?,
		        worker_id = '', lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND state = 'processing'`,
		units, accrue, now.Unix(), jobID)
	return affectedOne(res, err)
}

// AdvanceStage moves a pipeline job to its next stage and requeues it.
//
// ⚠ queued_at is deliberately NOT rewritten. A job halfway through a pipeline
// keeps the age it has accrued, so it is preferred over freshly uploaded work;
// resetting it would send every stage to the back of the aged queue and starve
// long pipelines under load.
func (r *Repo) AdvanceStage(ctx context.Context, jobID, nextLabel string, accrue int, now time.Time) error {
	res, err := r.write.ExecContext(ctx,
		`UPDATE jobs SET state = 'queued', label = ?, stage = stage + 1,
		        accrued_credits = accrued_credits + ?, worker_id = '',
		        lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND state = 'processing'`,
		nextLabel, accrue, now.Unix(), jobID)
	return affectedOne(res, err)
}

// MarkHasBlob records that a job now has a source blob.
//
// It exists for one case: a params-only job — a crawler — whose first stage
// produced output. That output becomes the next stage's input blob, so a job
// that started with no file now has one, and the worker for the next stage must
// be told to pass -i. Inferring this from the filename would be guessing.
func (r *Repo) MarkHasBlob(ctx context.Context, jobID string) error {
	res, err := r.write.ExecContext(ctx, `UPDATE jobs SET has_blob = 1 WHERE id = ?`, jobID)
	return affectedOne(res, err)
}

// RequeueJob returns a failed job to the queue, spending an attempt.
//
// `exitCode` is nil when the failure never reached an exit status — a timeout, an
// output-limit trip, a contract violation. It is written on BOTH failure writers
// so a code cannot appear on one row and vanish on the next.
func (r *Repo) RequeueJob(ctx context.Context, jobID, lastErr string, exitCode *int, now time.Time) error {
	res, err := r.write.ExecContext(ctx,
		`UPDATE jobs SET state = 'queued', attempts = attempts + 1, worker_id = '',
		        lease_expires_at = NULL, last_error = ?, exit_code = ?, updated_at = ?
		 WHERE id = ?`,
		lastErr, nullableInt(exitCode), now.Unix(), jobID)
	return affectedOne(res, err)
}

// ReclaimJob returns an ABANDONED job to the queue without spending an attempt.
//
// ⚠ IT SITS HERE, DIRECTLY BESIDE RequeueJob, ON PURPOSE. The two statements
// differ in exactly one column — `attempts + 1` against `reclaims + 1` — and
// that one column is the whole of ADR-0008. Separated, one of them later gains a
// field the other forgets, and the difference stops being visible to anybody
// reading either.
//
// The lease and worker_id are cleared for the same reason RequeueJob clears
// them: the previous holder must not be able to land a late result on a job
// another worker now owns.
//
// It records no exit code. A lease taken back is not a command that exited —
// that distinction is ADR-0007's, and inventing a 0 here would undo it.
func (r *Repo) ReclaimJob(ctx context.Context, jobID, reason string, now time.Time) error {
	res, err := r.write.ExecContext(ctx,
		`UPDATE jobs SET state = 'queued', reclaims = reclaims + 1, worker_id = '',
		        lease_expires_at = NULL, last_error = ?, updated_at = ?
		 WHERE id = ?`,
		reason, now.Unix(), jobID)
	return affectedOne(res, err)
}

// FailJobDead marks a job beyond retry. It is never charged.
func (r *Repo) FailJobDead(ctx context.Context, jobID, lastErr string, exitCode *int, now time.Time) error {
	res, err := r.write.ExecContext(ctx,
		`UPDATE jobs SET state = 'dead', attempts = attempts + 1, worker_id = '',
		        lease_expires_at = NULL, last_error = ?, exit_code = ?, updated_at = ?
		 WHERE id = ?`,
		lastErr, nullableInt(exitCode), now.Unix(), jobID)
	return affectedOne(res, err)
}

// nullableInt keeps a nil exit code NULL in the column rather than 0.
func nullableInt(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

// DeliverJob is the metering step: the ONLY place credits move for a job.
//
// The ledger row, the balance decrement and the state change commit together.
// Separating them would allow a crash to charge without delivering, or deliver
// without charging, and no later reconciliation could tell which happened.
//
// The WHERE state = 'done' guard is what makes a second delivery charge nothing:
// the second call updates zero rows and returns core.ErrNotFound.
func (r *Repo) DeliverJob(ctx context.Context, jobID, userID string, charge int, now time.Time) error {
	tx, err := r.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = 'delivered', updated_at = ? WHERE id = ? AND state = 'done'`,
		now.Unix(), jobID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Already delivered, or never finished. Either way nothing is charged.
		return core.ErrNotFound
	}

	if charge != 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET credits = credits - ? WHERE id = ?`, charge, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO credit_entries (id, user_id, job_id, delta, reason, created_at)
			 VALUES (?,?,?,?,?,?)`,
			core.NewID(), userID, jobID, -charge, "delivery", now.Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ExpireOverdue moves queued jobs past their deadline to 'expired'.
//
// ⚠ It touches ONLY queued rows. A job a worker is already running has consumed
// worker time that discarding it does not recover, so it runs to completion even
// past its deadline.
func (r *Repo) ExpireOverdue(ctx context.Context, now time.Time) ([]core.Job, error) {
	rows, err := r.write.QueryContext(ctx,
		`UPDATE jobs SET state = 'expired', updated_at = ?
		 WHERE state = 'queued' AND expires_at IS NOT NULL AND expires_at <= ?
		 RETURNING `+jobColumns,
		now.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectJobs(rows)
}

// LeaseExpired returns jobs whose worker went silent past its lease.
func (r *Repo) LeaseExpired(ctx context.Context, now time.Time) ([]core.Job, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs
		 WHERE state = 'processing' AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?`,
		now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectJobs(rows)
}

// ResetInFlightOnBoot returns every processing and done job to the queue.
//
// Leases and in-memory results die with the process, so on boot neither can be
// honoured. Nothing is charged and no blob is deleted: the source is still on
// disk, so the work is simply redone. Because nothing is charged before
// delivery, a restart can only cost repeated work — never a double charge.
func (r *Repo) ResetInFlightOnBoot(ctx context.Context, now time.Time) (int, error) {
	res, err := r.write.ExecContext(ctx,
		`UPDATE jobs SET state = 'queued', worker_id = '', lease_expires_at = NULL, updated_at = ?
		 WHERE state IN ('processing','done')`, now.Unix())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// SetRate upserts a service's price per output unit and its output mode.
//
// `raw` rides with the rate because it IS one: a raw service bills a flat credit
// instead of per unit, so letting it be set anywhere less privileged would put
// pricing back in reach of a worker (ADR-0006, ADR-0001 §security boundary).
func (r *Repo) SetRate(ctx context.Context, label string, creditsPerUnit int, raw bool, now time.Time) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO service_rates (label, credits_per_unit, raw, updated_at) VALUES (?,?,?,?)
		 ON CONFLICT(label) DO UPDATE SET credits_per_unit = excluded.credits_per_unit,
		                                  raw = excluded.raw,
		                                  updated_at = excluded.updated_at`,
		label, creditsPerUnit, boolToInt(raw), now.Unix())
	return err
}

// ---------- helpers ----------

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// affectedOne turns "updated nothing" into core.ErrNotFound.
//
// Every write above is guarded on a state or an id, so zero rows affected means
// the precondition did not hold — a lost race or a missing row — and silently
// reporting success would let a caller believe a transition happened.
func affectedOne(res sql.Result, err error) error {
	if err != nil {
		return mapConstraint(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return core.ErrNotFound
	}
	return nil
}

// sqliteConstraintUnique is SQLITE_CONSTRAINT_UNIQUE.
const sqliteConstraintUnique = 2067

// sqliteConstraintPrimaryKey is SQLITE_CONSTRAINT_PRIMARYKEY.
const sqliteConstraintPrimaryKey = 1555

// SetUserSettings writes the three operator-tunable columns and nothing else.
//
// ⚠ NOT UpdateUser, AND THE DIFFERENCE IS THE POINT. UpdateUser writes the whole
// row, so a caller must read the user first and send every field back — and
// `credits` moves underneath that read on every delivery. An admin editing a
// buffer limit while a job completes would silently restore the pre-delivery
// balance. Three named columns cannot clobber a fourth.
//
// It returns core.ErrNotFound when no row matched: silence would report success
// for a user that does not exist, which is how a settings change appears to work
// and does nothing.
func (r *Repo) SetUserSettings(ctx context.Context, userID string, bufferLimit, priority, jobTTLSecs int) error {
	res, err := r.write.ExecContext(ctx,
		`UPDATE users SET buffer_limit = ?, priority = ?, job_ttl_secs = ? WHERE id = ?`,
		bufferLimit, priority, jobTTLSecs, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return core.ErrNotFound
	}
	return nil
}

// SetPasswordHash stores a user's argon2id hash, or clears it with "".
//
// Clearing is a real operation, not an accident to guard against: an empty hash
// is how an account is barred from form login while keeping its bearer tokens.
func (r *Repo) SetPasswordHash(ctx context.Context, userID, hash string) error {
	res, err := r.write.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Silence here would report success for a user that does not exist,
		// which is how a set-password command appears to work and changes
		// nothing.
		return core.ErrNotFound
	}
	return nil
}

// mapConstraint turns a driver uniqueness violation into core.ErrConflict.
//
// It matches on the driver's numeric CODE through an anonymous interface rather
// than importing the concrete driver type or matching the message text. Message
// matching breaks on a driver upgrade and cannot distinguish one unique index
// from another; the interface assertion needs neither.
func mapConstraint(err error) error {
	if err == nil {
		return nil
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		switch coded.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return core.ErrConflict
		}
	}
	return err
}
