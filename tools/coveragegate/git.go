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
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// gitCommands runs git in one working tree. It implements refResolver.
type gitCommands struct {
	dir string
}

// run executes git and returns stdout, plus whether the command succeeded.
func (this *gitCommands) run(args ...string) (string, bool) {
	cmd := exec.Command("git", args...)
	cmd.Dir = this.dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// git is asked plenty of questions whose answer is "no", expressed as a
	// non-zero exit; those must not spill onto the gate's stderr.
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return stdout.String(), true
}

func (this *gitCommands) exists(ref string) bool {
	_, ok := this.run("rev-parse", "--verify", "--quiet", ref)
	return ok
}

// fullName resolves a ref to its fully qualified name, following symbolic refs.
// origin/HEAD is one, and older git does not dereference it in rev-parse.
func (this *gitCommands) fullName(ref string) string {
	out, ok := this.run("rev-parse", "--symbolic-full-name", ref)
	if !ok {
		return ""
	}
	full := strings.TrimSpace(out)
	// Bounded, so that a self referential ref in a damaged repository cannot
	// spin here.
	for i := 0; i < 5 && full != ""; i++ {
		next, ok := this.run("symbolic-ref", "--quiet", full)
		if !ok {
			break
		}
		trimmed := strings.TrimSpace(next)
		if trimmed == "" || trimmed == full {
			break
		}
		full = trimmed
	}
	return full
}

func (this *gitCommands) currentBranch() string {
	out, ok := this.run("symbolic-ref", "--quiet", "--short", "HEAD")
	if !ok {
		return ""
	}
	return strings.TrimSpace(out)
}

func (this *gitCommands) remotes() []string {
	out, ok := this.run("remote")
	if !ok {
		return nil
	}
	result := []string{}
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			result = append(result, name)
		}
	}
	return result
}

func (this *gitCommands) mergeBase(ref string) string {
	out, ok := this.run("merge-base", "HEAD", ref)
	if !ok {
		return ""
	}
	return strings.TrimSpace(out)
}

// untrackedGoFiles lists the Go files git does not track. They are part of the
// change: new code most often arrives in a file that was never added, and on
// this branch that is most of it.
func (this *gitCommands) untrackedGoFiles() ([]string, error) {
	out, ok := this.run("ls-files", "--others", "--exclude-standard", "-z")
	if !ok {
		return nil, fmt.Errorf("could not list untracked files")
	}
	result := []string{}
	// -z rather than newline separation, so that a path containing a newline
	// cannot split into two.
	for _, path := range strings.Split(out, "\x00") {
		if path != "" && isRelevantGoFile(path) {
			result = append(result, path)
		}
	}
	return result, nil
}

// diff runs a zero context diff and parses the added lines out of it.
//
// The prefixes are pinned on the command line so that a diff.noprefix or
// diff.mnemonicPrefix in someone's git configuration cannot change the shape of
// the header the parser reads, and quotePath is turned off so that a path with
// non-ASCII characters arrives verbatim rather than octal escaped.
func (this *gitCommands) diff(revisions ...string) (changedLines, error) {
	args := []string{
		"-c", "core.quotePath=false",
		"diff", "--unified=0", "--no-color", "--no-ext-diff",
		"--src-prefix=a/", "--dst-prefix=b/",
	}
	args = append(args, revisions...)
	// Everything after -- is a path, which keeps a branch named like an option
	// out of the argument list.
	args = append(args, "--")

	cmd := exec.Command("git", args...)
	cmd.Dir = this.dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s failed: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return parseDiff(&stdout)
}
