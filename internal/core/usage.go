package core

// Window is one customer's job counts over one period of time.
//
// ⚠ EVERY COUNT IS KEYED ON THE JOB'S created_at, INCLUDING THE TERMINAL ONES,
// and that is the rule to know before reading any of these numbers (ADR-0010).
// A Window describes a COHORT: of the jobs this customer PUSHED in the period,
// how many have since ended delivered, failed or expired. It does not describe
// what finished during the period.
//
// The consequence is the useful part: the four numbers reconcile, and
//
//	Pushed - Delivered - Failed - Expired
//
// is how many of that cohort are still in flight. Bucketing the terminal states
// by updated_at instead would answer a different question and break that — a job
// pushed on Monday and delivered on Tuesday would land in two different periods,
// and the subtraction would produce nonsense.
type Window struct {
	// Pushed is every job row the customer created in the period, whatever
	// became of it.
	Pushed int
	// Delivered is the cohort's collected-and-charged jobs.
	Delivered int
	// Failed is the cohort's DEAD jobs: the worker's command ran and exhausted
	// its attempts.
	Failed int
	// Expired is the cohort's EXPIRED jobs: the deadline passed while the job
	// was still queued, so nothing ever ran.
	//
	// ⚠ Kept apart from Failed deliberately. An operator acts on them
	// differently — a failing command is a broken service, an expired queue is a
	// service nobody served — and merging them hides which one is happening.
	Expired int
}

// Usage is one customer's counts over the four periods the dashboard shows.
//
// The rolling windows end at the caller's `now`; PrevMonth is the previous
// COMPLETE calendar month in UTC — August, when now is in September. Rolling 31
// days already answers "roughly the last month", so the column that earns its
// place beside it is the completed one an invoice matches.
type Usage struct {
	// Day, Week and Month are the rolling 24-hour, 7-day and 31-day periods.
	Day   Window
	Week  Window
	Month Window
	// PrevMonth is the previous complete calendar month, UTC.
	PrevMonth Window
}
