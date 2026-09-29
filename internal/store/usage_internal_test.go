package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/core"
)

// usageFixture is a repo plus a raw handle, because seeding a DELIVERED, DEAD or
// EXPIRED job at a chosen timestamp needs SQL.
//
// ⚠ This file is `package store` for that reason and no other: CreateJob
// hardcodes `state = queued`, and the only alternatives are to drive the real
// claim/complete/deliver sequence for every fixture row — which tests the
// transitions, not the counting — or to export a SetJobState that production has
// no caller for. migrate_internal_test.go records the same reasoning for goose.
type usageFixture struct {
	repo *Repo
	db   *DB
}

func newUsageFixture(t *testing.T) *usageFixture {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &usageFixture{repo: NewRepo(db), db: db}
}

// user adds a customer. The counters are per user_id and jobs reference users, so
// a job cannot be seeded without one.
func (f *usageFixture) user(t *testing.T, id string) {
	t.Helper()
	err := f.repo.CreateUser(context.Background(), core.User{
		ID: id, Email: id + "@example.com", Role: core.RoleClient,
		Credits: 1000, BufferLimit: 100, Active: true, CreatedAt: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", id, err)
	}
}

// job seeds one job owned by userID, created at `at`, in `state`.
func (f *usageFixture) job(t *testing.T, id, userID string, at time.Time, state core.JobState) {
	t.Helper()
	_, err := f.db.Write.Exec(
		`INSERT INTO jobs (id, user_id, filename, size_byte, label, pipeline, stage, params,
		                   has_blob, raw, state, attempts, units, accrued_credits, worker_id,
		                   lease_expires_at, last_error, queued_at, expires_at, created_at, updated_at)
		 VALUES (?,?,'f.pdf',10,'ocr','["ocr"]',0,'{}',1,0,?,0,0,0,'',NULL,'',?,NULL,?,?)`,
		id, userID, string(state), at.Unix(), at.Unix(), at.Unix())
	if err != nil {
		t.Fatalf("seeding job %s: %v", id, err)
	}
}

// sep is a fixed `now` in September, so "the previous calendar month" is August
// and the assertions do not move with the wall clock.
var sep = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

// TestUsageCountsOnlyTheWindowAndOnlyTheOwner is ADR-0010's central claim.
//
// ⚠ TWO customers, with jobs at the same instants. A single-customer fixture
// cannot see a missing GROUP BY or a wrong join: every number would be right by
// accident because there is nothing else to mix in.
func TestUsageCountsOnlyTheWindowAndOnlyTheOwner(t *testing.T) {
	f := newUsageFixture(t)
	f.user(t, "a")
	f.user(t, "b")

	// a: two jobs inside 24h, one 3 days old, one 40 days old.
	f.job(t, "a1", "a", sep.Add(-1*time.Hour), core.JobDelivered)
	f.job(t, "a2", "a", sep.Add(-2*time.Hour), core.JobDead)
	f.job(t, "a3", "a", sep.Add(-3*24*time.Hour), core.JobDelivered)
	f.job(t, "a4", "a", sep.Add(-40*24*time.Hour), core.JobDelivered)
	// b: one job inside 24h, at the same hour as one of a's.
	f.job(t, "b1", "b", sep.Add(-1*time.Hour), core.JobDelivered)

	got, err := f.repo.UsageByUser(context.Background(), sep)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}

	a, ok := got["a"]
	if !ok {
		t.Fatal("no usage for customer a")
	}
	if a.Day.Pushed != 2 {
		t.Errorf("a 24h pushed = %d, want 2", a.Day.Pushed)
	}
	if a.Day.Delivered != 1 || a.Day.Failed != 1 {
		t.Errorf("a 24h delivered/failed = %d/%d, want 1/1", a.Day.Delivered, a.Day.Failed)
	}
	if a.Week.Pushed != 3 {
		t.Errorf("a 7d pushed = %d, want 3", a.Week.Pushed)
	}
	if a.Month.Pushed != 3 {
		t.Errorf("a 31d pushed = %d, want 3 — the 40-day-old job is outside it", a.Month.Pushed)
	}

	b, ok := got["b"]
	if !ok {
		t.Fatal("no usage for customer b")
	}
	if b.Day.Pushed != 1 {
		t.Errorf("b 24h pushed = %d, want 1 — a's jobs are being counted against b", b.Day.Pushed)
	}
}

// TestUsageWindowFloorsAreInclusive pins the off-by-one this shape invites.
func TestUsageWindowFloorsAreInclusive(t *testing.T) {
	f := newUsageFixture(t)
	f.user(t, "a")

	// Exactly on each floor, and one second older than each.
	f.job(t, "on24", "a", sep.Add(-24*time.Hour), core.JobDelivered)
	f.job(t, "off24", "a", sep.Add(-24*time.Hour-time.Second), core.JobDelivered)
	f.job(t, "on7", "a", sep.Add(-7*24*time.Hour), core.JobDelivered)
	f.job(t, "off7", "a", sep.Add(-7*24*time.Hour-time.Second), core.JobDelivered)
	f.job(t, "on31", "a", sep.Add(-31*24*time.Hour), core.JobDelivered)
	f.job(t, "off31", "a", sep.Add(-31*24*time.Hour-time.Second), core.JobDelivered)

	got, err := f.repo.UsageByUser(context.Background(), sep)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	a := got["a"]

	if a.Day.Pushed != 1 {
		t.Errorf("24h pushed = %d, want 1 — the floor must include the instant itself and "+
			"exclude one second before it", a.Day.Pushed)
	}
	if a.Week.Pushed != 3 {
		t.Errorf("7d pushed = %d, want 3 (on24, off24, on7)", a.Week.Pushed)
	}
	if a.Month.Pushed != 5 {
		t.Errorf("31d pushed = %d, want 5 — everything but off31", a.Month.Pushed)
	}
}

// TestUsagePreviousMonthIsTheCompletedMonth pins the call the ADR marks ★: the
// month column is the PREVIOUS complete calendar month, not the current one.
func TestUsagePreviousMonthIsTheCompletedMonth(t *testing.T) {
	f := newUsageFixture(t)
	f.user(t, "a")

	f.job(t, "aug1", "a", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), core.JobDelivered)
	f.job(t, "aug2", "a", time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC), core.JobDead)
	f.job(t, "sep1", "a", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), core.JobDelivered)
	f.job(t, "jul1", "a", time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC), core.JobDelivered)

	got, err := f.repo.UsageByUser(context.Background(), sep)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	a := got["a"]

	if a.PrevMonth.Pushed != 2 {
		t.Errorf("previous-month pushed = %d, want 2 — August only, so neither the September job "+
			"nor the July one", a.PrevMonth.Pushed)
	}
	if a.PrevMonth.Delivered != 1 || a.PrevMonth.Failed != 1 {
		t.Errorf("previous-month delivered/failed = %d/%d, want 1/1",
			a.PrevMonth.Delivered, a.PrevMonth.Failed)
	}
}

// TestUsagePreviousMonthCrossesTheYear is red for any implementation that
// subtracts days, or that forgets the year when it subtracts a month.
func TestUsagePreviousMonthCrossesTheYear(t *testing.T) {
	f := newUsageFixture(t)
	f.user(t, "a")

	jan := time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC)
	f.job(t, "dec", "a", time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC), core.JobDelivered)
	f.job(t, "jan", "a", time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC), core.JobDelivered)
	f.job(t, "nov", "a", time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), core.JobDelivered)

	got, err := f.repo.UsageByUser(context.Background(), jan)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	a := got["a"]

	if a.PrevMonth.Pushed != 1 {
		t.Errorf("previous-month pushed = %d, want 1 — December 2026, the month before a January "+
			"2027 `now`. A day-subtracting implementation lands in the wrong month and a "+
			"year-forgetting one lands in December 2027", a.PrevMonth.Pushed)
	}
}

// TestUsagePreviousMonthHandlesAShortMonth is the test a SURVIVED MUTANT asked
// for, and it is worth recording why it was missing.
//
// The first two month tests used August and December. Both have 31 days, so
// `firstOfThisMonth.AddDate(0, 0, -31)` lands exactly on the first of the
// previous month and a day-subtracting implementation passes both — the fixture
// could not produce the failure, however the assertions were worded. February can:
// 1 March minus 31 days is 29 January, so a day-subtracting boundary pulls two
// days of JANUARY jobs into the February column.
func TestUsagePreviousMonthHandlesAShortMonth(t *testing.T) {
	f := newUsageFixture(t)
	f.user(t, "a")

	mar := time.Date(2027, 3, 10, 12, 0, 0, 0, time.UTC)
	// Late January — outside February however you count days.
	f.job(t, "jan30", "a", time.Date(2027, 1, 30, 0, 0, 0, 0, time.UTC), core.JobDelivered)
	f.job(t, "jan31", "a", time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), core.JobDelivered)
	// February itself.
	f.job(t, "feb", "a", time.Date(2027, 2, 14, 0, 0, 0, 0, time.UTC), core.JobDelivered)

	got, err := f.repo.UsageByUser(context.Background(), mar)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	a := got["a"]

	if a.PrevMonth.Pushed != 1 {
		t.Errorf("previous-month pushed = %d, want 1 — February 2027 only. A boundary that "+
			"subtracts 31 days from 1 March starts on 29 January and counts the two January "+
			"jobs as February's", a.PrevMonth.Pushed)
	}
}

// TestUsageBucketsReconcile keeps the four numbers a coherent set: the cohort is
// what was pushed, and what is not yet terminal is still in flight.
//
// The in-flight job makes the inequality STRICT — without it the assertion would
// pass on an implementation that simply counted everything twice.
func TestUsageBucketsReconcile(t *testing.T) {
	f := newUsageFixture(t)
	f.user(t, "a")

	f.job(t, "d", "a", sep.Add(-time.Hour), core.JobDelivered)
	f.job(t, "f", "a", sep.Add(-time.Hour), core.JobDead)
	f.job(t, "e", "a", sep.Add(-time.Hour), core.JobExpired)
	f.job(t, "q", "a", sep.Add(-time.Hour), core.JobQueued) // still in flight

	got, err := f.repo.UsageByUser(context.Background(), sep)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	a := got["a"]

	if a.Day.Pushed != 4 {
		t.Fatalf("pushed = %d, want 4 — the fixture itself is wrong", a.Day.Pushed)
	}
	terminal := a.Day.Delivered + a.Day.Failed + a.Day.Expired
	if terminal != 3 {
		t.Errorf("delivered+failed+expired = %d, want 3", terminal)
	}
	if terminal >= a.Day.Pushed {
		t.Errorf("terminal (%d) is not less than pushed (%d) — the queued job has vanished from "+
			"the cohort, so `pushed − the rest` stops meaning in flight", terminal, a.Day.Pushed)
	}
}

// TestUsageSeparatesFailedFromExpired keeps apart the two an operator acts on
// differently: a command that failed, and a service nobody served.
func TestUsageSeparatesFailedFromExpired(t *testing.T) {
	f := newUsageFixture(t)
	f.user(t, "a")

	f.job(t, "dead", "a", sep.Add(-time.Hour), core.JobDead)
	f.job(t, "exp", "a", sep.Add(-time.Hour), core.JobExpired)

	got, err := f.repo.UsageByUser(context.Background(), sep)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	a := got["a"]

	if a.Day.Failed != 1 {
		t.Errorf("failed = %d, want 1 (the dead job only)", a.Day.Failed)
	}
	if a.Day.Expired != 1 {
		t.Errorf("expired = %d, want 1 (the expired job only)", a.Day.Expired)
	}
}

// TestUsageIsOneStatement stops the cost silently becoming 16 × customers.
//
// ⚠ IT READS THE SOURCE, NOT A RUNTIME COUNTER, and that is a weaker claim than
// it looks: it proves the SHAPE is one query and proves nothing about what SQLite
// executed. This driver exposes no per-connection statement count, which is the
// fallback ADR-0010 T1's Risks section pre-registered rather than discovered.
//
// It is still worth having: the defect it guards is a loop over customers, or one
// query per window, and both are visible in the source as extra call sites.
func TestUsageIsOneStatement(t *testing.T) {
	src, err := os.ReadFile("usage.go")
	if err != nil {
		t.Fatalf("reading usage.go: %v", err)
	}
	text := string(src)

	var calls int
	for _, fn := range []string{"QueryContext", "QueryRowContext", "ExecContext"} {
		calls += strings.Count(text, fn+"(")
	}
	if calls != 1 {
		t.Errorf("internal/store/usage.go makes %d database calls, want exactly 1 — four windows "+
			"× four buckets × N customers as separate counts is what this record's cost estimate "+
			"rules out", calls)
	}
}
