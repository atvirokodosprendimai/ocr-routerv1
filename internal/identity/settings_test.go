package identity_test

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

// settingsFixture bootstraps an admin and a customer to edit.
type settingsFixture struct {
	*identityHarness
	admin    core.Principal
	client   core.Principal
	customer core.User
}

func newSettingsFixture(t *testing.T) *settingsFixture {
	t.Helper()
	h := newIdentityHarness(t)
	ctx := context.Background()

	adminUser := h.bootstrapAdmin(t)
	ap, err := h.svc.Authenticate(ctx, "Bearer "+h.adminToken, base)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	cu, err := h.svc.CreateUser(ctx, ap, "customer@example.com", core.RoleClient, base)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := h.repo.AddCredits(ctx, cu.ID, 100, "seed", base); err != nil {
		t.Fatalf("AddCredits: %v", err)
	}
	_ = adminUser

	return &settingsFixture{
		identityHarness: h,
		admin:           ap,
		client:          core.Principal{UserID: cu.ID, Role: core.RoleClient, TokenID: "t"},
		customer:        cu,
	}
}

func (f *settingsFixture) reload(t *testing.T) core.User {
	t.Helper()
	u, err := f.repo.UserByID(context.Background(), f.customer.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	return u
}

func TestUpdateSettingsWritesAllThree(t *testing.T) {
	f := newSettingsFixture(t)

	if err := f.svc.UpdateSettings(context.Background(), f.admin, f.customer.ID, 9, 7, 600); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	got := f.reload(t)
	if got.BufferLimit != 9 {
		t.Errorf("buffer limit = %d, want 9", got.BufferLimit)
	}
	if got.Priority != 7 {
		t.Errorf("priority = %d, want 7", got.Priority)
	}
	if got.JobTTLSecs != 600 {
		t.Errorf("job TTL = %d, want 600", got.JobTTLSecs)
	}
}

// TestUpdateSettingsDoesNotTouchCredits is red against a whole-row UpdateUser.
//
// ⚠ THE BALANCE MUST MOVE AFTER THE CALLER'S READ, or this proves nothing. A
// test that reads a user, writes settings and checks credits passes against
// UpdateUser too, because nothing changed in between. The hazard is a delivery
// landing between the read and the write — so the fixture makes that happen.
func TestUpdateSettingsDoesNotTouchCredits(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()

	// What an admin's form would have read.
	before := f.reload(t)
	if before.Credits != 100 {
		t.Fatalf("seed balance = %d, want 100", before.Credits)
	}

	// A job is delivered underneath the open form.
	if err := f.repo.AddCredits(ctx, f.customer.ID, -30, "delivery", base); err != nil {
		t.Fatalf("AddCredits: %v", err)
	}

	// The admin saves the settings they were looking at.
	if err := f.svc.UpdateSettings(ctx, f.admin, f.customer.ID, 6, 1, 0); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	got := f.reload(t)
	if got.Credits != 70 {
		t.Errorf("balance = %d, want 70. The settings write restored a stale credits value — a "+
			"whole-row UPDATE from a read-modify-write silently undoes every delivery that "+
			"landed while the form was open", got.Credits)
	}
	if got.BufferLimit != 6 {
		t.Errorf("the settings did not apply: buffer = %d", got.BufferLimit)
	}
}

func TestUpdateSettingsDoesNotTouchRoleOrEmail(t *testing.T) {
	f := newSettingsFixture(t)

	if err := f.svc.UpdateSettings(context.Background(), f.admin, f.customer.ID, 4, 0, 0); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	got := f.reload(t)
	if got.Role != core.RoleClient {
		t.Errorf("role = %q, want client", got.Role)
	}
	if got.Email != "customer@example.com" {
		t.Errorf("email = %q", got.Email)
	}
	if !got.Active {
		t.Error("the user was deactivated by a settings write")
	}
}

func TestBufferLimitBelowOneIsRefused(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()

	for _, bad := range []int{0, -1} {
		err := f.svc.UpdateSettings(ctx, f.admin, f.customer.ID, bad, 0, 0)
		if !errors.Is(err, core.ErrInvalidParam) {
			t.Errorf("buffer limit %d = %v, want core.ErrInvalidParam", bad, err)
			continue
		}
		// ⚠ The message must name `active`. An operator setting 0 means "stop
		// this customer", and they need telling where that actually lives —
		// otherwise 0 silently rejects every upload forever, reported to the
		// customer as ordinary back-pressure.
		if !strings.Contains(err.Error(), "active") {
			t.Errorf("the refusal does not name the active toggle: %v", err)
		}
	}

	// 1 is accepted — the bound is inclusive, and without this the refusals
	// above are satisfied by a check that rejects everything.
	if err := f.svc.UpdateSettings(ctx, f.admin, f.customer.ID, 1, 0, 0); err != nil {
		t.Errorf("buffer limit 1 was refused: %v", err)
	}
	if got := f.reload(t); got.BufferLimit != 1 {
		t.Errorf("buffer limit = %d, want 1", got.BufferLimit)
	}
}

// TestZeroJobTTLIsAccepted is red against a `> 0` validation.
//
// "Must be positive" is the natural way to write this check and it forbids the
// common case: zero means no deadline, which is what most customers want.
func TestZeroJobTTLIsAccepted(t *testing.T) {
	f := newSettingsFixture(t)

	if err := f.svc.UpdateSettings(context.Background(), f.admin, f.customer.ID, 4, 0, 0); err != nil {
		t.Fatalf("a zero job TTL was refused: %v — zero means NO DEADLINE and is the common "+
			"choice, so a `> 0` check forbids what most customers want", err)
	}
	if got := f.reload(t); got.JobTTLSecs != 0 {
		t.Errorf("job TTL = %d, want 0", got.JobTTLSecs)
	}
}

func TestNegativeJobTTLIsRefused(t *testing.T) {
	f := newSettingsFixture(t)

	err := f.svc.UpdateSettings(context.Background(), f.admin, f.customer.ID, 4, 0, -1)
	if !errors.Is(err, core.ErrInvalidParam) {
		t.Errorf("job TTL -1 = %v, want core.ErrInvalidParam", err)
	}
}

// TestNegativePriorityIsAccepted pins a deliberate non-validation.
//
// It looks like a missing check to the next reader, and this test is the only
// thing that stops someone "fixing" it — burying a customer below the default is
// a legitimate operator action.
func TestNegativePriorityIsAccepted(t *testing.T) {
	f := newSettingsFixture(t)

	if err := f.svc.UpdateSettings(context.Background(), f.admin, f.customer.ID, 4, -5, 0); err != nil {
		t.Fatalf("a negative priority was refused: %v", err)
	}
	if got := f.reload(t); got.Priority != -5 {
		t.Errorf("priority = %d, want -5", got.Priority)
	}
}

func TestUpdateSettingsRequiresAdmin(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()

	for name, actor := range map[string]core.Principal{
		"client": f.client,
		"worker": {UserID: f.customer.ID, Role: core.RoleWorker, TokenID: "w"},
		"zero":   {},
	} {
		t.Run(name, func(t *testing.T) {
			if err := f.svc.UpdateSettings(ctx, actor, f.customer.ID, 99, 99, 99); !errors.Is(err, core.ErrForbidden) {
				t.Errorf("err = %v, want core.ErrForbidden", err)
			}
			if got := f.reload(t); got.BufferLimit == 99 {
				t.Error("a non-admin's settings write was applied")
			}
		})
	}
}

// TestAdjustCreditsMovesBalanceAndWritesEntry asserts BOTH halves.
//
// ⚠ Checking only the balance passes against a direct write that skips the
// ledger — which is the exact defect ADR-0004 exists to prevent. Checking only
// the entry passes against one that logs and does not pay.
func TestAdjustCreditsMovesBalanceAndWritesEntry(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()

	ledgerBefore, err := f.repo.Ledger(ctx, f.customer.ID, 100)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}

	if err := f.svc.AdjustCredits(ctx, f.admin, f.customer.ID, 50, "goodwill", base); err != nil {
		t.Fatalf("AdjustCredits: %v", err)
	}

	if got := f.reload(t).Credits; got != 150 {
		t.Errorf("balance = %d, want 150", got)
	}

	ledgerAfter, err := f.repo.Ledger(ctx, f.customer.ID, 100)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if len(ledgerAfter) != len(ledgerBefore)+1 {
		t.Fatalf("the ledger gained %d entries, want exactly 1. A balance that moves without an "+
			"entry ends the audit trail silently, and ocrr_credits_debited_total goes on "+
			"promising the two can be reconciled", len(ledgerAfter)-len(ledgerBefore))
	}
	newest := ledgerAfter[0]
	if newest.Delta != 50 {
		t.Errorf("entry delta = %d, want 50", newest.Delta)
	}
	if !strings.Contains(newest.Reason, "goodwill") {
		t.Errorf("entry reason = %q, does not carry the given reason", newest.Reason)
	}
}

func TestAdjustCreditsNegativeDelta(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()

	if err := f.svc.AdjustCredits(ctx, f.admin, f.customer.ID, -20, "correction", base); err != nil {
		t.Fatalf("AdjustCredits: %v", err)
	}
	if got := f.reload(t).Credits; got != 80 {
		t.Errorf("balance = %d, want 80", got)
	}

	entries, _ := f.repo.Ledger(ctx, f.customer.ID, 100)
	if len(entries) == 0 || entries[0].Delta != -20 {
		t.Errorf("newest ledger entry is not -20: %+v", entries)
	}
}

func TestAdjustCreditsRefusesBlankReason(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()

	before, _ := f.repo.Ledger(ctx, f.customer.ID, 100)
	for _, blank := range []string{"", "   ", "\t\n"} {
		if err := f.svc.AdjustCredits(ctx, f.admin, f.customer.ID, 10, blank, base); !errors.Is(err, core.ErrInvalidParam) {
			t.Errorf("reason %q = %v, want core.ErrInvalidParam", blank, err)
		}
	}
	if got := f.reload(t).Credits; got != 100 {
		t.Errorf("a refused adjustment moved the balance to %d", got)
	}
	after, _ := f.repo.Ledger(ctx, f.customer.ID, 100)
	if len(after) != len(before) {
		t.Error("a refused adjustment still wrote a ledger entry")
	}

	// And a real reason works — without this, the refusals above are satisfied
	// by a method that rejects everything.
	if err := f.svc.AdjustCredits(ctx, f.admin, f.customer.ID, 10, "a real reason", base); err != nil {
		t.Errorf("a valid adjustment was refused: %v", err)
	}
}

func TestAdjustCreditsRefusesZeroDelta(t *testing.T) {
	f := newSettingsFixture(t)

	err := f.svc.AdjustCredits(context.Background(), f.admin, f.customer.ID, 0, "why not", base)
	if !errors.Is(err, core.ErrInvalidParam) {
		t.Errorf("a zero adjustment = %v, want core.ErrInvalidParam — an entry recording that "+
			"nothing happened is noise in the one log that has to stay readable", err)
	}
}

func TestAdjustCreditsRequiresAdmin(t *testing.T) {
	f := newSettingsFixture(t)

	if err := f.svc.AdjustCredits(context.Background(), f.client, f.customer.ID, 1000, "mine now", base); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("err = %v, want core.ErrForbidden", err)
	}
	if got := f.reload(t).Credits; got != 100 {
		t.Errorf("a customer credited themselves: balance = %d", got)
	}
}

func TestAdjustCreditsOnUnknownUser(t *testing.T) {
	f := newSettingsFixture(t)

	err := f.svc.AdjustCredits(context.Background(), f.admin, core.NewID(), 10, "typo", base)
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("err = %v, want core.ErrNotFound — AddCredits would otherwise match no row and "+
			"still write a ledger entry, leaving an orphan that reconciles against nothing", err)
	}
}

// TestNoMethodSetsCreditsDirectly is the structural half of ADR-0004 Decision 1.
//
// ⚠ It matches on method NAMES, so a method called `OverwriteBalance` would slip
// past. It is a tripwire for the obvious spelling, not a proof — the invariant
// and the bound mutant are what a reviewer reads. Saying so is better than
// letting the next author believe the rule is mechanically enforced.
func TestNoMethodSetsCreditsDirectly(t *testing.T) {
	bad := regexp.MustCompile(`(?i)^(set|write|overwrite|replace|put).*credit`)

	for _, typ := range []reflect.Type{
		reflect.TypeOf(&identity.Service{}),
		reflect.TypeOf(&store.Repo{}),
	} {
		for i := range typ.NumMethod() {
			name := typ.Method(i).Name
			if bad.MatchString(name) {
				t.Errorf("%s.%s looks like a direct credit write. Every movement must go through "+
					"AddCredits, which writes the balance and the ledger entry in one "+
					"transaction — a balance that moves without an entry ends the audit trail "+
					"with nothing failing", typ, name)
			}
		}
		if typ.NumMethod() == 0 {
			t.Fatalf("%s exposes no methods, so this assertion proved nothing", typ)
		}
	}
}
