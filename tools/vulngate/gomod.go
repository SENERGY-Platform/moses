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
	"bytes"
	"fmt"
	"strings"
)

// The pseudo module paths the vulnerability database uses for the parts of the
// Go distribution. They are not module paths and will never appear in a go.mod
// require block, so they have to be classified separately — and they classify
// as actionable, because a fix for either is a toolchain upgrade we control.
const (
	stdlibModulePath    = "stdlib"
	toolchainModulePath = "toolchain"
)

// The classifications a vulnerable module can get. Only direct and stdlib are
// actionable: a transitive finding is not ours to fix, which is exactly what
// the gate criterion excludes.
const (
	classDirect     = "direct"
	classTransitive = "transitive"
	classStdlib     = "stdlib"
)

// requirements is what go.mod says about this module: its own path and the set
// of module paths it requires directly, meaning a require line that does not
// carry an `// indirect` marker.
type requirements struct {
	mainModule string
	direct     map[string]bool
}

// classify sorts a module path from a govulncheck trace into one of the three
// classes above.
func (this *requirements) classify(modulePath string) string {
	switch {
	case modulePath == stdlibModulePath || modulePath == toolchainModulePath:
		return classStdlib
	case modulePath == this.mainModule:
		// A finding whose vulnerable frame is our own code is as direct as it
		// gets. The database does not carry entries for this module, so this is
		// defence against a surprise rather than an expected case.
		return classDirect
	case this.direct[modulePath]:
		return classDirect
	default:
		return classTransitive
	}
}

// actionable reports whether a class is one the criterion holds us responsible
// for.
func actionable(class string) bool {
	return class == classDirect || class == classStdlib
}

// the block keywords go.mod knows. Only require blocks carry requirements; the
// others are skipped wholesale so that a `replace` target can never be mistaken
// for a direct requirement.
var blockKeywords = map[string]bool{
	"require": true, "replace": true, "exclude": true,
	"retract": true, "tool": true, "godebug": true,
	"ignore": true,
}

// parseGoMod reads the module path and the direct requirements out of a go.mod.
//
// This deliberately does not use golang.org/x/mod/modfile: that would promote an
// indirect dependency of this repository to a direct one, which is a change to
// the very file being parsed. The subset of the grammar that matters here is
// small — a module line, require lines, and blocks to skip.
func parseGoMod(src []byte) (*requirements, error) {
	result := &requirements{direct: map[string]bool{}}
	scanner := bufio.NewScanner(bytes.NewReader(src))
	// go.mod files are small, but a pathological single line must not make the
	// scanner give up silently with bufio.ErrTooLong.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	block := "" // the keyword of the block we are inside, "" when at top level
	for scanner.Scan() {
		code, comment := splitComment(scanner.Text())
		code = strings.TrimSpace(code)
		if code == "" {
			// A whole-line comment. It must not be read as a suffix comment of
			// the following line: a standalone `// indirect` above a require
			// line does not make that requirement indirect.
			continue
		}

		if block != "" {
			if code == ")" {
				block = ""
				continue
			}
			if block == "require" {
				if err := result.addRequirement(code, comment); err != nil {
					return nil, err
				}
			}
			continue
		}

		fields := strings.Fields(code)
		keyword := fields[0]
		if blockKeywords[keyword] && fields[len(fields)-1] == "(" {
			block = keyword
			continue
		}
		switch keyword {
		case "module":
			if len(fields) < 2 {
				return nil, fmt.Errorf("go.mod: module line without a path: %q", code)
			}
			result.mainModule = unquoteToken(fields[1])
		case "require":
			if err := result.addRequirement(strings.TrimSpace(strings.TrimPrefix(code, "require")), comment); err != nil {
				return nil, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("go.mod: %w", err)
	}
	if result.mainModule == "" {
		return nil, fmt.Errorf("go.mod: no module line found")
	}
	return result, nil
}

// addRequirement records one `path version` requirement unless its suffix
// comment marks it indirect.
func (this *requirements) addRequirement(code string, comment string) error {
	fields := strings.Fields(code)
	if len(fields) < 2 {
		return fmt.Errorf("go.mod: require line without a version: %q", code)
	}
	if isIndirect(comment) {
		return nil
	}
	this.direct[unquoteToken(fields[0])] = true
	return nil
}

// isIndirect applies the same rule the go tooling applies to a require line's
// suffix comment: the comment counts as the indirect marker when its first word
// is "indirect", optionally followed by a semicolon and further prose.
func isIndirect(comment string) bool {
	text := strings.TrimSpace(strings.TrimPrefix(comment, "//"))
	return text == "indirect" || strings.HasPrefix(text, "indirect;")
}

// splitComment splits a go.mod line into its code and its trailing comment.
//
// The scan is quote aware because a go.mod token may be a quoted or raw string,
// and a `//` inside one is part of the token rather than the start of a comment.
// The returned comment keeps its leading `//` so that an absent comment ("") is
// distinguishable from an empty one.
func splitComment(line string) (code string, comment string) {
	inQuote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote == '"':
			// A backslash escapes the next byte inside an interpreted string,
			// so a `\"` does not end the quote.
			if c == '\\' {
				i++
			} else if c == '"' {
				inQuote = 0
			}
		case inQuote == '`':
			if c == '`' {
				inQuote = 0
			}
		case c == '"' || c == '`':
			inQuote = c
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return line[:i], line[i:]
		}
	}
	return line, ""
}

// unquoteToken removes the quoting from a go.mod token. A module path needing
// quotes is rare, but an unstripped quote would silently never match a module
// path from a govulncheck trace.
func unquoteToken(token string) string {
	if len(token) >= 2 {
		if token[0] == '"' && token[len(token)-1] == '"' {
			return strings.ReplaceAll(token[1:len(token)-1], `\"`, `"`)
		}
		if token[0] == '`' && token[len(token)-1] == '`' {
			return token[1 : len(token)-1]
		}
	}
	return token
}
