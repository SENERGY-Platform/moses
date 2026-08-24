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
	"strconv"
	"strings"
)

// changedLines holds, per repository relative path, the line numbers the change
// adds or rewrites, counted in the *new* file.
type changedLines map[string]map[int]bool

// add records one line for one file.
func (this changedLines) add(path string, line int) {
	lines := this[path]
	if lines == nil {
		lines = map[int]bool{}
		this[path] = lines
	}
	lines[line] = true
}

// addRange records a half open range of lines, which is how a hunk header
// expresses its added lines.
func (this changedLines) addRange(path string, start int, count int) {
	for i := 0; i < count; i++ {
		this.add(path, start+i)
	}
}

// merge folds another set into this one.
func (this changedLines) merge(other changedLines) {
	for path, lines := range other {
		for line := range lines {
			this.add(path, line)
		}
	}
}

// touches reports whether any line of the closed interval [start, end] is in the
// changed set for path. A coverage block spans a range of lines, and it counts as
// changed as soon as the change reaches into it.
func (this changedLines) touches(path string, start int, end int) bool {
	lines := this[path]
	if lines == nil {
		return false
	}
	// Iterating the block is cheaper than iterating the file: a block spans a
	// few lines, the changed set can span thousands.
	if end-start > len(lines) {
		for line := range lines {
			if line >= start && line <= end {
				return true
			}
		}
		return false
	}
	for line := start; line <= end; line++ {
		if lines[line] {
			return true
		}
	}
	return false
}

// isRelevantGoFile decides whether a path belongs in the changed set.
//
// Test files are excluded on purpose: a change that only adds a test would
// otherwise measure the test covering itself, which is not the signal the gate
// is after.
func isRelevantGoFile(path string) bool {
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
}

// parseDiff extracts the added line numbers per file from a unified diff
// produced with --unified=0.
//
// The hunk headers carry the answer, but a body line still has to be recognised
// as one: an added source line beginning with "++ " appears as "+++ …", the
// shape of a file header, and a Go raw string literal can contain one. Read as a
// header it would drop every remaining hunk of that file - under-reporting, the
// direction that weakens the gate.
//
// So the body is counted, not guessed: a --unified=0 hunk is followed by exactly
// oldCount removed plus newCount added lines, and nothing in between is read as
// structure.
func parseDiff(r io.Reader) (changedLines, error) {
	result := changedLines{}
	scanner := bufio.NewScanner(r)
	// A diff can carry very long lines (a minified file, a generated table).
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	path := ""
	remaining := 0 // body lines still expected for the current hunk
	for scanner.Scan() {
		line := scanner.Text()

		if remaining > 0 {
			switch {
			case strings.HasPrefix(line, `\`):
				// "\ No newline at end of file" annotates the previous line
				// rather than being one of its own.
				continue
			case strings.HasPrefix(line, "+"), strings.HasPrefix(line, "-"):
				remaining--
				continue
			default:
				// The body ended sooner than the header promised. Rather than
				// swallow the rest of the diff, fall through and read this line
				// as structure again.
				remaining = 0
			}
		}

		switch {
		case strings.HasPrefix(line, "diff --git "):
			// A new file section starts; forget the previous target until its
			// +++ header names one.
			path = ""
		case strings.HasPrefix(line, "+++ "):
			path = destinationPath(strings.TrimPrefix(line, "+++ "))
		case strings.HasPrefix(line, "@@ "):
			h, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			remaining = h.oldCount + h.newCount
			// A hunk for a deleted file, or for a path that is not measured,
			// still has to have its body skipped.
			if path != "" {
				result.addRange(path, h.newStart, h.newCount)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading the diff: %w", err)
	}
	return result, nil
}

// destinationPath turns the target of a `+++` header into a repository relative
// path, or "" when the header names no file.
func destinationPath(target string) string {
	// git appends no timestamp, but it does keep the b/ prefix unless it is
	// turned off. The caller pins the prefix explicitly so this is the only
	// form that can arrive.
	if target == "/dev/null" {
		return ""
	}
	target = strings.TrimPrefix(target, "b/")
	if !isRelevantGoFile(target) {
		return ""
	}
	return target
}

// hunk is the parsed range pair of a hunk header. The old counts matter only for
// knowing how long the body is.
type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
}

// parseHunkHeader reads both ranges of a hunk header.
//
// The form is `@@ -oldStart[,oldCount] +newStart[,newCount] @@ [heading]`. An
// absent count means one line — the common case for a single line change, and
// reading it as zero would drop the change. A newCount of zero means the hunk
// only deletes, and newStart is then the line *before* the removal, which is
// emphatically not a changed line: recording it would attribute the coverage of
// an untouched neighbour to this change.
func parseHunkHeader(header string) (hunk, error) {
	rest := strings.TrimPrefix(header, "@@ ")
	end := strings.Index(rest, " @@")
	if end < 0 {
		return hunk{}, fmt.Errorf("malformed hunk header: %q", header)
	}

	result := hunk{}
	haveOld, haveNew := false, false
	for _, field := range strings.Fields(rest[:end]) {
		switch {
		case strings.HasPrefix(field, "-") && !haveOld:
			var err error
			if result.oldStart, result.oldCount, err = parseHunkRange(header, field[1:]); err != nil {
				return hunk{}, err
			}
			haveOld = true
		case strings.HasPrefix(field, "+") && !haveNew:
			var err error
			if result.newStart, result.newCount, err = parseHunkRange(header, field[1:]); err != nil {
				return hunk{}, err
			}
			haveNew = true
		}
	}
	if !haveNew {
		return hunk{}, fmt.Errorf("hunk header without a new-file range: %q", header)
	}
	if !haveOld {
		return hunk{}, fmt.Errorf("hunk header without an old-file range: %q", header)
	}
	if result.newStart == 0 && result.newCount > 0 {
		// `+0,n` cannot happen with n > 0: line numbering is one based.
		return hunk{}, fmt.Errorf("hunk header %q: line 0 with a non-empty range", header)
	}
	return result, nil
}

// parseHunkRange reads a `start[,count]` range, with an absent count meaning one.
func parseHunkRange(header string, text string) (start int, count int, err error) {
	startText, countText := text, "1"
	if comma := strings.Index(text, ","); comma >= 0 {
		startText, countText = text[:comma], text[comma+1:]
	}
	if start, err = strconv.Atoi(startText); err != nil {
		return 0, 0, fmt.Errorf("hunk header %q: unreadable start line: %w", header, err)
	}
	if count, err = strconv.Atoi(countText); err != nil {
		return 0, 0, fmt.Errorf("hunk header %q: unreadable line count: %w", header, err)
	}
	if start < 0 || count < 0 {
		return 0, 0, fmt.Errorf("hunk header %q: negative range", header)
	}
	return start, count, nil
}
