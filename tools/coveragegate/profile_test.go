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
	"strings"
	"testing"
)

func TestParseProfileLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantName string
		want     block
		wantErr  bool
	}{
		{
			name:     "ordinary block",
			line:     "example.com/svc/lib/a.go:12.34,15.2 3 1",
			wantName: "example.com/svc/lib/a.go",
			want:     block{startLine: 12, startCol: 34, endLine: 15, endCol: 2, numStmts: 3, count: 1},
		},
		{
			name:     "uncovered block",
			line:     "example.com/svc/lib/a.go:1.1,1.1 1 0",
			wantName: "example.com/svc/lib/a.go",
			want:     block{startLine: 1, startCol: 1, endLine: 1, endCol: 1, numStmts: 1, count: 0},
		},
		{
			name:     "count mode hit count above one",
			line:     "example.com/svc/lib/a.go:5.2,7.3 2 4711",
			wantName: "example.com/svc/lib/a.go",
			want:     block{startLine: 5, startCol: 2, endLine: 7, endCol: 3, numStmts: 2, count: 4711},
		},
		{
			// The name is split off at the *last* colon, so a colon earlier in
			// the path cannot swallow the range.
			name:     "colon in the file name",
			line:     "example.com/svc/lib/od:d.go:1.1,2.2 1 1",
			wantName: "example.com/svc/lib/od:d.go",
			want:     block{startLine: 1, startCol: 1, endLine: 2, endCol: 2, numStmts: 1, count: 1},
		},
		{name: "no colon", line: "example.com/a.go 1 1", wantErr: true},
		{name: "missing count", line: "a.go:1.1,2.2 1", wantErr: true},
		{name: "extra field", line: "a.go:1.1,2.2 1 1 1", wantErr: true},
		{name: "no comma in the range", line: "a.go:1.1 2.2 1 1", wantErr: true},
		{name: "no dot in a position", line: "a.go:1,2.2 1 1", wantErr: true},
		{name: "unreadable statement count", line: "a.go:1.1,2.2 x 1", wantErr: true},
		{name: "unreadable hit count", line: "a.go:1.1,2.2 1 x", wantErr: true},
		{name: "negative hit count", line: "a.go:1.1,2.2 1 -1", wantErr: true},
		{name: "line zero", line: "a.go:0.1,2.2 1 1", wantErr: true},
		{name: "block ends before it starts", line: "a.go:9.1,2.2 1 1", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name, b, err := parseProfileLine(test.line)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q %+v", name, b)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != test.wantName || b != test.want {
				t.Errorf("got %q %+v, want %q %+v", name, b, test.wantName, test.want)
			}
		})
	}
}

func TestParseProfile(t *testing.T) {
	t.Run("mode and blocks", func(t *testing.T) {
		prof, err := parseProfile(strings.NewReader(
			"mode: set\n" +
				"example.com/svc/lib/a.go:3.1,5.2 2 1\n" +
				"example.com/svc/lib/a.go:1.1,2.2 1 0\n" +
				"example.com/svc/lib/b.go:1.1,2.2 4 1\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if prof.mode != "set" {
			t.Errorf("mode: got %q, want set", prof.mode)
		}
		if len(prof.blocks) != 2 {
			t.Fatalf("files: got %d, want 2", len(prof.blocks))
		}
		// Sorted by position, so the report and the measurement are stable.
		if prof.blocks["example.com/svc/lib/a.go"][0].startLine != 1 {
			t.Errorf("blocks are not sorted by start line: %+v", prof.blocks["example.com/svc/lib/a.go"])
		}
	})

	t.Run("a repeated block is merged, not counted twice", func(t *testing.T) {
		prof, err := parseProfile(strings.NewReader(
			"mode: set\n" +
				"example.com/svc/lib/a.go:1.1,2.2 3 0\n" +
				"example.com/svc/lib/a.go:1.1,2.2 3 1\n" +
				"example.com/svc/lib/a.go:1.1,2.2 3 0\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		blocks := prof.blocks["example.com/svc/lib/a.go"]
		if len(blocks) != 1 {
			t.Fatalf("got %d blocks, want 1 merged block: %+v", len(blocks), blocks)
		}
		// Set mode records presence, so the merge is a logical or; anything
		// else would report a covered block as uncovered depending on the
		// order the test binaries happened to run in.
		if blocks[0].count != 1 {
			t.Errorf("merged count: got %d, want 1", blocks[0].count)
		}
		if blocks[0].numStmts != 3 {
			t.Errorf("merged statement count: got %d, want 3", blocks[0].numStmts)
		}
	})

	t.Run("a block reported with two statement counts is an error", func(t *testing.T) {
		_, err := parseProfile(strings.NewReader(
			"mode: set\nexample.com/svc/a.go:1.1,2.2 3 0\nexample.com/svc/a.go:1.1,2.2 4 1\n"))
		if err == nil {
			t.Fatal("an inconsistent profile must not be measured")
		}
	})

	t.Run("no mode line", func(t *testing.T) {
		if _, err := parseProfile(strings.NewReader("example.com/svc/a.go:1.1,2.2 1 1\n")); err == nil {
			t.Fatal("a profile without a mode line is not a profile")
		}
	})

	t.Run("empty profile", func(t *testing.T) {
		if _, err := parseProfile(strings.NewReader("")); err == nil {
			t.Fatal("an empty file is not a profile")
		}
	})

	t.Run("mode line only", func(t *testing.T) {
		prof, err := parseProfile(strings.NewReader("mode: set\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(prof.blocks) != 0 {
			t.Errorf("expected no blocks, got %d", len(prof.blocks))
		}
	})
}

func TestRepoPath(t *testing.T) {
	tests := []struct {
		name     string
		module   string
		profile  string
		want     string
		inModule bool
	}{
		{"in module", "example.com/svc", "example.com/svc/lib/a.go", "lib/a.go", true},
		{"root file", "example.com/svc", "example.com/svc/main.go", "main.go", true},
		{"another module", "example.com/svc", "example.com/other/lib/a.go", "", false},
		// A module whose path is a prefix of another must not swallow it: the
		// separator is part of the prefix.
		{"prefix collision", "example.com/svc", "example.com/svcx/lib/a.go", "", false},
		{"the module itself", "example.com/svc", "example.com/svc", "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, inModule := repoPath(test.module, test.profile)
			if got != test.want || inModule != test.inModule {
				t.Errorf("repoPath(%q, %q) = (%q, %v), want (%q, %v)",
					test.module, test.profile, got, inModule, test.want, test.inModule)
			}
		})
	}
}

func TestBelowRepoAverage(t *testing.T) {
	tests := []struct {
		name string
		c    coverage
		want bool
	}{
		{
			name: "changed lines below the average",
			c:    coverage{repoTotal: 100, repoCovered: 50, changedTotal: 10, changedCovered: 4},
			want: true,
		},
		{
			name: "changed lines above the average",
			c:    coverage{repoTotal: 100, repoCovered: 50, changedTotal: 10, changedCovered: 6},
			want: false,
		},
		{
			// Exactly at the average passes: the criterion is "not below".
			// Comparing two float64 ratios is what makes this case fragile, so
			// the comparison is done by cross multiplication instead.
			name: "exactly at the average",
			c:    coverage{repoTotal: 100, repoCovered: 50, changedTotal: 10, changedCovered: 5},
			want: false,
		},
		{
			// Same ratio expressed with denominators that share no factor.
			name: "equal ratio with awkward denominators",
			c:    coverage{repoTotal: 300, repoCovered: 100, changedTotal: 3, changedCovered: 1},
			want: false,
		},
		{
			name: "one statement short of the average",
			c:    coverage{repoTotal: 300, repoCovered: 100, changedTotal: 300, changedCovered: 99},
			want: true,
		},
		{
			// The products here exceed the range of a 32 bit integer, which is
			// what int on a 32 bit build would give. The explicit int64 in the
			// comparison is what keeps the answer right there.
			name: "wide values do not overflow",
			c:    coverage{repoTotal: 1_000_000, repoCovered: 500_000, changedTotal: 1_000_000, changedCovered: 499_999},
			want: true,
		},
		{
			name: "wide values, equal ratio",
			c:    coverage{repoTotal: 1_000_000, repoCovered: 500_000, changedTotal: 1_000_000, changedCovered: 500_000},
			want: false,
		},
		{
			// Nothing measurable on the changed lines cannot be below anything.
			name: "no changed statements",
			c:    coverage{repoTotal: 100, repoCovered: 50, changedTotal: 0, changedCovered: 0},
			want: false,
		},
		{
			name: "no repository statements",
			c:    coverage{repoTotal: 0, repoCovered: 0, changedTotal: 10, changedCovered: 0},
			want: false,
		},
		{
			name: "a repository at zero coverage cannot be undercut",
			c:    coverage{repoTotal: 100, repoCovered: 0, changedTotal: 10, changedCovered: 0},
			want: false,
		},
		{
			name: "a fully covered repository is undercut by anything less",
			c:    coverage{repoTotal: 100, repoCovered: 100, changedTotal: 10, changedCovered: 9},
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.c.belowRepoAverage(); got != test.want {
				t.Errorf("belowRepoAverage() = %v, want %v (changed %d/%d, repo %d/%d)",
					got, test.want, test.c.changedCovered, test.c.changedTotal, test.c.repoCovered, test.c.repoTotal)
			}
		})
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		covered, total int
		want           string
	}{
		{0, 0, "n/a"},
		{0, 10, "0.00%"},
		{5, 10, "50.00%"},
		{10, 10, "100.00%"},
		{1, 3, "33.33%"},
	}
	for _, test := range tests {
		t.Run(test.want, func(t *testing.T) {
			if got := percent(test.covered, test.total); got != test.want {
				t.Errorf("percent(%d, %d) = %q, want %q", test.covered, test.total, got, test.want)
			}
		})
	}
}

func TestMeasure(t *testing.T) {
	prof, err := parseProfile(strings.NewReader(
		"mode: set\n" +
			// touched by the change, covered
			"example.com/svc/lib/a.go:10.1,12.2 3 1\n" +
			// touched by the change, not covered
			"example.com/svc/lib/a.go:20.1,22.2 2 0\n" +
			// not touched, covered — counts only towards the average
			"example.com/svc/lib/a.go:40.1,41.2 5 1\n" +
			// a different file, untouched
			"example.com/svc/lib/b.go:1.1,9.2 10 0\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	changed := changedLines{}
	changed.addRange("lib/a.go", 11, 1) // inside the first block
	changed.addRange("lib/a.go", 22, 1) // inside the second block
	changed.addRange("lib/c.go", 1, 5)  // a file the profile knows nothing about

	result := measure(prof, changed, "example.com/svc")

	if result.repoTotal != 20 || result.repoCovered != 8 {
		t.Errorf("repository: got %d/%d, want 8/20", result.repoCovered, result.repoTotal)
	}
	if result.changedTotal != 5 || result.changedCovered != 3 {
		t.Errorf("changed: got %d/%d, want 3/5", result.changedCovered, result.changedTotal)
	}
	if len(result.files) != 1 || result.files[0].path != "lib/a.go" {
		t.Fatalf("per file detail: got %+v, want one entry for lib/a.go", result.files)
	}
	if result.files[0].total != 5 || result.files[0].covered != 3 {
		t.Errorf("lib/a.go: got %d/%d, want 3/5", result.files[0].covered, result.files[0].total)
	}
	// A changed file the profile says nothing about is counted in neither
	// figure, so it has to be named instead of vanishing.
	if len(result.noData) != 1 || result.noData[0] != "lib/c.go" {
		t.Errorf("noData: got %v, want [lib/c.go]", result.noData)
	}
	// 3/5 = 60 % against an average of 8/20 = 40 %.
	if result.belowRepoAverage() {
		t.Error("60% on the changed lines is not below a 40% average")
	}
}

func TestMeasureWorstFileFirst(t *testing.T) {
	prof, err := parseProfile(strings.NewReader(
		"mode: set\n" +
			"example.com/svc/good.go:1.1,2.2 4 1\n" +
			"example.com/svc/bad.go:1.1,2.2 4 0\n" +
			"example.com/svc/mixed.go:1.1,2.2 2 1\n" +
			"example.com/svc/mixed.go:3.1,4.2 2 0\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	changed := changedLines{}
	for _, path := range []string{"good.go", "bad.go", "mixed.go"} {
		changed.addRange(path, 1, 4)
	}
	result := measure(prof, changed, "example.com/svc")
	got := []string{}
	for _, f := range result.files {
		got = append(got, f.path)
	}
	want := "bad.go,mixed.go,good.go"
	if strings.Join(got, ",") != want {
		t.Errorf("file order: got %v, want %s (worst first, so the reader finds the gap)", got, want)
	}
}
