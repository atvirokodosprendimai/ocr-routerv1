package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// readPassword is the seam the tests substitute.
//
// The real implementation reads from the terminal with echo disabled, which
// needs a TTY and therefore cannot run in CI. Everything ELSE about the
// prompt — that it asks twice, that a mismatch is refused, that a short password
// is refused before anything is written — is covered by substituting this.
// `term.ReadPassword` itself is covered by the human sign-off, and saying so is
// more honest than a test that fakes a terminal and proves nothing about one.
var readPassword = promptForPassword

// ErrNoPasswordSource means there was nothing to read a password FROM — no
// `--password` and no terminal to ask at.
//
// ⚠ It is distinct from every other prompt failure on purpose. "Nobody supplied
// one" is a normal outcome that leaves an account token-only, exactly as every
// account behaved before ADR-0003. "The two entries did not match" is a mistake
// and must stop the command. Collapsing the two would make a mistyped password
// silently mean "no password", which is the failure the confirmation exists to
// prevent.
var ErrNoPasswordSource = errors.New("no password supplied and no terminal to prompt at")

// promptForPassword asks twice, with echo disabled.
//
// ⚠ TWICE, and the two must match. A mistyped invisible password silently
// becomes the real one, and the operator discovers it by being locked out of the
// dashboard with nothing anywhere explaining why.
func promptForPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Not a terminal, so there is nowhere to ask. Reading an echoed password
		// from a pipe would put it in whatever is feeding us.
		return "", ErrNoPasswordSource
	}

	fmt.Fprint(os.Stderr, prompt+": ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}

	fmt.Fprint(os.Stderr, "Confirm: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}

	if string(first) != string(second) {
		return "", errors.New("the two entries do not match")
	}
	return string(first), nil
}

// readPasswordFrom is the non-terminal reader the tests install.
//
// It reads two lines and applies the same matching rule, so the confirmation
// behaviour is exercised even though the terminal handling is not.
func readPasswordFrom(r io.Reader) func(string) (string, error) {
	sc := bufio.NewScanner(r)
	return func(string) (string, error) {
		if !sc.Scan() {
			// Nothing to read: the stand-in for "no terminal".
			return "", ErrNoPasswordSource
		}
		first := sc.Text()
		if !sc.Scan() {
			return "", errors.New("no confirmation supplied")
		}
		if sc.Text() != first {
			return "", errors.New("the two entries do not match")
		}
		return first, nil
	}
}

// resolvePassword takes the flag, or prompts.
func resolvePassword(c *cli.Command, prompt string) (string, error) {
	if p := c.String("password"); p != "" {
		return p, nil
	}
	return readPassword(prompt)
}

// runSetPassword changes an administrator's dashboard password.
func runSetPassword(ctx context.Context, c *cli.Command) error {
	app, err := buildApp(configFrom(c))
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()

	email := c.String("email")
	password, err := resolvePassword(c, "New password for "+email)
	if err != nil {
		return err
	}

	if err := app.Ident.SetPassword(ctx, email, password, time.Now()); err != nil {
		return err
	}

	// ⚠ The password is never echoed, not even masked. A bootstrap that prints
	// it puts it in the operator's scrollback and in any CI log that captured
	// the run.
	fmt.Printf("password updated for %s\n", strings.ToLower(strings.TrimSpace(email)))
	fmt.Println("Sign in at /admin/login. TLS is required: the session cookie is Secure.")
	return nil
}
