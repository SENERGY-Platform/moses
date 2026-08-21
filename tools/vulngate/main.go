/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Command vulngate is the `vulns` quality gate for this repository.
//
// The gate criterion is "no new known vulnerability with an available fix in a
// direct dependency", and it explicitly excludes "transitive findings without a
// fix — those only make noise". Plain govulncheck is stricter than that: it
// fails on any called vulnerability, whether or not anybody can act on it. This
// repository has five called findings that are all transitive and all unfixed
// upstream, so the plain command fails a gate the criterion calls green, and the
// only way back to green would be to weaken the gate.
//
// So this program runs govulncheck, keeps only the findings in reachable code,
// and fails only on those that have a fix and sit in a direct requirement or in
// the Go distribution. It prints the rest either way: a green run that says
// nothing would hide five known, currently unfixable vulnerabilities.
//
// Exit codes: 0 green, 1 at least one blocking finding, 2 the scan could not be
// run or its output could not be trusted.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

const (
	exitGreen    = 0
	exitBlocking = 1
	exitBroken   = 2
)

// govulncheckArgs runs the scanner through `go run` rather than a local binary,
// so that the gate behaves the same on a developer machine and in CI, where
// nothing is installed.
var govulncheckArgs = []string{
	"run", "golang.org/x/vuln/cmd/govulncheck@latest", "-format", "json", "./...",
}

func main() {
	code, err := run(os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vulngate: %v\n", err)
		os.Exit(exitBroken)
	}
	os.Exit(code)
}

// run performs the scan and writes the report. A returned error always means the
// gate could not determine anything and is never a finding.
func run(out io.Writer) (int, error) {
	reqs, err := loadRequirements()
	if err != nil {
		return exitBroken, err
	}

	stdout, err := runGovulncheck()
	if err != nil {
		return exitBroken, err
	}

	scan, err := parseStream(bytes.NewReader(stdout))
	if err != nil {
		return exitBroken, err
	}

	vulns, err := collectCalled(scan.findings, reqs)
	if err != nil {
		return exitBroken, err
	}

	blocking := 0
	for i := range vulns {
		if vulns[i].blocking() {
			blocking++
		}
	}

	report(out, scan.config, vulns, blocking)
	if blocking > 0 {
		return exitBlocking, nil
	}
	return exitGreen, nil
}

// loadRequirements reads the go.mod of the main module. `go env GOMOD` is used
// rather than a relative path so that the tool works from any directory inside
// the module.
func loadRequirements() (*requirements, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("could not ask go for the go.mod path: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	path := strings.TrimSpace(string(raw))
	// Outside a module go prints an empty string, or the null device when
	// module mode is off. Neither leaves anything to classify against.
	if path == "" || path == os.DevNull {
		return nil, fmt.Errorf("not inside a go module, so no direct requirements can be determined")
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	return parseGoMod(src)
}

// runGovulncheck runs the scanner and returns its stdout.
//
// With -format json govulncheck exits 0 whatever it finds — its own
// documentation says so, and only the text renderer returns the "vulnerabilities
// found" exit code 3. A non-zero exit therefore means the scan itself failed,
// which has to be loud: reporting green because the output was empty is the one
// outcome worse than a false alarm. No timeout is set here; the gate runner
// applies one.
func runGovulncheck() ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("go", govulncheckArgs...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("govulncheck could not be run (go %s): %w\n%s",
			strings.Join(govulncheckArgs, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// report writes the summary. It prints every called finding, blocking or not,
// because the value of a green run here is that it names what is known and
// cannot currently be fixed.
func report(out io.Writer, cfg *config, vulns []vulnerability, blocking int) {
	if cfg != nil {
		fmt.Fprintf(out, "%s %s, database %s, scan level %s\n",
			cfg.ScannerName, cfg.ScannerVersion, cfg.DB, cfg.ScanLevel)
	}
	fmt.Fprintf(out, "called findings: %d, blocking: %d\n", len(vulns), blocking)
	fmt.Fprintf(out, "criterion: a called vulnerability blocks only with a fix available in a direct requirement or in the Go distribution\n")

	for i := range vulns {
		v := &vulns[i]
		fix := "no fix"
		if v.fixed != "" {
			fix = "fixed in " + v.fixed
		}
		marker := "  "
		if v.blocking() {
			marker = "->"
		}
		fmt.Fprintf(out, "%s %-14s %-10s %-40s %s (%s)\n",
			marker, v.id, v.class, v.module+moduleVersionSuffix(v.version), fix, "https://pkg.go.dev/vuln/"+v.id)
	}

	if blocking > 0 {
		fmt.Fprintf(out, "\nFAIL: %d called finding(s) marked -> have a fix and sit in a direct requirement or the Go distribution. Upgrade to the listed version.\n", blocking)
		return
	}
	if len(vulns) > 0 {
		fmt.Fprintf(out, "\nPASS: no called finding is both fixable and direct. The findings above stay listed until an upstream fix exists.\n")
		return
	}
	fmt.Fprintf(out, "\nPASS: no vulnerability is reachable from this code.\n")
}

// moduleVersionSuffix renders the version of a vulnerable module, which the
// stdlib pseudo module carries as a go version rather than a module version.
func moduleVersionSuffix(version string) string {
	if version == "" {
		return ""
	}
	return "@" + version
}
