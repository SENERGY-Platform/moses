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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCountLines pins how an untracked file's line count is taken. Every line of
// such a file is new, so an off-by-one here shifts the whole changed range of a
// new file against the coverage blocks.
func TestCountLines(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{"empty file", "", 0},
		{"one line with a newline", "package a\n", 1},
		{"one line without a newline", "package a", 1},
		{"three lines", "a\nb\nc\n", 3},
		{"three lines, no final newline", "a\nb\nc", 3},
		{"a trailing blank line", "a\n\n", 2},
		{"only a newline", "\n", 1},
		{"crlf line endings", "a\r\nb\r\n", 2},
	}
	dir := t.TempDir()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(dir, "f.go")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatalf("could not write the fixture: %v", err)
			}
			got, err := countLines(path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != test.want {
				t.Errorf("countLines(%q) = %d, want %d", test.content, got, test.want)
			}
		})
	}
}

func TestCountLinesMissingFile(t *testing.T) {
	if _, err := countLines(filepath.Join(t.TempDir(), "absent.go")); err == nil {
		t.Fatal("a missing file must be an error, not a silent zero")
	}
}

func TestDescribeBase(t *testing.T) {
	tests := []struct {
		name string
		base baseRef
		want string
	}{
		{"resolved", baseRef{name: "origin/master", mergeBase: "abc123"}, "origin/master at abc123"},
		{"no common history", baseRef{name: "origin/master"}, "origin/master (no common history, only the working tree is compared)"},
		{"nothing resolved", baseRef{}, "none resolved, only the working tree is compared"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := describeBase(test.base); got != test.want {
				t.Errorf("describeBase() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestConfiguredBaseRef pins the precedence the gate runner establishes: the
// runner sources gates.env and then reads ${BASE_REF:-}, so the file wins over
// the environment and the environment is used only when the file is silent.
func TestConfiguredBaseRef(t *testing.T) {
	tests := []struct {
		name    string
		file    string // "" means no gates.env at all
		env     string
		want    string
		wantErr bool
	}{
		{name: "neither", file: "", env: "", want: ""},
		{name: "file only", file: "BASE_REF=release\n", env: "", want: "release"},
		{name: "environment only", file: "test=enforce\n", env: "origin/master", want: "origin/master"},
		{name: "no file, environment only", file: "", env: "origin/master", want: "origin/master"},
		{name: "the file wins over the environment", file: `BASE_REF="release"` + "\n", env: "origin/master", want: "release"},
		{name: "an empty environment value is no value", file: "test=enforce\n", env: "   ", want: ""},
		{name: "a dash in the environment is refused", file: "test=enforce\n", env: "--git-dir=/etc", wantErr: true},
		{name: "a dash in the file is refused", file: "BASE_REF=-x\n", env: "", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := t.TempDir()
			if test.file != "" {
				if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o700); err != nil {
					t.Fatalf("could not create the fixture directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(repo, ".claude", "gates.env"), []byte(test.file), 0o600); err != nil {
					t.Fatalf("could not write the fixture: %v", err)
				}
			}
			t.Setenv("BASE_REF", test.env)
			got, err := configuredBaseRef(repo)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

// TestReportVerdict pins the verdict the report prints, because that sentence is
// what a reader acts on and it has to agree with the exit code the same
// measurement produces.
func TestReportVerdict(t *testing.T) {
	tests := []struct {
		name     string
		result   coverage
		want     string
		wantFail bool
	}{
		{
			name:   "below the average fails",
			result: coverage{repoTotal: 100, repoCovered: 50, changedTotal: 10, changedCovered: 4},
			want:   "FAIL: coverage of the changed lines (40.00%) is below the repository average (50.00%)",
		},
		{
			name:   "exactly at the average passes",
			result: coverage{repoTotal: 100, repoCovered: 50, changedTotal: 10, changedCovered: 5},
			want:   "PASS: coverage of the changed lines (50.00%) is at or above the repository average (50.00%)",
		},
		{
			name:   "no measurable statements passes",
			result: coverage{repoTotal: 100, repoCovered: 50, changedTotal: 0, changedCovered: 0},
			want:   "PASS: the changed lines carry no measurable statements.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out strings.Builder
			report(&out, &test.result, changedLines{"lib/a.go": {1: true}})
			if !strings.Contains(out.String(), test.want) {
				t.Errorf("report did not contain %q:\n%s", test.want, out.String())
			}
			// The verdict and the exit code must never disagree.
			isFail := strings.Contains(out.String(), "FAIL:")
			if isFail != test.result.belowRepoAverage() {
				t.Errorf("verdict says FAIL=%v but belowRepoAverage() says %v", isFail, test.result.belowRepoAverage())
			}
		})
	}
}
