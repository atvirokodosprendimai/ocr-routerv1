package session_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/session"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

type harness struct {
	db     *store.DB
	store  *session.Store
	userID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// A real user row, because sessions.user_id has a foreign key.
	id := core.NewID()
	if _, err := db.Write.Exec(
		`INSERT INTO users (id,email,role,created_at) VALUES (?,?,?,?)`,
		id, "admin@example.com", string(core.RoleAdmin), base.Unix()); err != nil {
		t.Fatalf("seeding user: %v", err)
	}
	return &harness{db: db, store: session.New(db), userID: id}
}

func (h *harness) create(t *testing.T, now, expires time.Time) string {
	t.Helper()
	secret, err := h.store.Create(context.Background(), h.userID, now, expires)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return secret
}

func TestCreateThenResolveRoundTrips(t *testing.T) {
	h := newHarness(t)
	secret := h.create(t, base, base.Add(time.Hour))

	got, err := h.store.Resolve(context.Background(), secret, base)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.UserID != h.userID {
		t.Errorf("user id = %q, want %q", got.UserID, h.userID)
	}
	if got.ID == "" {
		t.Error("the resolved session has no id")
	}
}

// TestSecretIsNotStoredInPlaintext scans EVERY column of every row.
//
// ⚠ Checking only `token_hash` would pass against a future change that adds a
// `label` or `user_agent` column and puts the secret there. The assertion has to
// be about the whole row, not about the column the secret is supposed to be
// absent from.
func TestSecretIsNotStoredInPlaintext(t *testing.T) {
	h := newHarness(t)
	secret := h.create(t, base, base.Add(time.Hour))

	rows, err := h.db.Read.Query(`SELECT * FROM sessions`)
	if err != nil {
		t.Fatalf("select *: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	var scanned int
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			var s any
			cells[i] = &s
		}
		if err := rows.Scan(cells...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		scanned++
		for i, c := range cells {
			v := fmt.Sprintf("%v", *(c.(*any)))
			if strings.Contains(v, secret) {
				t.Errorf("column %q holds the session secret in plaintext — a read of this "+
					"table would let anyone impersonate the session", cols[i])
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no session rows were scanned, so this assertion proved nothing")
	}
}

func TestTwoSessionsGetDifferentSecrets(t *testing.T) {
	h := newHarness(t)
	a := h.create(t, base, base.Add(time.Hour))
	b := h.create(t, base, base.Add(time.Hour))

	if a == b {
		t.Fatal("two sessions were issued the same secret")
	}
	for _, s := range []string{a, b} {
		if _, err := h.store.Resolve(context.Background(), s, base); err != nil {
			t.Errorf("a session did not resolve: %v", err)
		}
	}
}

func TestExpiredSessionIsRefused(t *testing.T) {
	h := newHarness(t)
	expires := base.Add(time.Hour)
	secret := h.create(t, base, expires)

	// One second before: alive.
	if _, err := h.store.Resolve(context.Background(), secret, expires.Add(-time.Second)); err != nil {
		t.Errorf("a session one second before its deadline was refused: %v", err)
	}
	// At the deadline: gone. A session that lives one second past its stated
	// lifetime has a lifetime that is not what it says.
	if _, err := h.store.Resolve(context.Background(), secret, expires); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("a session AT its deadline resolved (err=%v)", err)
	}
	if _, err := h.store.Resolve(context.Background(), secret, expires.Add(time.Second)); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("an expired session resolved (err=%v)", err)
	}
}

// TestExpiryIsNotExtendedByUse is red against a sliding window — the friendlier
// and wrong behaviour.
func TestExpiryIsNotExtendedByUse(t *testing.T) {
	h := newHarness(t)
	expires := base.Add(time.Hour)
	secret := h.create(t, base, expires)

	// Used repeatedly right up to the deadline.
	for at := base; at.Before(expires); at = at.Add(5 * time.Minute) {
		if _, err := h.store.Resolve(context.Background(), secret, at); err != nil {
			t.Fatalf("resolve at %v failed: %v", at, err)
		}
	}

	if _, err := h.store.Resolve(context.Background(), secret, expires); !errors.Is(err, core.ErrNotFound) {
		t.Error("the session survived its original deadline after being used throughout. Expiry " +
			"is being extended on use, so a browser left open never logs out — which is how an " +
			"unlocked laptop becomes a permanent admin credential")
	}
}

func TestRevokedSessionIsRefused(t *testing.T) {
	h := newHarness(t)
	secret := h.create(t, base, base.Add(time.Hour))

	if _, err := h.store.Resolve(context.Background(), secret, base); err != nil {
		t.Fatalf("the session did not resolve before revocation: %v", err)
	}
	if err := h.store.Revoke(context.Background(), secret); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := h.store.Resolve(context.Background(), secret, base); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("a revoked session still resolves (err=%v) — logout is cosmetic", err)
	}
}

func TestRevokeAllForUser(t *testing.T) {
	h := newHarness(t)
	a := h.create(t, base, base.Add(time.Hour))
	b := h.create(t, base, base.Add(time.Hour))

	if err := h.store.RevokeAllForUser(context.Background(), h.userID); err != nil {
		t.Fatalf("RevokeAllForUser: %v", err)
	}
	for i, s := range []string{a, b} {
		if _, err := h.store.Resolve(context.Background(), s, base); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("session %d survived a bulk revoke", i)
		}
	}
}

// TestSweepRemovesExpiredAndRevoked asserts BOTH halves.
//
// A sweep that truncates the table passes "expired rows are gone" on its own.
// The live session surviving is what separates the two.
func TestSweepRemovesExpiredAndRevoked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	expired := h.create(t, base, base.Add(time.Minute))
	revoked := h.create(t, base, base.Add(24*time.Hour))
	live := h.create(t, base, base.Add(24*time.Hour))
	if err := h.store.Revoke(ctx, revoked); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	before, err := h.store.Len(ctx)
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if before != 3 {
		t.Fatalf("Len before sweep = %d, want 3 — the fixture is wrong", before)
	}

	n, err := h.store.SweepExpired(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if n != 2 {
		t.Errorf("swept %d rows, want 2 (one expired, one revoked)", n)
	}

	after, err := h.store.Len(ctx)
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if after != 1 {
		t.Errorf("Len after sweep = %d, want 1 — the table did not shrink to the live session",
			after)
	}
	if _, err := h.store.Resolve(ctx, live, base.Add(time.Hour)); err != nil {
		t.Errorf("the sweep deleted a LIVE session: %v. A sweep that clears everything passes "+
			"the shrink assertion above on its own", err)
	}
	_ = expired
}

func TestResolveUnknownSecret(t *testing.T) {
	h := newHarness(t)
	h.create(t, base, base.Add(time.Hour))

	for _, s := range []string{"", "never-issued", strings.Repeat("a", 43)} {
		if _, err := h.store.Resolve(context.Background(), s, base); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("secret %q resolved (err=%v)", s, err)
		}
	}
}

// TestSessionsTableHasATokenHashIndex — the resolve query runs on every request
// under /admin, and its cost is invisible until the table is large.
//
// ⚠ It asserts that AN index is used, not that a particular one exists. Writing
// it the other way is what this test caught: the migration originally carried an
// explicit `CREATE INDEX` on `token_hash` that the planner never chose, because
// `UNIQUE` already provides `sqlite_autoindex_sessions_2`. A redundant index is
// written on every insert and read by nothing, and only the query plan shows it.
func TestSessionsTableHasATokenHashIndex(t *testing.T) {
	h := newHarness(t)

	rows, err := h.db.Read.Query(
		`EXPLAIN QUERY PLAN SELECT id FROM sessions WHERE token_hash = ?`, "x")
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan.WriteString(detail + "\n")
	}
	got := plan.String()
	if got == "" {
		t.Fatal("the query planner returned nothing, so this assertion proved nothing")
	}
	if !strings.Contains(got, "USING INDEX") || !strings.Contains(got, "token_hash=?") {
		t.Errorf("resolving a session does not use an index on token_hash — every request under "+
			"/admin pays for a table scan:\n%s", got)
	}
	if strings.Contains(got, "SCAN sessions") {
		t.Errorf("the resolve query scans the sessions table:\n%s", got)
	}
}

func TestConcurrentResolveIsRaceFree(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	secrets := make([]string, 10)
	for i := range secrets {
		secrets[i] = h.create(t, base, base.Add(24*time.Hour))
	}

	stop := make(chan struct{})
	var sweeping sync.WaitGroup
	sweeping.Add(1)
	go func() {
		defer sweeping.Done()
		for {
			select {
			case <-stop:
				return
			default:
				// Sweeps at a time before every deadline, so nothing live is
				// removed and the final assertion stays meaningful.
				_, _ = h.store.SweepExpired(ctx, base)
			}
		}
	}()

	var wg sync.WaitGroup
	for g := range 20 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for range 20 {
				_, _ = h.store.Resolve(ctx, secrets[g%len(secrets)], base)
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	sweeping.Wait()

	// -race carries the verdict, but a test whose only failure mode is a
	// command-line flag goes green when someone drops the flag.
	n, err := h.store.Len(ctx)
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != len(secrets) {
		t.Errorf("Len = %d after concurrent use, want %d — live sessions were lost", n, len(secrets))
	}
}
