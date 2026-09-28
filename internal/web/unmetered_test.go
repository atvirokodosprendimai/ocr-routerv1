package web_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestAdminTogglesUnmetered is the route, and the flip in both directions.
//
// A flag an administrator can set and not clear is a flag that makes work free
// for ever, which is why the second half is asserted rather than assumed.
func TestAdminTogglesUnmetered(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	resp := e.do(t, "POST", "/admin/users/"+e.clientID+"/unmetered", e.adminTok, strings.NewReader("{}"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST unmetered = %d, want 200", resp.StatusCode)
	}
	u, err := e.repo.UserByID(ctx, e.clientID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !u.Unmetered {
		t.Fatal("the customer is still metered after the toggle — the handler or the route is " +
			"not reaching identity.SetUnmetered")
	}

	resp = e.do(t, "POST", "/admin/users/"+e.clientID+"/unmetered", e.adminTok, strings.NewReader("{}"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST unmetered again = %d, want 200", resp.StatusCode)
	}
	if again, err := e.repo.UserByID(ctx, e.clientID); err != nil || again.Unmetered {
		t.Errorf("Unmetered after the second toggle = %v (err %v), want false — the control "+
			"cannot be undone", again.Unmetered, err)
	}
}

// TestUnmeteredRouteRefusesANonAdmin asserts the GATE, not the handler.
//
// It is red if the route is mounted outside `requireAdmin` — where a client
// token could make its own work free.
func TestUnmeteredRouteRefusesANonAdmin(t *testing.T) {
	e := newEnv(t)

	for name, tok := range map[string]string{"client": e.clientTok, "worker": e.workerTok} {
		resp := e.do(t, "POST", "/admin/users/"+e.clientID+"/unmetered", tok, strings.NewReader("{}"))
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST unmetered as %s = %d, want 403", name, resp.StatusCode)
		}
	}
	if u, err := e.repo.UserByID(context.Background(), e.clientID); err != nil || u.Unmetered {
		t.Errorf("a refused request still changed the flag (%v, err %v)", u.Unmetered, err)
	}
}

// TestUnmeteredRouteIgnoresAPostedValue is ADR-0004's reasoning applied to this
// control: the new state is the inverse of the STORED one, read server-side.
//
// Trusting a posted boolean lets a stale page re-assert a state somebody just
// changed — and for this flag that means a tab left open overnight can silently
// make a customer's work free again.
func TestUnmeteredRouteIgnoresAPostedValue(t *testing.T) {
	e := newEnv(t)

	resp := e.do(t, "POST", "/admin/users/"+e.clientID+"/unmetered", e.adminTok,
		strings.NewReader(`{"unmetered":false}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST unmetered = %d, want 200", resp.StatusCode)
	}
	u, err := e.repo.UserByID(context.Background(), e.clientID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !u.Unmetered {
		t.Error("posting `false` at a metered customer left them metered — the browser's opinion " +
			"is being consulted, so a stale page can undo a change nobody made")
	}
}

// TestTheCreditsCellReadsUnlimitedWhenUnmetered is what stops the state being
// stored and invisible.
//
// ⚠ It also pins the spelling: `unlimited`, never `-1`. M asked for "-1 credits
// means unlimited" and `-1` is stored nowhere — a cell showing it would invite an
// operator to type it into the adjust box, where it is a MOVEMENT and would land
// as balance − 1 while looking like it worked.
func TestTheCreditsCellReadsUnlimitedWhenUnmetered(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	before := e.page(t, "/admin/users")
	if !strings.Contains(before, "100") {
		t.Fatal("the metered customer's balance of 100 is not on the page at all, so this test " +
			"cannot tell the two renderings apart and would pass on an empty table")
	}
	if strings.Contains(before, "unlimited") {
		t.Fatal("the page says `unlimited` for a METERED customer")
	}

	u, err := e.repo.UserByID(ctx, e.clientID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	u.Unmetered = true
	if err := e.repo.UpdateUser(ctx, u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	after := e.page(t, "/admin/users")
	if !strings.Contains(after, "unlimited") {
		t.Error("the Credits cell does not say `unlimited` for an unmetered customer — the state " +
			"is stored and invisible, which is how a customer stays free by accident")
	}
	if strings.Contains(after, ">-1<") {
		t.Error("the page renders `-1`, which is a value nothing stores and which an operator " +
			"would reasonably type into the adjust box")
	}
}

// TestTheUnmeterToggleButtonNamesTheDirection keeps the label an ACTION.
//
// A button reading the current state is the control an operator clicks by
// mistake: on a row of five near-identical buttons, "Unmetered" reads as a
// status and "Unmeter" reads as a thing that is about to happen.
func TestTheUnmeterToggleButtonNamesTheDirection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	metered := e.page(t, "/admin/users")
	if !strings.Contains(metered, ">Unmeter<") {
		t.Error("no `Unmeter` button on a metered row — the capability is unreachable from the " +
			"page an operator is looking at")
	}

	u, err := e.repo.UserByID(ctx, e.clientID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	u.Unmetered = true
	if err := e.repo.UpdateUser(ctx, u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	free := e.page(t, "/admin/users")
	if !strings.Contains(free, ">Meter<") {
		t.Error("the button still offers to Unmeter a customer that already is — the label is " +
			"reporting state rather than naming what the click does")
	}
}
