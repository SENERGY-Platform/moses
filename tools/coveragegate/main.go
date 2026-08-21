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

// Command coveragegate is the `coverage_delta` quality gate for this repository.
//
// The criterion is "coverage of changed lines not below the repo average", not
// an absolute percentage: a repository at 12 % can meet it on day one, and the
// average then only rises, where a fixed threshold either blocks everything or
// checks nothing.
//
// So it measures both numbers on the same profile and compares them. The gate
// runner's adapter has no default for this, which is why the tool exists.
//
// Exit codes: 0 green (or nothing to measure), 1 changed lines below the
// average, 2 the measurement could not be taken.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	exitGreen  = 0
	exitBelow  = 1
	exitBroken = 2
)

func main() {
	code, err := run(os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "coveragegate: %v\n", err)
		os.Exit(exitBroken)
	}
	os.Exit(code)
}

func run(out io.Writer) (int, error) {
	repo, err := repositoryRoot()
	if err != nil {
		return exitBroken, err
	}
	git := &gitCommands{dir: repo}

	mainModule, err := modulePath(repo)
	if err != nil {
		return exitBroken, err
	}

	configured, err := configuredBaseRef(repo)
	if err != nil {
		return exitBroken, err
	}
	if configured != "" && !git.exists(configured) {
		// A pinned base that does not resolve is a configuration error, not
		// something to work around quietly: the repository would be measured
		// against a different ref than the file names, and nobody would know.
		return exitBroken, fmt.Errorf("BASE_REF=%q from .claude/gates.env does not resolve in this repository", configured)
	}

	base := selectBase(git, configured)
	changed, err := collectChanged(git, base)
	if err != nil {
		return exitBroken, err
	}

	fmt.Fprintf(out, "base ref: %s\n", describeBase(base))
	if len(changed) == 0 {
		// Nothing to measure. A documentation or configuration change must not
		// block, and running the suite first would only waste the runner's
		// timeout budget.
		fmt.Fprintf(out, "no changed Go lines outside test files, so there is nothing to measure\n\nPASS: coverage of changed lines is not applicable to this change.\n")
		return exitGreen, nil
	}

	prof, err := runCoverage(repo)
	if err != nil {
		return exitBroken, err
	}

	result := measure(prof, changed, mainModule)
	report(out, result, changed)
	if result.belowRepoAverage() {
		return exitBelow, nil
	}
	return exitGreen, nil
}

// describeBase renders the resolved base for the report, including the case
// where nothing resolved.
func describeBase(base baseRef) string {
	switch {
	case base.name == "":
		return "none resolved, only the working tree is compared"
	case base.mergeBase == "":
		return base.name + " (no common history, only the working tree is compared)"
	default:
		return fmt.Sprintf("%s at %s", base.name, base.mergeBase)
	}
}

// repositoryRoot finds the working tree root, so that the tool works from any
// directory inside it and so that diff paths and file paths mean the same thing.
func repositoryRoot() (string, error) {
	out, err := output(exec.Command("git", "rev-parse", "--show-toplevel"))
	if err != nil {
		return "", fmt.Errorf("not inside a git working tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// modulePath asks go for the module path, which is the prefix the coverage
// profile puts in front of every file name.
func modulePath(repo string) (string, error) {
	cmd := exec.Command("go", "list", "-m")
	cmd.Dir = repo
	out, err := output(cmd)
	if err != nil {
		return "", fmt.Errorf("could not determine the module path: %w", err)
	}
	path := strings.TrimSpace(out)
	// With more than one module in view go lists them all; the first is the
	// main module.
	if index := strings.IndexByte(path, '\n'); index >= 0 {
		path = path[:index]
	}
	if path == "" {
		return "", fmt.Errorf("go reported no module path")
	}
	return path, nil
}

// configuredBaseRef reads BASE_REF the way the gate runner does.
//
// The runner sources .claude/gates.env into its own shell and then reads
// ${BASE_REF:-}, so an assignment in the file wins and an inherited environment
// variable is used when the file is silent. That second path is what makes the
// gate measurable in CI: a checkout there has neither origin/HEAD nor a local
// default branch, so without a base handed in from the workflow the default
// chain resolves to nothing and there is no range left to measure.
func configuredBaseRef(repo string) (string, error) {
	src, err := os.ReadFile(filepath.Join(repo, ".claude", "gates.env"))
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("could not read .claude/gates.env: %w", err)
	}
	if err == nil {
		fromFile, err := readBaseRefSetting(src)
		if err != nil {
			return "", err
		}
		if fromFile != "" {
			return fromFile, nil
		}
	}
	fromEnv := strings.TrimSpace(os.Getenv("BASE_REF"))
	// The same boundary check as for the file: a value starting with a dash
	// would reach git as an option rather than as a ref.
	if strings.HasPrefix(fromEnv, "-") {
		return "", fmt.Errorf("BASE_REF=%q from the environment is not a ref name: a value starting with '-' would reach git as an option", fromEnv)
	}
	return fromEnv, nil
}

// collectChanged assembles the changed Go lines from three sources, which
// together are what the gate runner considers this change: what the branch adds
// on top of its base, what the working tree changes on top of HEAD, and the
// files that are not tracked at all.
func collectChanged(git *gitCommands, base baseRef) (changedLines, error) {
	result := changedLines{}

	if base.mergeBase != "" {
		// Equivalent to `git diff --unified=0 <base>...HEAD`, with the merge
		// base named explicitly so that an absent one is handled above rather
		// than as a git error.
		diff, err := git.diff(base.mergeBase, "HEAD")
		if err != nil {
			return nil, err
		}
		result.merge(diff)
	}

	if git.exists("HEAD") {
		// Staged and unstaged together: an unstaged edit is part of the change.
		diff, err := git.diff("HEAD")
		if err != nil {
			return nil, err
		}
		result.merge(diff)
	}

	untracked, err := git.untrackedGoFiles()
	if err != nil {
		return nil, err
	}
	for _, path := range untracked {
		count, err := countLines(filepath.Join(git.dir, path))
		if err != nil {
			return nil, err
		}
		// A file git has never seen is new in its entirety.
		result.addRange(path, 1, count)
	}

	// A file that the branch changed and then deleted has no coverage to
	// measure. Dropping it here keeps it out of the "no coverage data" list,
	// where it would read as a gap. Only a genuine absence drops the file: any
	// other stat error means something is wrong with the tree and must not be
	// mistaken for a deletion.
	for path := range result {
		if _, err := os.Stat(filepath.Join(git.dir, path)); os.IsNotExist(err) {
			delete(result, path)
		}
	}
	return result, nil
}

// countLines counts the lines of a file the way a diff would: a trailing
// newline does not start a further line, and a file without a final newline
// still ends in a line.
func countLines(path string) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("could not read the untracked file %s: %w", path, err)
	}
	if len(content) == 0 {
		return 0, nil
	}
	count := bytes.Count(content, []byte{'\n'})
	if content[len(content)-1] != '\n' {
		count++
	}
	return count, nil
}

// runCoverage runs the suite with coverage and parses the profile.
//
// The profile goes to a temporary file outside the repository: a gate must not
// leave anything behind in the working tree, least of all a file that would then
// show up in the very diff it measures. No timeout is set here — the suite
// includes docker backed integration tests and takes minutes; the gate runner
// owns the limit.
func runCoverage(repo string) (*profile, error) {
	dir, err := os.MkdirTemp("", "coveragegate-")
	if err != nil {
		return nil, fmt.Errorf("could not create a temporary directory: %w", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "coverage.out")

	cmd := exec.Command("go", "test", "-coverprofile="+path, "-covermode=set", "./...")
	cmd.Dir = repo
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		// A red suite makes the coverage figure meaningless: the profile then
		// reflects whatever ran before the failure. The test gate reports the
		// failure itself; this gate reports that it could not measure.
		return nil, fmt.Errorf("the test suite did not pass, so coverage cannot be measured: %w\n%s", err, combined.String())
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("go test wrote no coverage profile: %w", err)
	}
	defer file.Close()
	return parseProfile(file)
}

// report writes the two numbers and the per file detail.
func report(out io.Writer, result *coverage, changed changedLines) {
	fmt.Fprintf(out, "changed Go files (excluding tests): %d\n", len(changed))
	fmt.Fprintf(out, "repository average: %s (%d/%d statements)\n",
		percent(result.repoCovered, result.repoTotal), result.repoCovered, result.repoTotal)
	fmt.Fprintf(out, "changed lines:      %s (%d/%d statements)\n",
		percent(result.changedCovered, result.changedTotal), result.changedCovered, result.changedTotal)

	if len(result.files) > 0 {
		fmt.Fprintf(out, "\nchanged files with measurable statements, worst first:\n")
		for _, f := range result.files {
			fmt.Fprintf(out, "  %-7s %4d/%-4d %s\n", percent(f.covered, f.total), f.covered, f.total, f.path)
		}
	}
	if len(result.noData) > 0 {
		fmt.Fprintf(out, "\nno coverage data, so counted in neither figure (no statements in the file, or no test binary for its package):\n")
		for _, path := range result.noData {
			fmt.Fprintf(out, "  %s\n", path)
		}
	}

	switch {
	case result.changedTotal == 0:
		fmt.Fprintf(out, "\nPASS: the changed lines carry no measurable statements.\n")
	case result.belowRepoAverage():
		fmt.Fprintf(out, "\nFAIL: coverage of the changed lines (%s) is below the repository average (%s). The files listed above are where a test is missing.\n",
			percent(result.changedCovered, result.changedTotal), percent(result.repoCovered, result.repoTotal))
	default:
		fmt.Fprintf(out, "\nPASS: coverage of the changed lines (%s) is at or above the repository average (%s).\n",
			percent(result.changedCovered, result.changedTotal), percent(result.repoCovered, result.repoTotal))
	}
}

// output runs a command and returns its stdout, with stderr folded into the
// error so that a failure says why.
func output(cmd *exec.Cmd) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
