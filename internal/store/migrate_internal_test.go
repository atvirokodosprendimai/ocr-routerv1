package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestMigrationDownDropsRawColumns makes ADR-0006's Rollback step 1 executable
// rather than asserted.
//
// It lives in `package store` because goose and the embedded migration FS are
// both unexported, and the alternative — exporting a MigrateDown for a test to
// call — would widen production surface for a test's benefit.
//
// The claim under test is narrow and worth stating: a database migrated DOWN to
// 00002 must present the schema the pre-ADR-0006 code opens. SQLite can only
// drop a column when nothing depends on it, so a Down that "looks" reversible
// can still fail on a real file, and a rollback promise nobody has run is a
// promise nobody should rely on.
func TestMigrationDownDropsRawColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "down.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if !hasColumn(t, db.Write, "service_rates", "raw") {
		t.Fatal("service_rates.raw is absent after migrating up — 00003 did not apply")
	}
	if !hasColumn(t, db.Write, "jobs", "raw") {
		t.Fatal("jobs.raw is absent after migrating up — 00003 did not apply")
	}

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect(gooseDialect); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	// ⚠ DownTo a NAMED version, not Down. `Down` rolls back exactly one
	// migration, so a test written against "the latest" silently changes subject
	// the moment another migration is added — which is what happened when
	// ADR-0007 added 00004 and this test started asserting the wrong rollback.
	if err := goose.DownTo(db.Write, "migrations", 2); err != nil {
		t.Fatalf("goose.DownTo(2): %v — ADR-0006's Rollback step 1 claims this works", err)
	}

	if hasColumn(t, db.Write, "service_rates", "raw") {
		t.Error("service_rates.raw survived the down migration")
	}
	if hasColumn(t, db.Write, "jobs", "raw") {
		t.Error("jobs.raw survived the down migration")
	}
}

// TestExistingServiceRowIsNotPromotedToRaw proves the column DEFAULT, which is
// the safety property the whole of ADR-0006 rests on.
//
// ⚠ This test exists because a mutation showed the obvious one does not cover
// it. Flipping 00003's `DEFAULT 0` to `DEFAULT 1` left the suite green: the
// unconfigured case returns false from the ErrNoRows branch without touching the
// column, and the configured case has SetRate writing 0 explicitly. The default
// only ever applies to a row that ALREADY EXISTS when the migration runs — the
// shape every real deployment has and no test had.
//
// Getting that shape means migrating DOWN, writing a row under the old schema,
// and migrating back UP. There is no other way to produce it.
func TestExistingServiceRowIsNotPromotedToRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect(gooseDialect); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}

	// Back to the pre-ADR-0006 schema.
	// Down to the pre-ADR-0006 schema by VERSION, for the same reason as above.
	if err := goose.DownTo(db.Write, "migrations", 2); err != nil {
		t.Fatalf("goose.DownTo(2): %v", err)
	}
	if _, err := db.Write.Exec(
		`INSERT INTO service_rates (label, credits_per_unit, updated_at) VALUES (?,?,?)`,
		"legacy-paid", 5, 0); err != nil {
		t.Fatalf("seeding a pre-migration row: %v", err)
	}

	// Forward again: 00003 adds the column to a table that already has a row.
	if err := goose.Up(db.Write, "migrations"); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}

	rate, raw, err := NewRepo(db).ServiceMode(context.Background(), "legacy-paid")
	if err != nil {
		t.Fatalf("ServiceMode: %v", err)
	}
	if raw {
		t.Error("a service that existed before the migration came back RAW — every already-deployed " +
			"paid service would silently reprice from len(units)*rate to a flat 1 credit")
	}
	if rate != 5 {
		t.Errorf("rate = %d, want 5 — the migration disturbed an existing price", rate)
	}
}

// TestMigrationDownDropsUnmetered makes ADR-0009's Rollback executable rather
// than asserted, on the same reasoning as TestMigrationDownDropsRawColumns
// above: SQLite can only drop a column when nothing depends on it, so a Down
// that "looks" reversible can still fail on a real file.
func TestMigrationDownDropsUnmetered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "down9.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if !hasColumn(t, db.Write, "users", "unmetered") {
		t.Fatal("users.unmetered is absent after migrating up — 00006 did not apply")
	}

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect(gooseDialect); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	// DownTo a NAMED version, for the reason the sibling test above records: a
	// bare `Down` silently changes subject the next time a migration is added.
	if err := goose.DownTo(db.Write, "migrations", 5); err != nil {
		t.Fatalf("goose.DownTo(5): %v — ADR-0009's Rollback claims this works", err)
	}
	if hasColumn(t, db.Write, "users", "unmetered") {
		t.Error("users.unmetered survived the down migration")
	}
}

// TestAnExistingUserIsNotPromotedToUnmetered proves the column DEFAULT, which is
// the safety property ADR-0009 rests on: every customer already in the database
// must come back METERED.
//
// ⚠ It uses the down → insert → up shape because the OBVIOUS test cannot cover
// this, and ADR-0006 learned that the expensive way — flipping its `DEFAULT 0`
// to `DEFAULT 1` left the whole suite green. A default only ever applies to a
// row that ALREADY EXISTS when the migration runs, which is the shape every real
// deployment has and no ordinary test produces: CreateUser writes the column
// explicitly, so it never consults the default at all.
func TestAnExistingUserIsNotPromotedToUnmetered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy9.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect(gooseDialect); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}

	// Back to the pre-ADR-0009 schema, by version.
	if err := goose.DownTo(db.Write, "migrations", 5); err != nil {
		t.Fatalf("goose.DownTo(5): %v", err)
	}
	if _, err := db.Write.Exec(
		`INSERT INTO users (id, email, role, credits, buffer_limit, priority, job_ttl_secs, active, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		"legacy-user", "legacy@example.com", "client", 100, 4, 0, 0, 1, 0); err != nil {
		t.Fatalf("seeding a pre-migration user: %v", err)
	}

	// Forward again: 00006 adds the column to a table that already has a row.
	if err := goose.Up(db.Write, "migrations"); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}

	u, err := NewRepo(db).UserByID(context.Background(), "legacy-user")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if u.Unmetered {
		t.Error("a customer that existed before the migration came back UNMETERED — every " +
			"already-deployed customer would stop being billed, and nothing would report it")
	}
	if u.Credits != 100 {
		t.Errorf("credits = %d, want 100 — the migration disturbed an existing balance", u.Credits)
	}
}

// hasColumn asks SQLite's own schema rather than parsing the migration text.
// A test that greps the .sql file proves what was written, never what applied.
func hasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return false
}
