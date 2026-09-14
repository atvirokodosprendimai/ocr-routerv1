package identity_test

import (
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
)

func TestTokenHashIsStable(t *testing.T) {
	a := identity.HashToken("ocr_c_abc")
	b := identity.HashToken("ocr_c_abc")
	if a != b {
		t.Errorf("hashing the same token twice gave %q and %q", a, b)
	}
	if a == identity.HashToken("ocr_c_abd") {
		t.Error("two different tokens hashed to the same value")
	}
	if len(a) != 64 {
		t.Errorf("hash length = %d, want 64 hex characters of SHA-256", len(a))
	}
	if strings.Contains(a, "ocr_c_") {
		t.Error("the hash contains the plaintext")
	}
}

func TestGenerateTokenShape(t *testing.T) {
	seen := map[string]struct{}{}
	for _, r := range []core.Role{core.RoleAdmin, core.RoleClient, core.RoleWorker} {
		for i := 0; i < 200; i++ {
			tok, err := identity.GenerateToken(r)
			if err != nil {
				t.Fatalf("GenerateToken(%s): %v", r, err)
			}
			if _, dup := seen[tok]; dup {
				t.Fatalf("GenerateToken collided: %q", tok)
			}
			seen[tok] = struct{}{}

			want := identity.PrefixFor(r)
			if !strings.HasPrefix(tok, want) {
				t.Errorf("token %q does not start with %q — the prefix is how an operator "+
					"judges a leaked token's blast radius at a glance", tok, want)
			}
			if len(tok) < len(want)+40 {
				t.Errorf("token %q is too short to carry 32 bytes of entropy", tok)
			}
		}
	}
}

func TestPrefixesAreDistinct(t *testing.T) {
	a := identity.PrefixFor(core.RoleAdmin)
	c := identity.PrefixFor(core.RoleClient)
	w := identity.PrefixFor(core.RoleWorker)
	if a == c || c == w || a == w {
		t.Errorf("role prefixes collide: admin=%q client=%q worker=%q", a, c, w)
	}
}

func TestMintTokenReturnsPlaintextOnce(t *testing.T) {
	s := newService(t)
	_, adminTok := admin(t, s)
	ap := principalOf(t, s, adminTok)
	ctx := t.Context()

	u, _ := s.CreateUser(ctx, ap, "c@example.com", core.RoleClient, base)
	tokenID, plaintext, err := s.MintToken(ctx, ap, u.ID, core.RoleClient, "laptop", base)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if plaintext == "" {
		t.Fatal("MintToken returned an empty plaintext")
	}

	// Every read path must expose the hash and never the plaintext. "Shown once"
	// has to be a property of the storage, not a policy someone remembers.
	tokens, err := s.ListTokens(ctx, ap, u.ID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("ListTokens returned %d rows, want 1", len(tokens))
	}
	if tokens[0].ID != tokenID {
		t.Errorf("token id = %q, want %q", tokens[0].ID, tokenID)
	}
	if tokens[0].Hash == plaintext {
		t.Error("the stored hash IS the plaintext — the token was stored in the clear")
	}
	if strings.Contains(tokens[0].Hash, plaintext) {
		t.Error("the stored hash contains the plaintext")
	}
	if tokens[0].Hash != identity.HashToken(plaintext) {
		t.Error("the stored hash is not SHA-256 of the plaintext")
	}
}
