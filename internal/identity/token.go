package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// tokenBytes is the entropy behind a token. 32 bytes is far past any brute-force
// concern and keeps the printed string a manageable length.
const tokenBytes = 32

// PrefixFor returns the human-readable marker a token of this role carries.
//
// ⚠ The prefix is a LABEL, never authority. It exists so an operator who finds a
// token in a log or a config can tell at a glance what it could do — the
// difference between "rotate this at leisure" and "rotate this now". The role
// used for any authorisation decision is read from the database row; if it were
// parsed from this string, anyone could mint themselves an admin by typing one.
func PrefixFor(r core.Role) string {
	switch r {
	case core.RoleAdmin:
		return "ocr_a_"
	case core.RoleWorker:
		return "ocr_w_"
	default:
		return "ocr_c_"
	}
}

// GenerateToken mints a new bearer token in plaintext.
//
// The returned string is the only copy that will ever exist: the store keeps
// HashToken(tok) and nothing else.
func GenerateToken(r core.Role) (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	return PrefixFor(r) + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken returns the hex SHA-256 of a token.
//
// A plain hash rather than a password KDF is the right choice HERE and the
// reasoning is worth stating, because the usual advice points the other way: a
// token is 32 bytes of uniform randomness, not a human-chosen password. There is
// no dictionary to attack and no work factor that would meaningfully slow an
// attacker who already has the database. What a KDF would add is latency on
// every single request.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}
