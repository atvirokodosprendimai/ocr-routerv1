package store_test

import (
	"context"
	"testing"
)

// TestANewUserIsNotUnmetered is the direction a billing default has to fail in.
//
// ADR-0009 stores `unmetered INTEGER NOT NULL DEFAULT 0`, so every row that
// predates the column and every row created without thinking about it is
// METERED. The inverse spelling — a `metered` column defaulting to true — has
// identical mechanics and gets the default wrong the one time somebody forgets
// it.
func TestANewUserIsNotUnmetered(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	u := mkUser(t, r, "u1", 0, 100)

	got, err := r.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got.Unmetered {
		t.Error("a freshly created user is unmetered — the default makes work free for " +
			"anyone nobody thought about")
	}
}

// TestUnmeteredSurvivesTheRoundTrip is the one that catches the failure mode
// this task's Risks name: a column list and a scan list that move apart.
//
// It is red if `unmetered` is missing from UpdateUser's UPDATE (the flag is set
// and silently not stored) and red if it is missing from userColumns or the scan
// (which is a runtime error on every user read, so it is red loudly).
func TestUnmeteredSurvivesTheRoundTrip(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	u := mkUser(t, r, "u1", 0, 100)
	u.Unmetered = true
	if err := r.UpdateUser(ctx, u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	got, err := r.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !got.Unmetered {
		t.Fatal("Unmetered did not survive UpdateUser → UserByID")
	}

	// And back. A flag that can only be set is a flag an administrator cannot
	// undo, which for this one means work stays free for ever.
	got.Unmetered = false
	if err := r.UpdateUser(ctx, got); err != nil {
		t.Fatalf("UpdateUser clearing: %v", err)
	}
	if again, err := r.UserByID(ctx, u.ID); err != nil || again.Unmetered {
		t.Errorf("Unmetered after clearing = %v (err %v), want false", again.Unmetered, err)
	}
}

// TestUnmeteredIsCarriedByListUsers exists because ListUsers has its OWN scan.
//
// The dashboard reads customers through this path and not through UserByID, so a
// column added to one scan and not the other renders every customer as metered
// while the database says otherwise — a wrong answer rather than an error.
func TestUnmeteredIsCarriedByListUsers(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	u := mkUser(t, r, "u1", 0, 100)
	u.Unmetered = true
	if err := r.UpdateUser(ctx, u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	users, err := r.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	var seen bool
	for _, got := range users {
		if got.ID != u.ID {
			continue
		}
		seen = true
		if !got.Unmetered {
			t.Error("ListUsers reports the user as metered while UserByID would not — " +
				"the second scan is missing the column")
		}
	}
	if !seen {
		t.Fatalf("ListUsers did not return %s at all, so this assertion proved nothing", u.ID)
	}
}

// TestUnmeteredChangeDoesNotTouchCredits keeps ADR-0004's property true through
// this edit.
//
// `UpdateUser` writes the admin-settable knobs and deliberately never writes
// `credits`: the balance is a ledger and moves only inside DeliverJob's
// transaction. Adding a column to that UPDATE is exactly the kind of edit that
// could quietly add `credits = ?` beside it.
func TestUnmeteredChangeDoesNotTouchCredits(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	u := mkUser(t, r, "u1", 0, 100)

	// A balance the caller's in-memory copy disagrees with, which is the real
	// shape: the dashboard read this user, then somebody's job was delivered.
	if err := r.AddCredits(ctx, u.ID, 50, "seed", base); err != nil {
		t.Fatalf("AddCredits: %v", err)
	}
	u.Credits = 0 // the stale copy an admin form would post back
	u.Unmetered = true
	if err := r.UpdateUser(ctx, u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	got, err := r.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got.Credits != 150 {
		t.Errorf("credits = %d after UpdateUser, want 150 — UpdateUser wrote the balance, "+
			"which makes the ledger stop being an audit with nothing failing to say so",
			got.Credits)
	}
	if !got.Unmetered {
		t.Error("the flag did not persist, so this test proved nothing about the balance")
	}
}
