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

// The integration refs a branch is measured against, in order. This mirrors the
// gate runner exactly, because a gate that measures a different range than the
// runner reports is worse than no gate.
//
// The order matters in both directions. Restricting the base to @{u} switches the
// measurement off on a branch that was never pushed — the branch a change is
// written on. Taking @{u} first switches it off again the moment the branch is
// pushed, because @{u} is then the branch's own remote copy and "what this
// branch adds" comes out empty. So @{u} is the last resort, used only when no
// integration ref survives, which is the case on an integration branch itself.
var defaultBaseCandidates = []string{
	"origin/HEAD",
	"refs/heads/main",
	"refs/heads/master",
	"refs/heads/develop",
	"refs/heads/trunk",
}

// refResolver is the git interrogation the base selection needs. It exists as an
// interface so the selection rules can be tested without a repository.
type refResolver interface {
	// exists reports whether a ref resolves to an object.
	exists(ref string) bool
	// fullName gives the fully qualified ref name, symbolic refs followed.
	fullName(ref string) string
	// currentBranch is the checked out branch, "" on a detached head.
	currentBranch() string
	// remotes lists the configured remotes.
	remotes() []string
	// mergeBase gives the merge base of HEAD and ref, "" when there is none.
	mergeBase(ref string) string
}

// baseRef is the resolved comparison point.
type baseRef struct {
	// name is the readable name, "" when nothing resolved.
	name string
	// mergeBase is the commit the branch is measured from, "" when there is
	// none — an unborn head or unrelated histories.
	mergeBase string
}

// selectBase walks the candidate chain and returns the first integration ref
// that is neither the current branch nor its own remote counterpart.
func selectBase(resolver refResolver, configured string) baseRef {
	candidates := []string{}
	if configured != "" {
		candidates = append(candidates, configured)
	}
	candidates = append(candidates, defaultBaseCandidates...)

	branch := resolver.currentBranch()
	remotes := resolver.remotes()

	for _, candidate := range candidates {
		// Existence is checked before the name is resolved, because
		// `rev-parse --symbolic-full-name refs/heads/main` answers
		// "refs/heads/main" for a branch that does not exist at all.
		if !resolver.exists(candidate) {
			continue
		}
		full := resolver.fullName(candidate)
		if isOwnRef(full, branch, remotes) {
			continue
		}
		return baseRef{name: readableRefName(full, candidate), mergeBase: resolver.mergeBase(candidate)}
	}
	if resolver.exists("@{u}") {
		return baseRef{name: readableRefName(resolver.fullName("@{u}"), "@{u}"), mergeBase: resolver.mergeBase("@{u}")}
	}
	return baseRef{}
}

// isOwnRef reports whether a ref is the current branch or one of its remote
// copies.
//
// Ref names are compared, never commits: two branches may legitimately point at
// the same commit, and that does not make one the other's counterpart.
func isOwnRef(full string, branch string, remotes []string) bool {
	if branch == "" || full == "" {
		return false
	}
	if full == "refs/heads/"+branch {
		return true
	}
	for _, remote := range remotes {
		if full == "refs/remotes/"+remote+"/"+branch {
			return true
		}
	}
	return false
}

// readableRefName strips the ref namespace for the report.
func readableRefName(full string, fallback string) string {
	name := full
	for _, prefix := range []string{"refs/heads/", "refs/remotes/", "refs/tags/"} {
		name = strings.TrimPrefix(name, prefix)
	}
	if name == "" {
		return fallback
	}
	return name
}

// readBaseRefSetting pulls BASE_REF out of a gates.env.
//
// The file is shell syntax that the runner sources, but only this one assignment
// matters here, so it is read rather than executed. A later assignment wins, as
// it would in the shell.
func readBaseRefSetting(src []byte) (string, error) {
	value := ""
	scanner := bufio.NewScanner(bytes.NewReader(src))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		if !strings.HasPrefix(line, "BASE_REF=") {
			continue
		}
		value = unquoteShellValue(strings.TrimPrefix(line, "BASE_REF="))
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading gates.env: %w", err)
	}
	// Validated at the boundary rather than where it is used: a value starting
	// with a dash would reach git as an option instead of as a ref, and some of
	// those (--git-dir) even verify successfully.
	if strings.HasPrefix(value, "-") {
		return "", fmt.Errorf("BASE_REF=%q in .claude/gates.env is not a ref name: a value starting with '-' would reach git as an option", value)
	}
	return value, nil
}

// unquoteShellValue reduces the right hand side of a shell assignment to its
// value, handling the quoting forms a gates.env actually uses and stopping at an
// unquoted comment.
func unquoteShellValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && (raw[0] == '"' || raw[0] == '\'') {
		if end := strings.IndexByte(raw[1:], raw[0]); end >= 0 {
			return raw[1 : 1+end]
		}
	}
	if hash := strings.IndexByte(raw, '#'); hash >= 0 {
		raw = raw[:hash]
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
