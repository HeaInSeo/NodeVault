// Command govulncheckgate is a CI gate over `govulncheck -format json` output.
//
// govulncheck's -format json mode always exits 0 regardless of findings (only
// its default text mode uses the exit code to signal vulnerabilities), so a
// bare `continue-on-error: true` step masks every finding forever — this was
// discovered hiding 5 real, call-graph-reachable vulnerabilities in NodeVault's
// CI. This gate replaces that mask: a reachable finding passes only if it
// exactly matches a non-expired entry (id + module + package) in an
// exceptions file. An unlisted finding, an id/module/package mismatch, an
// expired exception, or unparseable/empty input all fail with a non-zero
// exit code.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const expiresLayout = "2006-01-02"

type exception struct {
	ID            string `yaml:"id"`
	Module        string `yaml:"module"`
	Package       string `yaml:"package"`
	Reason        string `yaml:"reason"`
	Owner         string `yaml:"owner"`
	Expires       string `yaml:"expires"`
	TrackingIssue string `yaml:"tracking_issue"`
}

type exceptionsFile struct {
	Exceptions []exception `yaml:"exceptions"`
}

// govulncheckMessage mirrors the subset of govulncheck's JSON message
// envelope (golang.org/x/vuln/exp/govulncheck) this gate needs.
type govulncheckMessage struct {
	Finding *finding `json:"finding"`
}

type finding struct {
	OSV   string  `json:"osv"`
	Trace []frame `json:"trace"`
}

type frame struct {
	Module   string `json:"module"`
	Package  string `json:"package"`
	Function string `json:"function"`
}

// occurrence is the module/package pair a reachable finding was traced to,
// taken from the deepest trace frame (index 0) — the frame closest to the
// actual vulnerable symbol.
type occurrence struct {
	module string
	pkg    string
	trace  []frame
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "govulncheckgate:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("govulncheckgate", flag.ContinueOnError)
	exceptionsPath := fs.String("exceptions", "security/govulncheck-exceptions.yaml", "path to the exceptions YAML file")
	inputPath := fs.String("input", "-", "path to govulncheck -format json output ('-' for stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	exFile, err := loadExceptions(*exceptionsPath)
	if err != nil {
		return err
	}

	input, closeInput, err := openInput(*inputPath, stdin)
	if err != nil {
		return err
	}
	defer closeInput()

	messageCount, reachable, err := parseReachableFindings(input)
	if err != nil {
		return fmt.Errorf("parse govulncheck output: %w", err)
	}
	if messageCount == 0 {
		return fmt.Errorf("govulncheck produced no output at all — treat as a scan failure, not a clean pass")
	}

	return evaluate(exFile, reachable, stdout)
}

func loadExceptions(path string) (exceptionsFile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is a CI-controlled flag, not user input
	if err != nil {
		return exceptionsFile{}, fmt.Errorf("read exceptions file: %w", err)
	}
	var exFile exceptionsFile
	if err := yaml.Unmarshal(data, &exFile); err != nil {
		return exceptionsFile{}, fmt.Errorf("parse exceptions file: %w", err)
	}
	return exFile, nil
}

func openInput(path string, stdin io.Reader) (io.Reader, func(), error) {
	if path == "-" {
		return stdin, func() {}, nil
	}
	f, err := os.Open(path) //nolint:gosec // path is a CI-controlled flag, not user input
	if err != nil {
		return nil, func() {}, fmt.Errorf("open govulncheck output: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}

// parseReachableFindings decodes a stream of govulncheck JSON messages and
// returns the total message count (so callers can detect empty/truncated
// input) plus every reachable finding's occurrence, grouped by OSV ID.
func parseReachableFindings(r io.Reader) (count int, reachable map[string][]occurrence, err error) {
	dec := json.NewDecoder(r)
	reachable = make(map[string][]occurrence)
	for {
		var msg govulncheckMessage
		if decErr := dec.Decode(&msg); decErr != nil {
			if decErr == io.EOF {
				break
			}
			return count, nil, decErr
		}
		count++
		if msg.Finding == nil || len(msg.Finding.Trace) == 0 {
			continue
		}
		deepest := msg.Finding.Trace[0]
		reachable[msg.Finding.OSV] = append(reachable[msg.Finding.OSV], occurrence{
			module: deepest.Module,
			pkg:    deepest.Package,
			trace:  msg.Finding.Trace,
		})
	}
	return count, reachable, nil
}

func evaluate(exFile exceptionsFile, reachable map[string][]occurrence, stdout io.Writer) error {
	now := time.Now()
	usedException := make([]bool, len(exFile.Exceptions))
	var failures []string

	for id, occurrences := range reachable {
		if covered, msgs := coveredByExceptions(id, occurrences, exFile.Exceptions, usedException, now); !covered {
			failures = append(failures, msgs...)
		}
	}

	const unusedExceptionFmt = "WARNING: exception %s (%s / %s) is no longer detected as reachable" +
		" — consider removing it (owner: %s, tracking: %s)\n"
	for i, exc := range exFile.Exceptions {
		if !usedException[i] {
			_, _ = fmt.Fprintf(stdout, unusedExceptionFmt, exc.ID, exc.Module, exc.Package, exc.Owner, exc.TrackingIssue)
		}
	}

	if len(failures) > 0 {
		for _, f := range failures {
			_, _ = fmt.Fprintln(stdout, "FAIL:", f)
		}
		return fmt.Errorf("%d unapproved govulncheck finding(s)", len(failures))
	}

	_, _ = fmt.Fprintln(stdout, "govulncheck gate: all reachable findings are covered by non-expired exceptions")
	return nil
}

// coveredByExceptions reports whether every occurrence of id is covered by
// at least one non-expired exception whose module matches exactly and whose
// package matches or is a parent of the occurrence's package (so one
// exception can cover a vulnerable module's internal subpackages, e.g.
// golang.org/x/crypto/openpgp covering golang.org/x/crypto/openpgp/armor).
// Some finding traces carry only module-level granularity (empty package);
// those match on module alone since there is no finer detail to compare.
func coveredByExceptions(
	id string, occurrences []occurrence, exceptions []exception, used []bool, now time.Time,
) (covered bool, failures []string) {
	for _, occ := range occurrences {
		matched := false
		for i, exc := range exceptions {
			if exc.ID != id || exc.Module != occ.module {
				continue
			}
			if occ.pkg != "" && !packageCovers(exc.Package, occ.pkg) {
				continue
			}
			expires, err := time.Parse(expiresLayout, exc.Expires)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: exception has invalid expires date %q: %v", id, exc.Expires, err))
				continue
			}
			if now.After(expires) {
				failures = append(failures, fmt.Sprintf(
					"%s: exception (module=%s package=%s) expired on %s (owner: %s, tracking: %s)",
					id, exc.Module, exc.Package, exc.Expires, exc.Owner, exc.TrackingIssue,
				))
				continue
			}
			matched = true
			used[i] = true
		}
		if !matched {
			failures = append(failures, fmt.Sprintf(
				"%s: reachable finding (module=%s package=%s) has no matching non-expired exception",
				id, occ.module, occ.pkg,
			))
		}
	}
	return len(failures) == 0, failures
}

func packageCovers(exceptionPkg, findingPkg string) bool {
	if exceptionPkg == findingPkg {
		return true
	}
	return strings.HasPrefix(findingPkg, exceptionPkg+"/")
}
