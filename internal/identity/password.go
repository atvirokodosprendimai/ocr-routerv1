package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// ErrWeakPassword rejects a password below the minimum length.
//
// ⚠ It is returned only when SETTING a password, never when verifying one. On
// the login path every failure must be indistinguishable (see Login), and a
// distinct "too short" error there would tell an attacker something about the
// stored credential.
var ErrWeakPassword = errors.New("password must be at least 12 characters")

// MinPasswordRunes is the only password rule.
//
// Length, and nothing else. Mandated symbol classes reliably produce
// `Password1!` and a sticky note; length is the requirement that survives
// contact with how people actually choose passwords. Counted in RUNES so a
// passphrase in any script is measured the way its author would count it.
const MinPasswordRunes = 12

// argon2 parameters. They are written INTO each hash, so raising them later
// applies to new passwords without invalidating any existing one.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// params are the cost settings for one hash, read back out of the stored string.
type params struct {
	time    uint32
	memory  uint32
	threads uint8
}

// HashPassword returns a PHC-encoded argon2id hash.
//
// The format is the standard `$argon2id$v=19$m=…,t=…,p=…$salt$hash`, and the
// parameters travel WITH the hash for a reason: it is what lets the cost be
// raised later without a migration or a forced reset. Verification reads the
// parameters out of the string it was given, never from the constants above.
func HashPassword(plain string) (string, error) {
	if utf8.RuneCountInString(plain) < MinPasswordRunes {
		return "", ErrWeakPassword
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}

	key := argon2.IDKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return encodeHash(params{time: argonTime, memory: argonMemory, threads: argonThreads}, salt, key), nil
}

// VerifyPassword reports whether plain matches the encoded hash.
//
// ⚠ IT DELIBERATELY BURNS TIME WHEN THERE IS NOTHING TO VERIFY. An empty or
// malformed `encoded` — the shape of a user who does not exist, or one with no
// password set — still runs a full derivation against a fixed dummy hash before
// returning false. Without that, "no such account" returns in microseconds where
// a real check takes tens of milliseconds, and the login endpoint becomes an
// account-enumeration oracle that no amount of identical error messages can
// hide.
//
// It returns a bool rather than an error because there is exactly one useful
// answer, and an error return is one a caller can forget to check.
func VerifyPassword(encoded, plain string) bool {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		// Burn comparable time, then fail. The result is discarded; what matters
		// is that the caller waited for it.
		dummy := argon2.IDKey([]byte(plain), dummySalt, argonTime, argonMemory, argonThreads, argonKeyLen)
		_ = subtle.ConstantTimeCompare(dummy, dummyKey)
		return false
	}

	got := argon2.IDKey([]byte(plain), salt, p.time, p.memory, p.threads, argonKeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummySalt and dummyKey feed the no-op derivation above. Fixed values are fine:
// nothing is secret about them, and their only job is to make the work happen.
var (
	dummySalt = []byte("ocr-router-dummy")
	dummyKey  = make([]byte, argonKeyLen)
)

// encodeHash renders the PHC string.
func encodeHash(p params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// decodeHash parses a PHC string back into its parameters, salt and key.
//
// Every malformed shape is one error: this is fed untrusted-ish data from a
// database column, and a caller that distinguished the failures would only be
// tempted to report them.
func decodeHash(encoded string) (params, []byte, []byte, error) {
	var zero params
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return zero, nil, nil, errors.New("identity: not an argon2id hash")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return zero, nil, nil, errors.New("identity: unsupported argon2 version")
	}

	var p params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return zero, nil, nil, errors.New("identity: unreadable argon2 parameters")
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 {
		// argon2.IDKey panics on a zero parameter, and a stored hash is exactly
		// the place a zero could arrive from.
		return zero, nil, nil, errors.New("identity: zero argon2 parameter")
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return zero, nil, nil, errors.New("identity: unreadable salt")
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return zero, nil, nil, errors.New("identity: unreadable key")
	}
	return p, salt, key, nil
}
