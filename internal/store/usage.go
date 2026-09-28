package store

import (
	"context"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// previousMonth returns the first instant of the previous complete calendar month
// and the first instant of the current one, both UTC.
//
// ⚠ IT SUBTRACTS A MONTH, NOT DAYS, and the difference is not pedantry. Taking
// `now.AddDate(0, 0, -31)` lands in the wrong month for every month that is not
// 31 days long, and taking `now.Add(-31*24*time.Hour)` does the same while also
// ignoring that months have different lengths. `time.Date` with `m-1` normalises
// on its own: month 0 is December of the previous YEAR, which is the case a
// hand-rolled subtraction forgets.
//
// UTC because every timestamp in this schema is unix seconds and the system
// carries no operator timezone. Introducing one for this single boundary would
// make one number disagree with every other date on the dashboard.
func previousMonth(now time.Time) (start, end time.Time) {
	utc := now.UTC()
	end = time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
	start = time.Date(utc.Year(), utc.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	return start, end
}

// UsageByUser counts every customer's jobs over the four windows the dashboard
// shows, keyed by user id.
//
// ⚠ ONE STATEMENT, WHATEVER THE CUSTOMER COUNT, and that is a cost requirement
// rather than a style choice: this runs on the SSE push path, so four windows ×
// four buckets × N customers as separate counts would be 16N statements every
// fifteen seconds. Conditional sums inside one GROUP BY make it a single pass,
// and idx_jobs_created_at bounds that pass to the oldest window.
//
// ⚠ THE SCAN FLOOR IS THE OLDEST OF THE FOUR WINDOWS, which is the previous
// month's start and NOT `now-31d`. In the first days of a month those are weeks
// apart, and using the 31-day floor would silently truncate the previous-month
// column — the numbers would look plausible and be wrong.
//
// Every count keys on created_at, so a window is a cohort; see core.Window for
// why, and for what the four numbers then reconcile to.
func (r *Repo) UsageByUser(ctx context.Context, now time.Time) (map[string]core.Usage, error) {
	day := now.Add(-24 * time.Hour).Unix()
	week := now.Add(-7 * 24 * time.Hour).Unix()
	month := now.Add(-31 * 24 * time.Hour).Unix()
	prevStart, prevEnd := previousMonth(now)

	floor := min(min(day, week), min(month, prevStart.Unix()))

	// Read handle: this is a read, and a counter must never be able to write.
	rows, err := r.read.QueryContext(ctx,
		`SELECT user_id,
		        SUM(created_at >= ?1),
		        SUM(created_at >= ?1 AND state = 'delivered'),
		        SUM(created_at >= ?1 AND state = 'dead'),
		        SUM(created_at >= ?1 AND state = 'expired'),
		        SUM(created_at >= ?2),
		        SUM(created_at >= ?2 AND state = 'delivered'),
		        SUM(created_at >= ?2 AND state = 'dead'),
		        SUM(created_at >= ?2 AND state = 'expired'),
		        SUM(created_at >= ?3),
		        SUM(created_at >= ?3 AND state = 'delivered'),
		        SUM(created_at >= ?3 AND state = 'dead'),
		        SUM(created_at >= ?3 AND state = 'expired'),
		        SUM(created_at >= ?4 AND created_at < ?5),
		        SUM(created_at >= ?4 AND created_at < ?5 AND state = 'delivered'),
		        SUM(created_at >= ?4 AND created_at < ?5 AND state = 'dead'),
		        SUM(created_at >= ?4 AND created_at < ?5 AND state = 'expired')
		 FROM jobs
		 WHERE created_at >= ?6
		 GROUP BY user_id`,
		day, week, month, prevStart.Unix(), prevEnd.Unix(), floor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]core.Usage{}
	for rows.Next() {
		var (
			id string
			u  core.Usage
		)
		if err := rows.Scan(&id,
			&u.Day.Pushed, &u.Day.Delivered, &u.Day.Failed, &u.Day.Expired,
			&u.Week.Pushed, &u.Week.Delivered, &u.Week.Failed, &u.Week.Expired,
			&u.Month.Pushed, &u.Month.Delivered, &u.Month.Failed, &u.Month.Expired,
			&u.PrevMonth.Pushed, &u.PrevMonth.Delivered, &u.PrevMonth.Failed,
			&u.PrevMonth.Expired,
		); err != nil {
			return nil, err
		}
		out[id] = u
	}
	return out, rows.Err()
}
