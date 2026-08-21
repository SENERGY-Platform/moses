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

package main

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// block is one coverage block: a span of source with a statement count and a hit
// count.
type block struct {
	startLine int
	startCol  int
	endLine   int
	endCol    int
	numStmts  int
	count     int
}

// profile is a parsed coverage profile, keyed by the file name as the profile
// spells it — the import path of the package plus the file name, not a path
// relative to the repository.
type profile struct {
	mode   string
	blocks map[string][]block
}

// parseProfile reads a coverage profile in the format `go test -coverprofile`
// writes: a `mode:` line, then one line per block of the form
//
//	name.go:startLine.startCol,endLine.endCol numStmts count
func parseProfile(r io.Reader) (*profile, error) {
	result := &profile{blocks: map[string][]block{}}
	// Blocks are keyed by their position so that the same block appearing twice
	// is merged rather than counted twice. A file instrumented for more than one
	// test binary does appear more than once, and double counting it would skew
	// both the average and the changed-line figure.
	type position struct {
		name                                 string
		startLine, startCol, endLine, endCol int
	}
	merged := map[position]*block{}
	order := []position{}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "mode:") {
			result.mode = strings.TrimSpace(strings.TrimPrefix(line, "mode:"))
			continue
		}
		name, b, err := parseProfileLine(line)
		if err != nil {
			return nil, fmt.Errorf("coverage profile line %d: %w", lineNo, err)
		}
		pos := position{name, b.startLine, b.startCol, b.endLine, b.endCol}
		existing, found := merged[pos]
		if !found {
			copied := b
			merged[pos] = &copied
			order = append(order, pos)
			continue
		}
		if existing.numStmts != b.numStmts {
			return nil, fmt.Errorf("coverage profile line %d: block %s:%d.%d,%d.%d reported with %d statements and then with %d",
				lineNo, name, b.startLine, b.startCol, b.endLine, b.endCol, existing.numStmts, b.numStmts)
		}
		// Set mode records presence, so two profiles for the same block are
		// combined with a logical or. Taking the larger count also does the
		// right thing for the counting modes.
		if b.count > existing.count {
			existing.count = b.count
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading the coverage profile: %w", err)
	}
	if result.mode == "" {
		return nil, fmt.Errorf("coverage profile has no mode line, so it is not a profile")
	}
	for _, pos := range order {
		result.blocks[pos.name] = append(result.blocks[pos.name], *merged[pos])
	}
	for name := range result.blocks {
		blocks := result.blocks[name]
		sort.Slice(blocks, func(i, j int) bool {
			if blocks[i].startLine != blocks[j].startLine {
				return blocks[i].startLine < blocks[j].startLine
			}
			return blocks[i].startCol < blocks[j].startCol
		})
	}
	return result, nil
}

// parseProfileLine reads one block line.
func parseProfileLine(line string) (name string, b block, err error) {
	// The file name is separated from the range by the *last* colon: the name is
	// an import path, which may itself contain a colon on no sane host but the
	// range never does.
	colon := strings.LastIndex(line, ":")
	if colon <= 0 {
		return "", block{}, fmt.Errorf("no file name separator in %q", line)
	}
	name = line[:colon]
	fields := strings.Fields(line[colon+1:])
	if len(fields) != 3 {
		return "", block{}, fmt.Errorf("expected `range numStmts count` after the file name in %q", line)
	}

	span := strings.Split(fields[0], ",")
	if len(span) != 2 {
		return "", block{}, fmt.Errorf("expected `start,end` range in %q", line)
	}
	if b.startLine, b.startCol, err = parseLineCol(span[0]); err != nil {
		return "", block{}, fmt.Errorf("in %q: %w", line, err)
	}
	if b.endLine, b.endCol, err = parseLineCol(span[1]); err != nil {
		return "", block{}, fmt.Errorf("in %q: %w", line, err)
	}
	if b.numStmts, err = strconv.Atoi(fields[1]); err != nil {
		return "", block{}, fmt.Errorf("in %q: unreadable statement count: %w", line, err)
	}
	if b.count, err = strconv.Atoi(fields[2]); err != nil {
		return "", block{}, fmt.Errorf("in %q: unreadable hit count: %w", line, err)
	}
	if b.numStmts < 0 || b.count < 0 {
		return "", block{}, fmt.Errorf("in %q: negative statement or hit count", line)
	}
	if b.endLine < b.startLine {
		return "", block{}, fmt.Errorf("in %q: block ends before it starts", line)
	}
	return name, b, nil
}

// parseLineCol reads a `line.column` pair.
func parseLineCol(text string) (line int, col int, err error) {
	dot := strings.Index(text, ".")
	if dot < 0 {
		return 0, 0, fmt.Errorf("expected `line.column`, got %q", text)
	}
	if line, err = strconv.Atoi(text[:dot]); err != nil {
		return 0, 0, fmt.Errorf("unreadable line in %q: %w", text, err)
	}
	if col, err = strconv.Atoi(text[dot+1:]); err != nil {
		return 0, 0, fmt.Errorf("unreadable column in %q: %w", text, err)
	}
	if line < 1 {
		return 0, 0, fmt.Errorf("line number below one in %q", text)
	}
	return line, col, nil
}

// repoPath turns a profile file name into a path relative to the repository
// root, which is what the diff speaks in. The second result is false when the
// name does not belong to the module under test.
func repoPath(mainModule string, name string) (string, bool) {
	prefix := mainModule + "/"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	return strings.TrimPrefix(name, prefix), true
}

// fileStats is the per file result for a changed file.
type fileStats struct {
	path    string
	total   int
	covered int
}

// coverage is the whole measurement the gate decides on.
type coverage struct {
	repoTotal      int
	repoCovered    int
	changedTotal   int
	changedCovered int
	files          []fileStats
	// noData lists changed files the profile says nothing about. Two causes
	// look identical from the profile alone: the file holds no statements at
	// all (only declarations, as an interface or a constant block does), or its
	// package produced no test binary. They are counted in neither figure, so
	// the report has to name them rather than let them vanish.
	noData []string
}

// belowRepoAverage reports whether the changed lines are covered worse than the
// repository on average.
//
// The comparison is done by cross multiplication rather than on two float64
// ratios: with floating point division a genuinely equal pair can come out
// unequal in the last bit, and "not below the average" has to hold exactly when
// the change is exactly at the average.
func (this *coverage) belowRepoAverage() bool {
	if this.changedTotal == 0 || this.repoTotal == 0 {
		return false
	}
	// changedCovered/changedTotal < repoCovered/repoTotal
	// int64 keeps the products safe: a repository would need billions of
	// statements to come near the limit.
	left := int64(this.changedCovered) * int64(this.repoTotal)
	right := int64(this.repoCovered) * int64(this.changedTotal)
	return left < right
}

// percent renders a ratio for humans. It never decides anything.
func percent(covered int, total int) string {
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f%%", 100*float64(covered)/float64(total))
}

// measure combines the profile with the changed lines.
func measure(prof *profile, changed changedLines, mainModule string) *coverage {
	result := &coverage{}
	perFile := map[string]*fileStats{}

	for name, blocks := range prof.blocks {
		path, inModule := repoPath(mainModule, name)
		for _, b := range blocks {
			result.repoTotal += b.numStmts
			if b.count > 0 {
				result.repoCovered += b.numStmts
			}
			if !inModule || !changed.touches(path, b.startLine, b.endLine) {
				continue
			}
			stats := perFile[path]
			if stats == nil {
				stats = &fileStats{path: path}
				perFile[path] = stats
			}
			stats.total += b.numStmts
			if b.count > 0 {
				stats.covered += b.numStmts
			}
			result.changedTotal += b.numStmts
			if b.count > 0 {
				result.changedCovered += b.numStmts
			}
		}
	}

	// Which changed files the profile covered at all, so that a file in a
	// package with no test binary is visible rather than silently absent.
	profiled := map[string]bool{}
	for name := range prof.blocks {
		if path, inModule := repoPath(mainModule, name); inModule {
			profiled[path] = true
		}
	}
	for path := range changed {
		if !profiled[path] {
			result.noData = append(result.noData, path)
		}
	}
	sort.Strings(result.noData)

	for _, stats := range perFile {
		result.files = append(result.files, *stats)
	}
	sort.Slice(result.files, func(i, j int) bool {
		// Worst first: the report is read to find where a test is missing.
		left, right := &result.files[i], &result.files[j]
		if left.covered*right.total != right.covered*left.total {
			return left.covered*right.total < right.covered*left.total
		}
		return left.path < right.path
	})
	return result
}
