package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

// open is the helper every test uses. It returns the two handles and closes
// them on cleanup.
func open(t *testing.T) (*store.DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestOpenRunsMigration(t *testing.T) {
	db, _ := open(t)
	// Five tables, counted against the list in 00001_init.sql. If these two
	// disagree the migration is not what the schema comment says it is.
	want := []string{"users", "tokens", "jobs", "credit_entries", "service_rates"}
	for _, table := range want {
		var n int
		err := db.Read.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&n)
		if err != nil {
			t.Fatalf("querying for table %q: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %q: found %d, want 1 — migration did not create it", table, n)
		}
	}
}

func TestReadHandleRefusesWrite(t *testing.T) {
	db, _ := open(t)
	// The whole point of the read handle. If query_only(1) is not honoured by
	// this driver, "read models must not write" is a code-review rule instead of
	// an invariant the driver enforces on every test run.
	_, err := db.Read.Exec(
		`INSERT INTO users (id, email, role, created_at) VALUES ('x','x@example.com','client',0)`)
	if err == nil {
		t.Fatal("INSERT through the READ handle succeeded — _pragma=query_only(1) is NOT honoured")
	}
	t.Logf("read handle correctly refused the write: %v", err)
}

func TestReadHandleStillReadsUnderWAL(t *testing.T) {
	db, _ := open(t)
	// The reader DSN omits journal_mode because setting WAL requires a write.
	// This asserts that omission does not break reading a WAL file.
	if _, err := db.Write.Exec(
		`INSERT INTO users (id, email, role, credits, created_at)
		 VALUES ('u1','a@example.com','client',5,0)`); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	var credits int
	if err := db.Read.QueryRow(`SELECT credits FROM users WHERE id='u1'`).Scan(&credits); err != nil {
		t.Fatalf("read through the read handle: %v", err)
	}
	if credits != 5 {
		t.Errorf("credits = %d, want 5", credits)
	}
	var mode string
	if err := db.Read.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("reading journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q — the writer should have set it on the file", mode, "wal")
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	db, _ := open(t)
	// SQLite defaults foreign_keys OFF. A prior project in this workspace shipped
	// ON DELETE CASCADE clauses that never fired and leaked orphans silently, so
	// both halves are asserted: the reference is checked, and the cascade runs.
	_, err := db.Write.Exec(
		`INSERT INTO tokens (id, user_id, role, hash, created_at)
		 VALUES ('t1','nosuchuser','client','deadbeef',0)`)
	if err == nil {
		t.Fatal("token for a non-existent user was accepted — foreign_keys is OFF")
	}

	mustExec(t, db.Write, `INSERT INTO users (id,email,role,created_at) VALUES ('u1','a@b.c','client',0)`)
	mustExec(t, db.Write, `INSERT INTO tokens (id,user_id,role,hash,created_at) VALUES ('t1','u1','client','h1',0)`)
	mustExec(t, db.Write, `DELETE FROM users WHERE id='u1'`)

	var n int
	if err := db.Read.QueryRow(`SELECT count(*) FROM tokens WHERE id='t1'`).Scan(&n); err != nil {
		t.Fatalf("counting tokens: %v", err)
	}
	if n != 0 {
		t.Errorf("token survived its user's deletion (%d rows) — ON DELETE CASCADE did not fire", n)
	}
}

// TestTxlockImmediateBothArms proves _txlock=immediate is HONOURED rather than
// merely present in the DSN.
//
// A silently-ignored DSN parameter is indistinguishable from an honoured one, so
// asserting the string is in the DSN would assert nothing. Both arms run the
// same read-then-write contention: without the parameter it must produce
// failures, with it must produce none. If the two arms ever agree, the parameter
// is doing nothing and the concurrency story needs a different mechanism.
func TestTxlockImmediateBothArms(t *testing.T) {
	const (
		goroutines = 8
		iterations = 40
		// ⚠ MORE THAN ONE writer connection, in BOTH arms. With the production
		// setting of 1, database/sql serialises every transaction through one
		// connection, no two read-then-write transactions ever overlap, and the
		// deferred arm records zero failures — so both arms agree and the probe
		// proves nothing while looking like a clean result. Measured here on
		// 2026-09-15: writeConns=1 gave deferred=0 immediate=0 of 320.
		writeConns = 4
	)

	run := func(t *testing.T, immediate bool) int {
		dir := t.TempDir()
		path := filepath.Join(dir, "contend.db")
		db, err := store.OpenForTest(path, immediate, writeConns)
		if err != nil {
			t.Fatalf("open (immediate=%v): %v", immediate, err)
		}
		defer db.Close()
		mustExec(t, db.Write, `INSERT INTO users (id,email,role,credits,created_at) VALUES ('u1','a@b.c','client',0,0)`)

		var (
			mu       sync.Mutex
			failures int
			wg       sync.WaitGroup
		)
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					if err := readThenWrite(db.Write); err != nil {
						mu.Lock()
						failures++
						mu.Unlock()
					}
				}
			}()
		}
		wg.Wait()
		return failures
	}

	deferred := run(t, false)
	immediate := run(t, true)

	t.Logf("read-then-write failures: deferred=%d immediate=%d (of %d)",
		deferred, immediate, goroutines*iterations)

	if immediate != 0 {
		t.Errorf("with _txlock=immediate: %d failures, want 0", immediate)
	}
	if deferred == 0 {
		t.Errorf("without _txlock=immediate: 0 failures — the two arms agree, so the "+
			"parameter is not doing anything and this test cannot detect whether it is honoured "+
			"(immediate=%d)", immediate)
	}
}

// readThenWrite is the transaction shape that upgrades a shared lock to an
// exclusive one mid-transaction, which is exactly what BEGIN IMMEDIATE avoids.
func readThenWrite(db *sql.DB) error {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var credits int
	if err := tx.QueryRow(`SELECT credits FROM users WHERE id='u1'`).Scan(&credits); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE users SET credits=? WHERE id='u1'`, credits+1); err != nil {
		return err
	}
	return tx.Commit()
}

func TestGooseDialectMatchesDriver(t *testing.T) {
	// The driver registers as "sqlite" while goose's dialect is "sqlite3". The
	// two names differ and a mismatch surfaces only at startup, so a successful
	// Open IS the assertion.
	db, path := open(t)
	if db.Read == nil || db.Write == nil {
		t.Fatal("Open returned a nil handle")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file was not created at %s: %v", path, err)
	}
	var version int64
	if err := db.Read.QueryRow(`SELECT max(version_id) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("goose version table missing — migrations did not run through goose: %v", err)
	}
	if version < 1 {
		t.Errorf("goose version = %d, want >= 1", version)
	}
}

func TestOpenIsIdempotentAcrossRestart(t *testing.T) {
	// Boot recovery reopens the same file. Running the migration twice must be a
	// no-op rather than an error, or every restart fails.
	dir := t.TempDir()
	path := filepath.Join(dir, "restart.db")

	db1, err := store.Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	mustExec(t, db1.Write, `INSERT INTO users (id,email,role,created_at) VALUES ('u1','a@b.c','client',0)`)
	if err := db1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("second open on the same file: %v", err)
	}
	defer db2.Close()

	var n int
	if err := db2.Read.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if n != 1 {
		t.Errorf("rows after reopen = %d, want 1 — data did not survive", n)
	}
}

// TestMigrationAddsColumnToAnExistingDatabase is the only test that exercises
// the risk `ALTER TABLE ADD COLUMN` actually carries.
//
// ⚠ A migration test against a FRESH database proves nothing here: the whole
// question is what happens to rows that already exist. This builds a database at
// schema 00001, puts real rows in it, stamps goose's version table so 00001 is
// already applied, and only then opens it normally — which is the moment 00002
// runs against live data, exactly as it will in production.
//
// It is red if the column is added `NOT NULL` without a default, which is the
// spelling that looks stricter and fails on every existing row.
func TestMigrationAddsColumnToAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")

	// 1. Build the database at schema 00001, reading the real migration rather
	//    than a copy of it — a hand-written copy would drift and the test would
	//    then be asserting against a schema nobody ships.
	first, err := os.ReadFile(filepath.Join("migrations", "00001_init.sql"))
	if err != nil {
		t.Fatalf("reading 00001: %v", err)
	}
	up := upSection(t, string(first))

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening raw: %v", err)
	}
	if _, err := raw.Exec(up); err != nil {
		t.Fatalf("applying 00001: %v", err)
	}
	// Rows that predate the new column.
	if _, err := raw.Exec(
		`INSERT INTO users (id,email,role,credits,created_at) VALUES
		 ('u1','a@example.com','client',7,100),
		 ('u2','b@example.com','admin',0,200)`); err != nil {
		t.Fatalf("seeding users: %v", err)
	}
	// Stamp goose so it believes 00001 is applied and only 00002 remains.
	if _, err := raw.Exec(`CREATE TABLE goose_db_version (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		version_id INTEGER NOT NULL,
		is_applied INTEGER NOT NULL,
		tstamp TIMESTAMP DEFAULT (datetime('now')))`); err != nil {
		t.Fatalf("creating goose table: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO goose_db_version (version_id,is_applied) VALUES (0,1),(1,1)`); err != nil {
		t.Fatalf("stamping goose: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("closing raw: %v", err)
	}

	// 2. Open normally. This is where 00002 runs against the rows above.
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("opening an existing 00001 database: %v — the migration cannot be applied to a "+
			"database that already has rows, which is every real deployment", err)
	}
	defer db.Close()

	// 3. Every row survived, with an empty hash.
	rows, err := db.Read.Query(`SELECT id, credits, password_hash FROM users ORDER BY id`)
	if err != nil {
		t.Fatalf("reading after migration: %v", err)
	}
	defer rows.Close()

	type got struct {
		id      string
		credits int
		hash    string
	}
	var all []got
	for rows.Next() {
		var g got
		if err := rows.Scan(&g.id, &g.credits, &g.hash); err != nil {
			t.Fatalf("scan: %v", err)
		}
		all = append(all, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(all) != 2 {
		t.Fatalf("got %d users after migration, want 2 — rows were lost", len(all))
	}
	if all[0].credits != 7 {
		t.Errorf("u1 credits = %d, want 7 — existing data did not survive", all[0].credits)
	}
	for _, g := range all {
		if g.hash != "" {
			t.Errorf("%s has password_hash %q after migration, want empty — an existing account "+
				"must not silently acquire a credential", g.id, g.hash)
		}
	}

	// 4. And the sessions table arrived in the same migration.
	var name string
	if err := db.Read.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='sessions'`).Scan(&name); err != nil {
		t.Errorf("sessions table missing after migration: %v", err)
	}
}

// upSection returns the statements between the first goose Up markers.
func upSection(t *testing.T, migration string) string {
	t.Helper()
	const begin, end = "-- +goose StatementBegin", "-- +goose StatementEnd"
	i := strings.Index(migration, begin)
	j := strings.Index(migration, end)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("migration has no Up statement block, so this test would apply nothing")
	}
	return migration[i+len(begin) : j]
}

func TestWriterIsSerialised(t *testing.T) {
	db, _ := open(t)
	if got := db.Write.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("writer MaxOpenConnections = %d, want 1 — more than one writer connection "+
			"reintroduces the contention _txlock and the single-writer rule exist to remove", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := db.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Errorf("second close returned %v, want nil or ErrConnDone", err)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

var _ = time.Now
