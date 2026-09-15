package identity_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

const goodPassword = "correct horse battery staple"

// identityHarness exposes the repo as well as the service, which the password
// and session paths need and the token tests did not.
type identityHarness struct {
	svc  *identity.Service
	repo *store.Repo
	db   *store.DB
	// adminToken is the bootstrapped admin's bearer token, kept so a test can
	// authenticate as an administrator to call the admin-gated write methods.
	adminToken string
}

func newIdentityHarness(t *testing.T) *identityHarness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "id.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := store.NewRepo(db)
	return &identityHarness{svc: identity.New(repo), repo: repo, db: db}
}

func (h *identityHarness) bootstrapAdmin(t *testing.T) core.User {
	t.Helper()
	u, tok, err := h.svc.Bootstrap(context.Background(), "admin@example.com", base)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	h.adminToken = tok
	return u
}

func TestHashThenVerifyRoundTrips(t *testing.T) {
	h, err := identity.HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !identity.VerifyPassword(h, goodPassword) {
		t.Error("a password did not verify against its own hash — every negative test below " +
			"would be satisfied by a function that simply refuses everything")
	}
}

func TestWrongPasswordIsRefused(t *testing.T) {
	h, _ := identity.HashPassword(goodPassword)
	if identity.VerifyPassword(h, "correct horse battery stapl") {
		t.Error("a one-character-different password verified")
	}
}

func TestSamePasswordHashesDifferently(t *testing.T) {
	a, _ := identity.HashPassword(goodPassword)
	b, _ := identity.HashPassword(goodPassword)

	if a == b {
		t.Error("hashing one password twice produced identical strings — the salt is not random " +
			"per hash, so two admins choosing the same password are visibly identical in the " +
			"database")
	}
	// Both must still verify: a salt that varied but was not stored would break this.
	if !identity.VerifyPassword(a, goodPassword) || !identity.VerifyPassword(b, goodPassword) {
		t.Error("a salted hash did not verify")
	}
}

func TestHashIsPHCFormatted(t *testing.T) {
	h, _ := identity.HashPassword(goodPassword)

	parts := strings.Split(h, "$")
	if len(parts) != 6 {
		t.Fatalf("hash has %d $-separated parts, want 6: %q", len(parts), h)
	}
	if parts[1] != "argon2id" {
		t.Errorf("algorithm = %q, want argon2id", parts[1])
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		t.Errorf("version segment = %q", parts[2])
	}
	if !strings.HasPrefix(parts[3], "m=") || !strings.Contains(parts[3], ",t=") ||
		!strings.Contains(parts[3], ",p=") {
		t.Errorf("parameter segment = %q, want m=…,t=…,p=…", parts[3])
	}
	for i, seg := range []string{parts[4], parts[5]} {
		if _, err := base64.RawStdEncoding.DecodeString(seg); err != nil {
			t.Errorf("segment %d is not raw-std base64: %q", i+4, seg)
		}
	}
}

// TestVerifyUsesTheParametersInTheHash is what makes raising the cost later a
// non-breaking change.
//
// ⚠ The hash is built BY HAND with parameters deliberately unlike the package
// defaults. Calling HashPassword here would prove nothing, because the defaults
// would match whether verification read them from the string or from a constant.
func TestVerifyUsesTheParametersInTheHash(t *testing.T) {
	var (
		salt    = []byte("0123456789abcdef")
		time_   = uint32(1)
		memory  = uint32(8 * 1024)
		threads = uint8(1)
	)
	key := argon2.IDKey([]byte(goodPassword), salt, time_, memory, threads, 32)
	encoded := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, time_, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))

	if !identity.VerifyPassword(encoded, goodPassword) {
		t.Error("a hash carrying non-default parameters did not verify — verification is using " +
			"package constants instead of the values stored with the hash, which means raising " +
			"the cost later would silently invalidate every existing password")
	}
	if identity.VerifyPassword(encoded, "wrong password entirely") {
		t.Error("the hand-built hash verified a wrong password, so the assertion above proves " +
			"nothing")
	}
}

func TestShortPasswordIsRejected(t *testing.T) {
	// Both sides of the boundary, so an off-by-one is visible.
	if _, err := identity.HashPassword(strings.Repeat("a", 11)); !errors.Is(err, identity.ErrWeakPassword) {
		t.Errorf("11 characters returned %v, want ErrWeakPassword", err)
	}
	if _, err := identity.HashPassword(strings.Repeat("a", 12)); err != nil {
		t.Errorf("12 characters returned %v, want success — the minimum must be inclusive", err)
	}
}

func TestLengthIsCountedInRunes(t *testing.T) {
	// Twelve runes, thirty-six bytes. A byte count would accept it; a rune count
	// is what the author of the passphrase would call twelve.
	pw := strings.Repeat("日", 12)
	if _, err := identity.HashPassword(pw); err != nil {
		t.Errorf("a 12-rune passphrase was rejected: %v", err)
	}
	// And eleven runes must still fail, or the test above passes for the wrong
	// reason — a byte count would accept eleven of these too.
	if _, err := identity.HashPassword(strings.Repeat("日", 11)); !errors.Is(err, identity.ErrWeakPassword) {
		t.Error("11 runes of multi-byte text was accepted — the length is being measured in bytes")
	}
}

func TestEmptyOrMalformedHashVerifiesFalse(t *testing.T) {
	good, _ := identity.HashPassword(goodPassword)
	parts := strings.Split(good, "$")

	for name, encoded := range map[string]string{
		"empty":           "",
		"garbage":         "garbage",
		"truncated":       strings.Join(parts[:4], "$"),
		"bad base64 salt": strings.Join([]string{parts[0], parts[1], parts[2], parts[3], "!!!!", parts[5]}, "$"),
		"bad base64 key":  strings.Join([]string{parts[0], parts[1], parts[2], parts[3], parts[4], "!!!!"}, "$"),
		"wrong algorithm": strings.Replace(good, "argon2id", "argon2i", 1),
		"bad version":     strings.Replace(good, fmt.Sprintf("v=%d", argon2.Version), "v=1", 1),
		// A stored zero would make argon2.IDKey panic, and a database column is
		// exactly where one could arrive from.
		"zero memory": strings.Replace(good, parts[3], "m=0,t=3,p=4", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if identity.VerifyPassword(encoded, goodPassword) {
				t.Errorf("a %s hash verified", name)
			}
		})
	}
}

// TestVerifyAgainstEmptyHashStillCostsTime is the anti-enumeration property,
// measured rather than read off the source.
//
// ⚠ The threshold is a RATIO against a real verification in the same run, not an
// absolute duration, and it is deliberately loose. The defect it catches is
// microseconds versus tens of milliseconds — four orders of magnitude — so a
// tight threshold would buy nothing and flake on a loaded CI box.
func TestVerifyAgainstEmptyHashStillCostsTime(t *testing.T) {
	h, _ := identity.HashPassword(goodPassword)

	start := time.Now()
	identity.VerifyPassword(h, "some wrong password")
	real_ := time.Since(start)

	start = time.Now()
	identity.VerifyPassword("", "some wrong password")
	empty := time.Since(start)

	if real_ <= 0 {
		t.Fatal("a real verification took no measurable time; the clock is unusable here")
	}
	if empty*3 < real_ {
		t.Errorf("verifying against an empty hash took %v where a real one took %v. A missing "+
			"account must not answer faster than a real one — the difference is an "+
			"account-enumeration oracle, and identical error messages cannot hide it",
			empty, real_)
	}
}

// TestUserStructHasNoPasswordField pins the invariant structurally.
//
// The hash cannot reach a handler that renders a user table, because there is
// nowhere on the struct it renders for the hash to sit.
func TestUserStructHasNoPasswordField(t *testing.T) {
	typ := reflect.TypeOf(core.User{})
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(name, "password") || strings.Contains(name, "passwd") {
			t.Errorf("core.User has field %q — the hash can now reach anything that renders a "+
				"user, and the dashboard renders a user table", typ.Field(i).Name)
		}
	}
	if typ.NumField() == 0 {
		t.Fatal("core.User has no fields at all, so this assertion proved nothing")
	}
}

func TestSetAndReadPasswordHash(t *testing.T) {
	h := newIdentityHarness(t)
	admin := h.bootstrapAdmin(t)

	hash, err := identity.HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := h.repo.SetPasswordHash(context.Background(), admin.ID, hash); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}

	gotID, gotHash, err := h.repo.PasswordHashByEmail(context.Background(), admin.Email)
	if err != nil {
		t.Fatalf("PasswordHashByEmail: %v", err)
	}
	if gotID != admin.ID {
		t.Errorf("user id = %q, want %q", gotID, admin.ID)
	}
	if !identity.VerifyPassword(gotHash, goodPassword) {
		t.Error("the hash that came back does not verify the password that was stored")
	}
}

// TestPasswordHashByEmailMatchesTheStoredFormOnly documents where normalisation
// lives.
//
// ⚠ The repository matches the column EXACTLY. Emails are normalised on the way
// in by identity.CreateUser, so the stored form is already lower-cased and
// trimmed — and a login form is not, and a human typing their own address is
// not. Normalising is therefore the CALLER's job, and identity.Login does it
// (T2). This test pins that division so nobody later "fixes" it in both places
// or neither.
func TestPasswordHashByEmailMatchesTheStoredFormOnly(t *testing.T) {
	h := newIdentityHarness(t)
	admin := h.bootstrapAdmin(t)
	hash, _ := identity.HashPassword(goodPassword)
	if err := h.repo.SetPasswordHash(context.Background(), admin.ID, hash); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}

	// The stored form resolves.
	gotID, _, err := h.repo.PasswordHashByEmail(context.Background(), admin.Email)
	if err != nil || gotID != admin.ID {
		t.Fatalf("the stored form did not resolve: id=%q err=%v", gotID, err)
	}

	// An unnormalised form does not — which is exactly why Login normalises.
	if _, _, err := h.repo.PasswordHashByEmail(context.Background(),
		"  "+strings.ToUpper(admin.Email)+"  "); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("an unnormalised email resolved (err=%v). That is not wrong in itself, but it "+
			"means normalisation happens in two places, and the two will drift", err)
	}
}

func TestPasswordHashByEmailOnUnknownEmail(t *testing.T) {
	h := newIdentityHarness(t)

	id, hash, err := h.repo.PasswordHashByEmail(context.Background(), "nobody@example.com")
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("err = %v, want core.ErrNotFound", err)
	}
	if id != "" || hash != "" {
		t.Errorf("unknown email returned id=%q hash=%q — a partially populated result is one a "+
			"caller might verify against", id, hash)
	}
}

func TestNewUserHasNoPassword(t *testing.T) {
	// The migration's DEFAULT '' is what makes every pre-existing account, and
	// every client and worker, unable to log in with a form.
	h := newIdentityHarness(t)
	admin := h.bootstrapAdmin(t)

	_, hash, err := h.repo.PasswordHashByEmail(context.Background(), admin.Email)
	if err != nil {
		t.Fatalf("PasswordHashByEmail: %v", err)
	}
	if hash != "" {
		t.Errorf("a freshly bootstrapped admin already has a password hash %q", hash)
	}
}
