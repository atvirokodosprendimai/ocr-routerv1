package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/atvirokodosprendimai/ocr-router/internal/client"
)

// reporter renders progress.
//
// ⚠ IT WRITES TO STDERR, ALWAYS, and that is not a preference. `-o -` puts the
// RESULT on stdout, which is what makes this tool composable — a progress line
// on the same stream corrupts every pipe it is used in.
type reporter struct {
	w io.Writer
	// tty is whether w is a terminal. On a terminal the line is rewritten in
	// place; off one it is a plain line per transition, because `\r` in a CI log
	// is noise and in a pipe is corruption.
	tty   bool
	quiet bool
	// width tracks the last line's length so a shorter one fully erases it.
	width int
}

func newReporter(w io.Writer, quiet bool) *reporter {
	r := &reporter{w: w, quiet: quiet}
	if f, ok := w.(*os.File); ok {
		r.tty = term.IsTerminal(int(f.Fd()))
	}
	return r
}

// stage renders one transition.
func (r *reporter) stage(s client.Stage, detail string) {
	if r.quiet {
		return
	}

	msg := describe(s, detail)
	if !r.tty {
		fmt.Fprintln(r.w, msg)
		return
	}

	// Pad to erase whatever the previous, possibly longer, line left behind.
	pad := ""
	if n := r.width - len(msg); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	fmt.Fprintf(r.w, "\r%s%s", msg, pad)
	r.width = len(msg)

	if s == client.StageDone {
		fmt.Fprintln(r.w)
	}
}

// done closes an in-place line that did not reach StageDone, so an error message
// does not land on the same row as a half-written progress line.
func (r *reporter) done() {
	if !r.quiet && r.tty && r.width > 0 {
		fmt.Fprintln(r.w)
		r.width = 0
	}
}

// describe names a stage in words an operator can act on.
//
// ⚠ NO PERCENTAGE. The router reports states, not fractions — it cannot know how
// long a worker will take — and a fabricated percentage is worse than an honest
// state name, because it invites the caller to estimate from it.
func describe(s client.Stage, detail string) string {
	switch s {
	case client.StageUploading:
		return "uploading…"
	case client.StageWaiting:
		if detail != "" {
			return "waiting for " + short(detail) + "…"
		}
		return "waiting…"
	case client.StageCollecting:
		return "collecting…"
	case client.StageDone:
		return "done"
	default:
		return string(s)
	}
}

func short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
