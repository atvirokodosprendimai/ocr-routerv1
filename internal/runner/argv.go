package runner

import (
	"fmt"
	"sort"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// Job is what the runner needs to know to execute one unit of work.
type Job struct {
	ID string
	// InputPath is the materialised source file, or empty when the job has no
	// blob — the crawler shape, where the service fetches its own input from a
	// parameter.
	InputPath string
	Params    map[string]string
}

// BuildArgv assembles the command line for a job.
//
// The shape is:
//
//	<cmd> [-i <input>] -o - [--<key> <value>]...
//
// ⚠ THIS IS THE PROJECT'S ONE UNTRUSTED-INPUT-TO-PROCESS BOUNDARY, and the rules
// that make it safe are all structural rather than sanitising:
//
//   - The result is an ARGV SLICE, executed with exec.CommandContext. There is
//     no shell anywhere on this path, so a value containing `;`, backticks or
//     `$(…)` is a sequence of bytes and not syntax. Nothing needs escaping
//     because nothing is ever parsed.
//   - Each value is its OWN element and is never concatenated with its flag. A
//     value cannot split itself into two arguments.
//   - KEYS are validated, because a key becomes a flag NAME. Values are data and
//     are deliberately not pattern-checked: constraining them would break
//     legitimate input (a URL, a sentence) for no security gain.
//
// Keys are sorted so the argv is deterministic — a map's iteration order is not
// a contract, and a test that asserts on argv needs one.
//
// ⚠ RESIDUAL, DOCUMENTED RATHER THAN HIDDEN: a VALUE beginning with `-` may be
// read as a flag by the child program. We do not attempt to solve that — the
// child's flag parser is not ours to model, and a partial mitigation would read
// as a solved problem. Point --cmd at a program that honours `--`, or at a
// two-line wrapper.
func BuildArgv(cmd string, j Job) ([]string, error) {
	if cmd == "" {
		return nil, fmt.Errorf("%w: no command configured", core.ErrInvalidParam)
	}

	argv := []string{cmd}
	if j.InputPath != "" {
		argv = append(argv, "-i", j.InputPath)
	}
	argv = append(argv, "-o", "-")

	keys := make([]string, 0, len(j.Params))
	for k := range j.Params {
		if !core.ValidParamKey(k) {
			return nil, fmt.Errorf("%w: parameter name %q", core.ErrInvalidParam, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		argv = append(argv, "--"+k, j.Params[k])
	}
	return argv, nil
}
