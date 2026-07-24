// Command coveragegate is a CI check over a `go test -coverprofile` profile.
//
// It measures statement-weighted coverage for the packages under pkg/ only
// — excluding protos/ (generated stubs, which drag the raw ./... aggregate
// from ~69% down to ~39% and would make a single repo-wide threshold
// meaningless) and cmd/ (thin entrypoint wiring, not representative of
// tested business logic) — and compares it against a checked-in baseline
// with a tolerance, so a small drop from run-to-run noise doesn't fail while
// a real regression does. See docs/COVERAGE_POLICY.md for the design
// rationale (NodeVault issue #50).
//
// This check is intentionally NOT wired into the branch ruleset's required
// checks yet: per issue #50's acceptance criteria, it must be observed
// passing/failing correctly on real PRs first. Until that observation
// window closes and someone adds it to the ruleset, a failing run here is
// informational only and does not block merges.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type baseline struct {
	BaselinePercent  float64 `yaml:"baseline_percent"`
	TolerancePercent float64 `yaml:"tolerance_percent"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "coveragegate:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("coveragegate", flag.ContinueOnError)
	profilePath := fs.String("profile", "coverage.out", "path to a go test -coverprofile output file")
	baselinePath := fs.String("baseline", ".coverage-baseline.yaml", "path to the checked-in coverage baseline")
	scopeSegment := fs.String("scope-segment", "pkg", "only count profile lines under this path segment")
	if err := fs.Parse(args); err != nil {
		return err
	}

	profile, err := os.ReadFile(*profilePath) //nolint:gosec // path is a CI-controlled flag, not user input
	if err != nil {
		return fmt.Errorf("read coverage profile: %w", err)
	}
	base, err := loadBaseline(*baselinePath)
	if err != nil {
		return fmt.Errorf("load baseline: %w", err)
	}

	stmts, covered, err := scopedCoverage(profile, *scopeSegment)
	if err != nil {
		return fmt.Errorf("parse coverage profile: %w", err)
	}
	if stmts == 0 {
		return fmt.Errorf("no profile lines matched scope segment %q (empty profile, or wrong segment)", *scopeSegment)
	}

	current := 100 * float64(covered) / float64(stmts)
	floor := base.BaselinePercent - base.TolerancePercent

	_, _ = fmt.Fprintf(stdout, "coveragegate: %s/ coverage = %.1f%% (baseline %.1f%%, tolerance %.1f, floor %.1f)\n",
		*scopeSegment, current, base.BaselinePercent, base.TolerancePercent, floor)

	if current < floor {
		return fmt.Errorf("coverage %.1f%% is below floor %.1f%% (baseline %.1f%% - tolerance %.1f)",
			current, floor, base.BaselinePercent, base.TolerancePercent)
	}
	_, _ = fmt.Fprintln(stdout, "coveragegate: within tolerance of baseline")
	return nil
}

func loadBaseline(path string) (baseline, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is a CI-controlled flag, not user input
	if err != nil {
		return baseline{}, err
	}
	var b baseline
	if err := yaml.Unmarshal(data, &b); err != nil {
		return baseline{}, err
	}
	if b.BaselinePercent <= 0 || b.BaselinePercent > 100 {
		return baseline{}, fmt.Errorf("baseline_percent %.1f is out of range (0,100]", b.BaselinePercent)
	}
	if b.TolerancePercent < 0 {
		return baseline{}, fmt.Errorf("tolerance_percent %.1f must not be negative", b.TolerancePercent)
	}
	return b, nil
}

// scopedCoverage sums statement/covered counts from a go test -coverprofile
// text profile, keeping only lines whose file path has scopeSegment as a
// path segment (e.g. scopeSegment "pkg" matches
// ".../pkg/build/service.go:12.3,14.4 3 1" but not ".../cmd/..." or
// ".../protos/..."). The mode line and any unparseable line are skipped.
func scopedCoverage(profile []byte, scopeSegment string) (stmts, covered int64, err error) {
	scanner := bufio.NewScanner(strings.NewReader(string(profile)))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		filePart, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if !hasPathSegment(filePart, scopeSegment) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}
		// fields is [statementRange, numStmts, count]; only the profile's
		// own middle/last fields matter here — the range itself is unused.
		n, nErr := strconv.ParseInt(fields[len(fields)-2], 10, 64)
		if nErr != nil {
			return 0, 0, fmt.Errorf("parse numStmt in %q: %w", line, nErr)
		}
		count, cErr := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if cErr != nil {
			return 0, 0, fmt.Errorf("parse count in %q: %w", line, cErr)
		}
		stmts += n
		if count > 0 {
			covered += n
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return 0, 0, scanErr
	}
	return stmts, covered, nil
}

func hasPathSegment(path, segment string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == segment {
			return true
		}
	}
	return false
}
