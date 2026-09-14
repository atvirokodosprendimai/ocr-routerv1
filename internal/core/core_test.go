package core_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

func TestJobCanTransitionTo(t *testing.T) {
	// The full edge set of the lifecycle in ADR-0001. Every pair not listed as
	// allowed must be refused, which is why this is a matrix rather than a
	// handful of positive cases: a permissive CanTransitionTo passes any test
	// that only checks the happy edges.
	allowed := map[core.JobState][]core.JobState{
		core.JobQueued:     {core.JobProcessing, core.JobExpired, core.JobDead},
		core.JobProcessing: {core.JobDone, core.JobQueued, core.JobDead},
		core.JobDone:       {core.JobDelivered, core.JobQueued},
		core.JobDelivered:  {},
		core.JobDead:       {},
		core.JobExpired:    {},
	}

	all := []core.JobState{
		core.JobQueued, core.JobProcessing, core.JobDone,
		core.JobDelivered, core.JobDead, core.JobExpired,
	}

	for from, tos := range allowed {
		ok := map[core.JobState]bool{}
		for _, to := range tos {
			ok[to] = true
		}
		for _, to := range all {
			j := core.Job{State: from}
			got := j.CanTransitionTo(to)
			if got != ok[to] {
				t.Errorf("CanTransitionTo(%s -> %s) = %v, want %v", from, to, got, ok[to])
			}
		}
	}
}

func TestJobCanTransitionToRejectsTerminal(t *testing.T) {
	// Called out separately from the matrix because "terminal" is the property
	// a future edit is most likely to break by adding a convenience transition.
	for _, from := range []core.JobState{core.JobDelivered, core.JobDead, core.JobExpired} {
		for _, to := range []core.JobState{
			core.JobQueued, core.JobProcessing, core.JobDone,
			core.JobDelivered, core.JobDead, core.JobExpired,
		} {
			j := core.Job{State: from}
			if j.CanTransitionTo(to) {
				t.Errorf("%s is terminal but allowed transition to %s", from, to)
			}
		}
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range []core.Role{core.RoleAdmin, core.RoleClient, core.RoleWorker} {
		if !r.Valid() {
			t.Errorf("Role(%q).Valid() = false, want true", r)
		}
	}
	for _, r := range []core.Role{"", "Admin", "ADMIN", "root", "client ", "unknown"} {
		if core.Role(r).Valid() {
			t.Errorf("Role(%q).Valid() = true, want false", r)
		}
	}
}

func TestJobStateValid(t *testing.T) {
	for _, s := range []core.JobState{
		core.JobQueued, core.JobProcessing, core.JobDone,
		core.JobDelivered, core.JobDead, core.JobExpired,
	} {
		if !s.Valid() {
			t.Errorf("JobState(%q).Valid() = false, want true", s)
		}
	}
	for _, s := range []string{"", "Queued", "QUEUED", "running", "failed", "pending"} {
		if core.JobState(s).Valid() {
			t.Errorf("JobState(%q).Valid() = true, want false", s)
		}
	}
}

func TestNewIDIsOrdered(t *testing.T) {
	// uuidv7 is time-ordered, which is what lets the claim statement use
	// `ORDER BY id` as FIFO-within-a-tier instead of carrying a sequence column.
	const n = 500
	ids := make([]string, n)
	for i := range ids {
		ids[i] = core.NewID()
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("ids are not lexically ordered by mint order: index %d has %q, sorted has %q",
				i, ids[i], sorted[i])
		}
	}
}

func TestNewIDIsUnique(t *testing.T) {
	const n = 10000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := core.NewID()
		if _, dup := seen[id]; dup {
			t.Fatalf("NewID collided after %d ids: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestJobIsLastStage(t *testing.T) {
	cases := []struct {
		name     string
		pipeline []string
		stage    int
		want     bool
	}{
		{"single stage at 0", []string{"ocr"}, 0, true},
		{"two stages at 0", []string{"crawl", "strip-html"}, 0, false},
		{"two stages at 1", []string{"crawl", "strip-html"}, 1, true},
		{"three stages at 1", []string{"crawl", "strip-html", "ocr"}, 1, false},
		{"empty pipeline is its own last stage", nil, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := core.Job{Pipeline: c.pipeline, Stage: c.stage}
			if got := j.IsLastStage(); got != c.want {
				t.Errorf("IsLastStage() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestValidParamKey(t *testing.T) {
	// A param key becomes a FLAG NAME in the worker's argv, which is why the key
	// is the constrained half and the value is not (ADR-0001 §Decision).
	good := []string{"url", "a", "max-depth", "x9", "a-b-c", strings.Repeat("a", 32)}
	for _, k := range good {
		if !core.ValidParamKey(k) {
			t.Errorf("ValidParamKey(%q) = false, want true", k)
		}
	}
	bad := []string{
		"",                      // empty
		"URL",                   // uppercase
		"1st",                   // leading digit
		"-lead",                 // leading dash
		"has space",             // space
		"--flag",                // looks like a flag
		"a;b",                   // shell metacharacter
		"a_b",                   // underscore is not in the set
		"a.b",                   // dot
		"a/b",                   // path separator
		strings.Repeat("a", 33), // one over the cap
		"a\nb",                  // newline
	}
	for _, k := range bad {
		if core.ValidParamKey(k) {
			t.Errorf("ValidParamKey(%q) = true, want false", k)
		}
	}
}
