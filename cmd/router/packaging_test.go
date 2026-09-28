package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ⚠ AN EXAMPLE FILE ROTS SILENTLY, WHICH IS WHY THIS TEST EXISTS.
//
// compose.yml.example passes flags to the binaries by name. Rename a flag in
// code and nothing anywhere fails: the example still parses as YAML, `docker
// compose config` still validates it, and the failure arrives as
// `flag provided but not defined` on somebody's first run — after they have
// copied the file and edited it.
//
// The same is true of the COMMENTED blocks, and they are checked too. A
// commented example is the one a reader uncomments, so a stale flag in it is
// worse than a stale flag in the live part: nobody has ever run it.

var (
	// A flag as the example passes it: a YAML sequence item holding "--name".
	exampleFlag = regexp.MustCompile(`-\s+"--([a-z][a-z-]*)"`)
	// A flag or command as urfave/cli declares it.
	declaredName = regexp.MustCompile(`Name:\s*"([a-z][a-z-]*)"`)
)

// TestComposeExampleOnlyUsesFlagsThatExist is the anti-rot check.
//
// ⚠ IT IS AN EXISTENCE CHECK, NOT AN ATTRIBUTION ONE, and the difference is
// worth stating rather than leaving for a reader to assume: the declared set is
// a SUPERSET — it includes subcommand names (`admin`, `bootstrap`) as well as
// flags, and it pools the router's names with the worker's. So a router flag
// wrongly passed to the worker would pass this test. What it catches is the
// defect that actually happens: a flag that exists NOWHERE, because it was
// renamed or removed in code and left behind in the example.
func TestComposeExampleOnlyUsesFlagsThatExist(t *testing.T) {
	example := readRepoFile(t, "../../compose.yml.example")
	declared := declaredNames(t, "main.go", "../worker/main.go")

	// Strip comment markers: the worker and proxy blocks are commented out, and
	// a stale flag in them is exactly as broken as one in the live part.
	uncommented := regexp.MustCompile(`(?m)^\s*#\s?`).ReplaceAllString(example, "")

	found := exampleFlag.FindAllStringSubmatch(uncommented, -1)
	if len(found) == 0 {
		t.Fatal("no flags found in compose.yml.example at all, so this assertion proved " +
			"nothing — the example's shape changed and this regex no longer matches it")
	}

	var checked int
	for _, m := range found {
		flag := m[1]
		checked++
		if !declared[flag] {
			t.Errorf("compose.yml.example passes --%s, which neither cmd/router nor cmd/worker "+
				"declares. A reader copying this file gets `flag provided but not defined` on "+
				"their first run", flag)
		}
	}
	t.Logf("checked %d flag uses in compose.yml.example", checked)
}

// TestComposeExampleKeepsTheStateOnAVolume is the one substantive claim in the
// example that is not about flag spelling.
//
// The database holds the customers, the tokens and the CREDIT LEDGER, and the
// blob directory holds uploaded files. An example that pointed --db somewhere
// inside the container's writable layer would work perfectly, pass every other
// check here, and lose the ledger on the first `docker compose up --force-recreate`
// — with no way to reconstruct it from anything else.
func TestComposeExampleKeepsTheStateOnAVolume(t *testing.T) {
	example := readRepoFile(t, "../../compose.yml.example")

	if !strings.Contains(example, "router-data:/data") {
		t.Error("the example does not mount a volume at /data, so the database and the blobs " +
			"live in the container's writable layer and die with the container")
	}
	for _, path := range []string{`"/data/router.db"`, `"/data/blobs"`} {
		if !strings.Contains(example, path) {
			t.Errorf("the example does not point at %s, so state lands outside the volume it "+
				"mounts", path)
		}
	}
	// And the Dockerfile has to declare it, or `docker run` without compose puts
	// the database in a layer too.
	dockerfile := readRepoFile(t, "../../Dockerfile")
	if !strings.Contains(dockerfile, `VOLUME ["/data"]`) {
		t.Error("the Dockerfile does not declare /data as a volume")
	}
}

// TestTheRouterImageDoesNotPublishMetrics pins a decision, not a preference.
//
// ADR-0001 task T11 binds /metrics to loopback because queue depths and
// throughput say how much work a customer pushes, and the main listener faces
// the internet. In a container, loopback is container-local — which preserves
// that property for free. Publishing the port, or moving the listener to
// 0.0.0.0, undoes a decision rather than configuring one.
func TestTheRouterImageDoesNotPublishMetrics(t *testing.T) {
	dockerfile := readRepoFile(t, "../../Dockerfile")
	example := readRepoFile(t, "../../compose.yml.example")

	if strings.Contains(dockerfile, "EXPOSE 9090") {
		t.Error("the Dockerfile EXPOSEs the metrics port, which ADR-0001 T11 deliberately binds " +
			"to loopback")
	}
	// The flag may appear in prose explaining why not to use it; what must not
	// appear is the example actually passing it.
	if exampleFlag.MatchString(strings.ReplaceAll(example, "\n", " ")) {
		for _, m := range exampleFlag.FindAllStringSubmatch(example, -1) {
			if m[1] == "metrics-addr" {
				t.Error("the example passes --metrics-addr, which moves the metrics listener off " +
					"loopback — see the note at the bottom of the file for the sidecar instead")
			}
		}
	}
	if strings.Contains(example, `"9090:9090"`) {
		t.Error("the example publishes the metrics port to the host")
	}
}

// TestTheInsecureCookieFlagIsExplainedWhereItIsUsed.
//
// ⚠ This is the single most consequential line in the example. Without it the
// dashboard cannot be signed into over http://localhost at all — the login
// handler refuses rather than setting a Secure cookie the browser would discard.
// With it, the session cookie travels in clear text. A reader who copies the
// file for a real deployment and does not notice has a plaintext admin session,
// so the warning has to travel WITH the flag rather than living in a README.
func TestTheInsecureCookieFlagIsExplainedWhereItIsUsed(t *testing.T) {
	example := readRepoFile(t, "../../compose.yml.example")

	i := strings.Index(example, `"--insecure-cookies"`)
	if i < 0 {
		t.Fatal("the example does not use --insecure-cookies, so signing in on localhost is " +
			"impossible and this test's subject has changed")
	}
	// The 1200 characters before it are the comment block that must carry the
	// warning; a reader scanning the command sees them together.
	start := i - 1200
	if start < 0 {
		start = 0
	}
	preamble := example[start:i]
	for _, want := range []string{"clear text", "TLS"} {
		if !strings.Contains(preamble, want) {
			t.Errorf("the comment beside --insecure-cookies does not mention %q. The warning has "+
				"to be next to the flag: a reader copying this file will not go looking for it",
				want)
		}
	}
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// declaredNames pools every flag and command name the given sources declare.
func declaredNames(t *testing.T, paths ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, p := range paths {
		for _, m := range declaredName.FindAllStringSubmatch(readRepoFile(t, p), -1) {
			out[m[1]] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("no flag names found in the command sources, so the comparison below would " +
			"pass whatever the example says")
	}
	return out
}
