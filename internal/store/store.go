// Package store owns every SQL statement in the router, and the two handles
// they run on.
//
// The two-handle split is the CQRS invariant made physical: reads go through a
// connection opened with query_only(1), so a read model that tries to write
// fails at the driver rather than at code review. Both handles address the same
// file.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"

	"github.com/pressly/goose/v3"

	// Registers the CGO-free driver under the name "sqlite".
	//
	// ⚠ The driver name and goose's dialect name are DIFFERENT strings —
	// "sqlite" here, "sqlite3" in SetDialect below — and a mismatch surfaces
	// only when Open is first called. Changing either without the other is a
	// startup failure, not a compile error.
	_ "modernc.org/sqlite"
)

//go:embed all:migrations
var migrationsFS embed.FS

// driverName is what modernc.org/sqlite registers itself as.
const driverName = "sqlite"

// gooseDialect is what goose calls the same database. Not a typo: see the
// import comment above.
const gooseDialect = "sqlite3"

// DB holds the router's two handles on one SQLite file.
//
// Read is a pool and is physically incapable of writing. Write is a single
// serialised connection. Nothing in the codebase should hold a third handle.
type DB struct {
	// Read is query_only. Use it for everything that does not mutate.
	Read *sql.DB
	// Write is the only handle that may mutate, limited to one connection.
	Write *sql.DB
}

// Open opens the database, applies pragmas to both handles, and runs migrations.
//
// It is safe to call on an existing file: goose skips migrations already
// applied, which is what makes a restart — and therefore boot recovery — work.
func Open(path string) (*DB, error) {
	// One writer connection, always. SQLite permits exactly one writer at a time
	// anyway; making that explicit turns lock contention into queueing inside
	// database/sql, which is cheaper and far easier to reason about. It also
	// means _txlock=immediate is belt-and-braces IN THIS PROCESS — it earns its
	// place against a second process touching the same file (a backup, a
	// migration tool, the sqlite3 CLI), which the pool cannot serialise.
	return OpenForTest(path, true, 1)
}

// OpenForTest is Open with the two knobs that make the _txlock probe possible.
//
// It exists for exactly one test: proving that _txlock=immediate is HONOURED by
// this driver rather than silently ignored. A silently-ignored DSN parameter is
// indistinguishable from an honoured one, so the only proof is a behaviour
// difference between two arms.
//
// ⚠ writeConns is the half that is easy to get wrong, and getting it wrong makes
// the probe vacuous. With writeConns == 1 — the production setting — database/sql
// serialises every transaction through one connection, so two read-then-write
// transactions NEVER overlap and the deferred arm records zero failures. Both
// arms then agree and the test reads as "the parameter does nothing", when in
// fact the fixture could not produce the contention in the first place. The
// probe must pass writeConns > 1 to create real overlap.
//
// No production caller should use this: Open pins immediate=true and one
// connection.
func OpenForTest(path string, immediate bool, writeConns int) (*DB, error) {
	write, err := sql.Open(driverName, writeDSN(path, immediate))
	if err != nil {
		return nil, fmt.Errorf("opening write handle: %w", err)
	}
	write.SetMaxOpenConns(writeConns)

	if err := migrate(write); err != nil {
		_ = write.Close()
		return nil, err
	}

	read, err := sql.Open(driverName, readDSN(path))
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("opening read handle: %w", err)
	}

	// Force a real connection now so a bad DSN fails here rather than on the
	// first request. sql.Open is lazy and would otherwise defer the error.
	if err := read.Ping(); err != nil {
		_ = write.Close()
		_ = read.Close()
		return nil, fmt.Errorf("pinging read handle: %w", err)
	}

	return &DB{Read: read, Write: write}, nil
}

// writeDSN builds the writer's connection string.
//
// journal_mode(WAL) lets readers proceed during a write; busy_timeout stops a
// momentary lock becoming an immediate error; foreign_keys(1) is set EXPLICITLY
// because SQLite defaults it OFF and the schema's ON DELETE CASCADE clauses read
// as though they would fire when they would not.
func writeDSN(path string, immediate bool) string {
	dsn := path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"
	if immediate {
		// BEGIN IMMEDIATE takes the write lock up front, so a transaction that
		// reads and then writes cannot fail on a lock upgrade partway through.
		dsn += "&_txlock=immediate"
	}
	return dsn
}

// readDSN builds the reader's connection string.
//
// It omits journal_mode deliberately: setting WAL requires a write, which this
// handle cannot perform. Journal mode is a property of the FILE and the writer
// has already set it, so the reader inherits it.
func readDSN(path string) string {
	return path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=query_only(1)"
}

// migrate applies the embedded migrations through the write handle.
func migrate(write *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect(gooseDialect); err != nil {
		return fmt.Errorf("goose dialect %q (driver is %q): %w", gooseDialect, driverName, err)
	}
	if err := goose.Up(write, "migrations"); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

// Close closes both handles. It is safe to call more than once.
func (d *DB) Close() error {
	var errs []error
	if d.Read != nil {
		if err := d.Read.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if d.Write != nil {
		if err := d.Write.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// escape is unused today but kept next to the DSN builders as a reminder that a
// path containing a query character would corrupt them.
var _ = url.QueryEscape
