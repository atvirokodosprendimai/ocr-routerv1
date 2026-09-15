package web_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// postSignals sends a datastar signal payload, the way the dashboard's buttons
// do.
func (e *env) postSignals(t *testing.T, path, token, body string) *http.Response {
	t.Helper()
	return e.do(t, "POST", path, token, strings.NewReader(body))
}

func (e *env) reloadCustomer(t *testing.T) (buffer, priority, ttl, credits int, active bool) {
	t.Helper()
	u, err := e.repo.UserByID(context.Background(), e.clientID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	return u.BufferLimit, u.Priority, u.JobTTLSecs, u.Credits, u.Active
}

// TestBufferLimitIsEditableFromTheDashboard is the operator's actual complaint,
// and it is red against the dashboard as it stood before ADR-0004.
func TestBufferLimitIsEditableFromTheDashboard(t *testing.T) {
	e := newEnv(t)

	resp := e.postSignals(t, "/admin/users/"+e.clientID+"/settings", e.adminTok,
		`{"bufferLimit":"9","priority":"0","jobTtl":"0"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settings POST = %d, want 200", resp.StatusCode)
	}

	buffer, _, _, _, _ := e.reloadCustomer(t)
	if buffer != 9 {
		t.Errorf("buffer limit = %d, want 9. The dashboard displays this field and cannot change "+
			"it, so an operator has to open the SQLite file to do it", buffer)
	}
}

func TestPriorityAndTTLAreEditable(t *testing.T) {
	e := newEnv(t)

	e.postSignals(t, "/admin/users/"+e.clientID+"/settings", e.adminTok,
		`{"bufferLimit":"4","priority":"12","jobTtl":"900"}`)

	_, priority, ttl, _, _ := e.reloadCustomer(t)
	if priority != 12 {
		t.Errorf("priority = %d, want 12", priority)
	}
	if ttl != 900 {
		t.Errorf("job TTL = %d, want 900", ttl)
	}
}

func TestSettingsEditRerendersTheTable(t *testing.T) {
	e := newEnv(t)

	resp := e.postSignals(t, "/admin/users/"+e.clientID+"/settings", e.adminTok,
		`{"bufferLimit":"7","priority":"0","jobTtl":"0"}`)
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	if !strings.Contains(out, "user-table") {
		t.Fatalf("the edit did not re-render the table, so the operator gets no confirmation "+
			"and no new state:\n%s", out)
	}
	if !strings.Contains(out, `value=\"7\"`) && !strings.Contains(out, `value="7"`) {
		t.Errorf("the re-rendered table does not show the new buffer limit:\n%s", out)
	}
}

// TestInvalidBufferLimitShowsTheServiceMessage — the service's own message has
// to survive the trip to the screen, or an operator who typed 0 meaning "stop
// this customer" is told nothing useful.
func TestInvalidBufferLimitShowsTheServiceMessage(t *testing.T) {
	e := newEnv(t)
	beforeBuffer, _, _, _, _ := e.reloadCustomer(t)

	resp := e.postSignals(t, "/admin/users/"+e.clientID+"/settings", e.adminTok,
		`{"bufferLimit":"0","priority":"0","jobTtl":"0"}`)
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	if !strings.Contains(out, "active") {
		t.Errorf("the refusal does not name the active toggle, so an operator who set 0 meaning "+
			"'stop this customer' is not told where that actually lives:\n%s", out)
	}

	afterBuffer, _, _, _, _ := e.reloadCustomer(t)
	if afterBuffer != beforeBuffer {
		t.Errorf("a refused settings write still applied: buffer %d -> %d", beforeBuffer, afterBuffer)
	}
}

// TestCreditsAreNeverSetDirectly is ADR-0004's `Enforced-by:` check.
//
// ⚠ IT COUNTS LEDGER ENTRIES. A UI that SET the balance would pass a
// balance-only assertion and silently end the audit trail — credit_entries would
// stop being a record of every movement, with nothing failing, while ADR-0002's
// ocrr_credits_debited_total goes on promising the two can be reconciled.
func TestCreditsAreNeverSetDirectly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	before, err := e.repo.Ledger(ctx, e.clientID, 200)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	_, _, _, creditsBefore, _ := e.reloadCustomer(t)

	resp := e.postSignals(t, "/admin/users/"+e.clientID+"/credits", e.adminTok,
		`{"creditDelta":"50","creditReason":"goodwill"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("credits POST = %d, want 200", resp.StatusCode)
	}

	_, _, _, creditsAfter, _ := e.reloadCustomer(t)
	if creditsAfter != creditsBefore+50 {
		t.Errorf("balance %d -> %d, want +50", creditsBefore, creditsAfter)
	}

	after, err := e.repo.Ledger(ctx, e.clientID, 200)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("the ledger gained %d entries, want exactly 1. A balance that moves without an "+
			"entry ends the audit trail silently — which is the whole reason this record exists",
			len(after)-len(before))
	}
	if after[0].Delta != 50 {
		t.Errorf("newest ledger entry delta = %d, want 50", after[0].Delta)
	}
	if !strings.Contains(after[0].Reason, "goodwill") {
		t.Errorf("the entry does not carry the operator's reason: %q", after[0].Reason)
	}
}

func TestCreditAdjustmentRequiresAReason(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	before, _ := e.repo.Ledger(ctx, e.clientID, 200)
	_, _, _, creditsBefore, _ := e.reloadCustomer(t)

	resp := e.postSignals(t, "/admin/users/"+e.clientID+"/credits", e.adminTok,
		`{"creditDelta":"50","creditReason":"   "}`)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "reason") {
		t.Errorf("the refusal does not mention the reason:\n%s", body)
	}

	_, _, _, creditsAfter, _ := e.reloadCustomer(t)
	if creditsAfter != creditsBefore {
		t.Errorf("a reasonless adjustment moved the balance %d -> %d", creditsBefore, creditsAfter)
	}
	after, _ := e.repo.Ledger(ctx, e.clientID, 200)
	if len(after) != len(before) {
		t.Error("a refused adjustment still wrote a ledger entry")
	}
}

func TestNegativeCreditAdjustment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, _, _, creditsBefore, _ := e.reloadCustomer(t)

	e.postSignals(t, "/admin/users/"+e.clientID+"/credits", e.adminTok,
		`{"creditDelta":"-20","creditReason":"correction"}`)

	_, _, _, creditsAfter, _ := e.reloadCustomer(t)
	if creditsAfter != creditsBefore-20 {
		t.Errorf("balance %d -> %d, want -20", creditsBefore, creditsAfter)
	}
	entries, _ := e.repo.Ledger(ctx, e.clientID, 200)
	if len(entries) == 0 || entries[0].Delta != -20 {
		t.Errorf("newest ledger entry is not -20: %+v", entries)
	}
}

// TestActiveToggleIsReachable is identity.SetActive's first caller ever.
func TestActiveToggleIsReachable(t *testing.T) {
	e := newEnv(t)

	if _, _, _, _, active := e.reloadCustomer(t); !active {
		t.Fatal("the seeded customer is already disabled, so this proves nothing")
	}

	resp := e.do(t, "POST", "/admin/users/"+e.clientID+"/active", e.adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("active POST = %d, want 200", resp.StatusCode)
	}
	if _, _, _, _, active := e.reloadCustomer(t); active {
		t.Fatal("the toggle did not deactivate the customer")
	}

	// And back — a toggle that only goes one way is half a control.
	e.do(t, "POST", "/admin/users/"+e.clientID+"/active", e.adminTok, nil)
	if _, _, _, _, active := e.reloadCustomer(t); !active {
		t.Error("the toggle did not reactivate the customer")
	}
}

// TestDeactivatedUsersTokensStopWorking asserts the consequence the button's
// title promises, rather than describing it.
func TestDeactivatedUsersTokensStopWorking(t *testing.T) {
	e := newEnv(t)

	if got := e.do(t, "GET", "/services", e.clientTok, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("the client token does not work to begin with: %d", got)
	}

	e.do(t, "POST", "/admin/users/"+e.clientID+"/active", e.adminTok, nil)

	if got := e.do(t, "GET", "/services", e.clientTok, nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("after deactivating, the customer's token still works (%d). The button's title "+
			"promises every token stops immediately", got)
	}
}

func TestSettingsRoutesRequireAdmin(t *testing.T) {
	e := newEnv(t)

	paths := []string{
		"/admin/users/" + e.clientID + "/settings",
		"/admin/users/" + e.clientID + "/credits",
		"/admin/users/" + e.clientID + "/active",
	}
	for _, p := range paths {
		// Authenticated but not an admin. A customer raising their own buffer
		// limit or crediting themselves is the failure this guards.
		if got := e.do(t, "POST", p, e.clientTok, nil).StatusCode; got != http.StatusForbidden {
			t.Errorf("POST %s as a client = %d, want 403", p, got)
		}
		if got := e.do(t, "POST", p, "", nil).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("POST %s with no token = %d, want 401", p, got)
		}
	}

	// Nothing was applied by any of those attempts.
	buffer, _, _, credits, active := e.reloadCustomer(t)
	if buffer != 4 || credits != 100 || !active {
		t.Errorf("an unauthorised attempt changed something: buffer=%d credits=%d active=%v",
			buffer, credits, active)
	}
}

func TestSettingsRejectsNonNumericInput(t *testing.T) {
	e := newEnv(t)
	beforeBuffer, _, _, _, _ := e.reloadCustomer(t)

	resp := e.postSignals(t, "/admin/users/"+e.clientID+"/settings", e.adminTok,
		`{"bufferLimit":"eight","priority":"0","jobTtl":"0"}`)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "whole number") {
		t.Errorf("the refusal does not say what is wrong with the field:\n%s", body)
	}

	afterBuffer, _, _, _, _ := e.reloadCustomer(t)
	if afterBuffer != beforeBuffer {
		t.Error("a non-numeric settings write applied something")
	}
}

func TestSettingsRejectsEmptyField(t *testing.T) {
	e := newEnv(t)

	resp := e.postSignals(t, "/admin/users/"+e.clientID+"/settings", e.adminTok,
		`{"bufferLimit":"","priority":"0","jobTtl":"0"}`)
	body, _ := io.ReadAll(resp.Body)
	// An empty field read as 0 would hit the service floor and produce a
	// confusing refusal about a value the operator never typed.
	if !strings.Contains(string(body), "required") {
		t.Errorf("an empty buffer limit was not reported as missing:\n%s", body)
	}
}

// TestSettingsRoutesAreOriginGuarded — three new state-changing routes are three
// new CSRF surfaces, and they are only safe because they sit inside the group
// ADR-0003's guard covers. A route added outside it would pass every other test
// in this file.
func TestSettingsRoutesAreOriginGuarded(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)

	u, err := e.repo.UserByEmail(context.Background(), "admin@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}

	for _, path := range []string{
		"/admin/users/" + u.ID + "/settings",
		"/admin/users/" + u.ID + "/credits",
		"/admin/users/" + u.ID + "/active",
	} {
		req, _ := http.NewRequest("POST", e.srv.URL+path,
			strings.NewReader(`{"bufferLimit":"9","priority":"0","jobTtl":"0"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(c)
		// No Origin and no Referer — the shape an attacker's page produces.
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s with a session cookie and no Origin = %d, want 403. The route is "+
				"outside the guarded group, so any site can make an administrator's browser "+
				"change a customer's settings", path, resp.StatusCode)
		}
	}

	// And the dashboard's own origin still works, or the assertions above are
	// satisfied by a guard that refuses everything.
	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/users/"+u.ID+"/settings",
		strings.NewReader(`{"bufferLimit":"9","priority":"0","jobTtl":"0"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.srv.URL)
	req.AddCookie(c)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("same-origin POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Error("the dashboard's own settings POST was refused as cross-origin")
	}
}

// TestCreateCustomerSurvivesTheNumberSignals is the regression for a bug the
// operator found in a browser and every test here missed.
//
// ⚠ DATASTAR SIGNALS ARE GLOBAL: every unprefixed one is posted on EVERY action.
// So clicking "Create customer" also sends bufferLimit, priority, jobTtl and
// creditDelta — and `data-bind` on an `<input type="number">` sends them as JSON
// NUMBERS. The signal struct declared them as `string`, so ReadSignals failed,
// and EVERY handler reported "could not read the form" — including create, which
// has nothing to do with those fields.
//
// The tests in this file all passed, because each one hand-wrote
// `{"bufferLimit":"9"}` with quotes. They invented the wire format instead of
// observing it, so they agreed with the code and both were wrong together.
func TestCreateCustomerSurvivesTheNumberSignals(t *testing.T) {
	e := newEnv(t)

	// Exactly what the browser posts once the customer table is on screen.
	resp := e.postSignals(t, "/admin/users", e.adminTok,
		`{"newEmail":"brand-new@example.com","newRole":"client",`+
			`"bufferLimit":4,"priority":0,"jobTtl":0,"creditDelta":null,"creditReason":""}`)
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	if strings.Contains(out, "could not read the form") {
		t.Fatalf("creating a customer failed to read the form. The number signals the customer "+
			"table binds are posted on EVERY action, and the signal struct cannot decode "+
			"them:\n%s", out)
	}

	u, err := e.repo.UserByEmail(context.Background(), "brand-new@example.com")
	if err != nil {
		t.Fatalf("the customer was not created: %v", err)
	}
	if u.Email != "brand-new@example.com" {
		t.Errorf("created %q", u.Email)
	}
}

// TestSettingsAcceptTheBrowsersNumericSignals — the same wire format, on the
// handler the numbers actually belong to.
func TestSettingsAcceptTheBrowsersNumericSignals(t *testing.T) {
	e := newEnv(t)

	// JSON numbers, not quoted strings. This is what `<input type="number">`
	// produces; the quoted form the other tests use is what a Go author guesses.
	resp := e.postSignals(t, "/admin/users/"+e.clientID+"/settings", e.adminTok,
		`{"bufferLimit":11,"priority":2,"jobTtl":300}`)
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "could not read the form") {
		t.Fatalf("the settings handler cannot decode the browser's own payload:\n%s", body)
	}

	buffer, priority, ttl, _, _ := e.reloadCustomer(t)
	if buffer != 11 || priority != 2 || ttl != 300 {
		t.Errorf("numeric signals did not apply: buffer=%d priority=%d ttl=%d", buffer, priority, ttl)
	}
}

// TestCreditAdjustmentAcceptsANumericDelta — same shape, on the credits path.
func TestCreditAdjustmentAcceptsANumericDelta(t *testing.T) {
	e := newEnv(t)
	_, _, _, before, _ := e.reloadCustomer(t)

	resp := e.postSignals(t, "/admin/users/"+e.clientID+"/credits", e.adminTok,
		`{"creditDelta":-15,"creditReason":"numeric signal"}`)
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "could not read the form") {
		t.Fatalf("the credits handler cannot decode a numeric delta:\n%s", body)
	}

	_, _, _, after, _ := e.reloadCustomer(t)
	if after != before-15 {
		t.Errorf("balance %d -> %d, want -15", before, after)
	}
}

// TestEmptyNumberSignalIsReportedAsMissing — an untouched number input sends
// null or "", and neither may become a silent zero.
func TestEmptyNumberSignalIsReportedAsMissing(t *testing.T) {
	e := newEnv(t)

	for _, payload := range []string{
		`{"bufferLimit":null,"priority":0,"jobTtl":0}`,
		`{"bufferLimit":"","priority":0,"jobTtl":0}`,
	} {
		resp := e.postSignals(t, "/admin/users/"+e.clientID+"/settings", e.adminTok, payload)
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "required") {
			t.Errorf("payload %s did not report the missing field:\n%s", payload, body)
		}
	}
}
