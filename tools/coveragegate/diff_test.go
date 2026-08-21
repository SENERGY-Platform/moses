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
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestParseHunkHeader(t *testing.T) {
	tests := []struct {
		name         string
		header       string
		wantStart    int
		wantCount    int
		wantOldCount int
		wantErr      bool
	}{
		{
			// The common single line change. An absent count means one line,
			// and reading it as zero would drop the change entirely.
			name: "absent count means one line", header: "@@ -12 +12 @@",
			wantStart: 12, wantCount: 1,
		},
		{
			name: "explicit count", header: "@@ -10,3 +10,5 @@",
			wantStart: 10, wantCount: 5, wantOldCount: 3,
		},
		{
			// A pure deletion. newStart is the line before the removal, which
			// is untouched: recording it would attribute a neighbour's
			// coverage to this change.
			name: "count zero is a deletion and adds nothing", header: "@@ -40,7 +39,0 @@",
			wantStart: 39, wantCount: 0,
		},
		{
			name: "pure addition into an empty file", header: "@@ -0,0 +1,7 @@",
			wantStart: 1, wantCount: 7,
		},
		{
			name: "absent count on the old side only", header: "@@ -12 +12,3 @@",
			wantStart: 12, wantCount: 3,
		},
		{
			name: "trailing section heading is ignored", header: "@@ -10,0 +11,2 @@ func (this *Thing) Method() {",
			wantStart: 11, wantCount: 2,
		},
		{
			// A heading that itself contains " @@" must not shorten the range.
			name: "heading containing the marker", header: "@@ -1,0 +2,1 @@ func f() { // @@ note",
			wantStart: 2, wantCount: 1,
		},
		{name: "no closing marker", header: "@@ -1,2 +3,4", wantErr: true},
		{name: "no new side", header: "@@ -1,2 @@", wantErr: true},
		{name: "no old side", header: "@@ +1,2 @@", wantErr: true},
		{name: "unreadable start", header: "@@ -1,2 +x,4 @@", wantErr: true},
		{name: "unreadable count", header: "@@ -1,2 +3,y @@", wantErr: true},
		{name: "line zero with a range", header: "@@ -1,2 +0,4 @@", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseHunkHeader(test.header)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.newStart != test.wantStart || got.newCount != test.wantCount {
				t.Errorf("got newStart=%d newCount=%d, want %d and %d",
					got.newStart, got.newCount, test.wantStart, test.wantCount)
			}
			if test.wantOldCount != 0 && got.oldCount != test.wantOldCount {
				t.Errorf("got oldCount=%d, want %d — the body length depends on it",
					got.oldCount, test.wantOldCount)
			}
		})
	}
}

func TestParseDiff(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string][]int
	}{
		{
			name: "two files, added and rewritten lines",
			input: `diff --git a/lib/a.go b/lib/a.go
index 1111111..2222222 100644
--- a/lib/a.go
+++ b/lib/a.go
@@ -5,0 +6,2 @@ func A() {
+	x := 1
+	_ = x
@@ -20 +21 @@
-	return nil
+	return err
diff --git a/lib/b.go b/lib/b.go
index 3333333..4444444 100644
--- a/lib/b.go
+++ b/lib/b.go
@@ -1,0 +2 @@
+// note
`,
			want: map[string][]int{
				"lib/a.go": {6, 7, 21},
				"lib/b.go": {2},
			},
		},
		{
			// A deleted file has no lines in the new revision, so nothing to
			// measure. Left in, its hunks would be attributed to whichever file
			// was named before it.
			name: "deleted file contributes nothing and does not leak into the next",
			input: `diff --git a/lib/gone.go b/lib/gone.go
deleted file mode 100644
index 5555555..0000000
--- a/lib/gone.go
+++ /dev/null
@@ -1,40 +0,0 @@
-package gone
diff --git a/lib/kept.go b/lib/kept.go
--- a/lib/kept.go
+++ b/lib/kept.go
@@ -0,0 +1,2 @@
+package kept
+
`,
			want: map[string][]int{"lib/kept.go": {1, 2}},
		},
		{
			// Test code covering itself is not the signal the gate is after.
			name: "test files and non-Go files are excluded",
			input: `diff --git a/lib/a_test.go b/lib/a_test.go
--- a/lib/a_test.go
+++ b/lib/a_test.go
@@ -0,0 +1,3 @@
+package lib
+
+func TestA(t *testing.T) {}
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -0,0 +1,1 @@
+# title
diff --git a/lib/a.go b/lib/a.go
--- a/lib/a.go
+++ b/lib/a.go
@@ -0,0 +1,1 @@
+package lib
`,
			want: map[string][]int{"lib/a.go": {1}},
		},
		{
			name: "rename records the destination",
			input: `diff --git a/lib/old.go b/lib/new.go
similarity index 96%
rename from lib/old.go
rename to lib/new.go
--- a/lib/old.go
+++ b/lib/new.go
@@ -3 +3 @@
-old
+new
`,
			want: map[string][]int{"lib/new.go": {3}},
		},
		{
			name: "binary file without hunks",
			input: `diff --git a/docs/x.png b/docs/x.png
index 1111111..2222222 100644
Binary files a/docs/x.png and b/docs/x.png differ
`,
			want: map[string][]int{},
		},
		{
			// Body lines are never read, so content that looks like a header
			// cannot be mistaken for one. With --unified=0 the header alone
			// carries the whole answer.
			name: "content that looks like diff syntax",
			input: `diff --git a/lib/a.go b/lib/a.go
--- a/lib/a.go
+++ b/lib/a.go
@@ -0,0 +1,3 @@
+const marker = "@@ -1,2 +3,4 @@"
+const other = "+++ b/lib/injected.go"
+const third = "diff --git a/x b/x"
`,
			want: map[string][]int{"lib/a.go": {1, 2, 3}},
		},
		{
			name: "no newline at end of file marker",
			input: `diff --git a/lib/a.go b/lib/a.go
--- a/lib/a.go
+++ b/lib/a.go
@@ -3 +3 @@
-old
\ No newline at end of file
+new
\ No newline at end of file
`,
			want: map[string][]int{"lib/a.go": {3}},
		},
		{
			name:  "empty diff",
			input: "",
			want:  map[string][]int{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseDiff(strings.NewReader(test.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if describeChanged(got) != describeChanged(fromWant(test.want)) {
				t.Errorf("got %s, want %s", describeChanged(got), describeChanged(fromWant(test.want)))
			}
		})
	}
}

func TestParseDiffRejectsMalformedHunk(t *testing.T) {
	input := "diff --git a/lib/a.go b/lib/a.go\n--- a/lib/a.go\n+++ b/lib/a.go\n@@ -1,2 +nope @@\n"
	if _, err := parseDiff(strings.NewReader(input)); err == nil {
		t.Fatal("a malformed hunk header must be an error, not a silently empty change set")
	}
}

func TestChangedLinesTouches(t *testing.T) {
	changed := changedLines{}
	changed.addRange("a.go", 10, 3) // 10, 11, 12

	tests := []struct {
		name       string
		path       string
		start, end int
		want       bool
	}{
		{"block entirely before", "a.go", 1, 9, false},
		{"block ends on the first changed line", "a.go", 1, 10, true},
		{"block starts on the last changed line", "a.go", 12, 30, true},
		{"block entirely after", "a.go", 13, 30, false},
		{"block inside", "a.go", 11, 11, true},
		{"block spanning the change", "a.go", 1, 30, true},
		{"unknown file", "b.go", 10, 12, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := changed.touches(test.path, test.start, test.end); got != test.want {
				t.Errorf("touches(%s, %d, %d) = %v, want %v", test.path, test.start, test.end, got, test.want)
			}
		})
	}
}

// TestChangedLinesTouchesBothStrategies pins that the two ways touches can scan
// — over the block or over the changed set — agree. The switch between them is
// an optimisation and must never be observable.
func TestChangedLinesTouchesBothStrategies(t *testing.T) {
	changed := changedLines{}
	for _, line := range []int{5, 500} {
		changed.add("a.go", line)
	}
	cases := []struct {
		start, end int
		want       bool
	}{
		{1, 4, false},
		{1, 5, true},
		{6, 499, false},      // wide block, scanned over the changed set
		{6, 4000, true},      // wide block that reaches line 500
		{400, 600, true},     // narrow-ish block, scanned over the block
		{501, 100000, false}, // very wide block past everything
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d-%d", c.start, c.end), func(t *testing.T) {
			if got := changed.touches("a.go", c.start, c.end); got != c.want {
				t.Errorf("touches(a.go, %d, %d) = %v, want %v", c.start, c.end, got, c.want)
			}
		})
	}
}

func TestIsRelevantGoFile(t *testing.T) {
	tests := map[string]bool{
		"lib/a.go":            true,
		"lib/a_test.go":       false,
		"lib/testdata/a.go":   true,
		"README.md":           false,
		"lib/a.go.tmpl":       false,
		"_test.go":            false,
		"tools/vulngate/x.go": true,
	}
	for path, want := range tests {
		t.Run(path, func(t *testing.T) {
			if got := isRelevantGoFile(path); got != want {
				t.Errorf("isRelevantGoFile(%q) = %v, want %v", path, got, want)
			}
		})
	}
}

// describeChanged renders a change set in a stable, comparable form.
func describeChanged(changed changedLines) string {
	paths := make([]string, 0, len(changed))
	for path := range changed {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	parts := []string{}
	for _, path := range paths {
		lines := make([]int, 0, len(changed[path]))
		for line := range changed[path] {
			lines = append(lines, line)
		}
		sort.Ints(lines)
		parts = append(parts, fmt.Sprintf("%s:%v", path, lines))
	}
	return strings.Join(parts, " ")
}

func fromWant(want map[string][]int) changedLines {
	result := changedLines{}
	for path, lines := range want {
		for _, line := range lines {
			result.add(path, line)
		}
	}
	return result
}

// TestParseDiffBodyLineThatLooksLikeAHeader is the case prefix matching alone
// gets wrong. An added source line beginning with "++ " — which a Go raw string
// literal may well contain — reaches the diff as "+++ …", the exact shape of a
// file header. Read as one, it would retarget the parser and silently drop
// every later hunk of that file. Under-reporting the changed lines is the
// direction that weakens the gate, so this is pinned.
func TestParseDiffBodyLineThatLooksLikeAHeader(t *testing.T) {
	// Verbatim shape of `git diff --unified=0` over a file containing a raw
	// string whose second line is "++ …".
	input := `diff --git a/lib/a.go b/lib/a.go
--- /dev/null
+++ b/lib/a.go
@@ -0,0 +1,5 @@
+package a
+
+const x = ` + "`" + `
+++ this line starts with a plus plus
+` + "`" + `
@@ -0,0 +20,1 @@
+func f() {}
diff --git a/lib/b.go b/lib/b.go
--- a/lib/b.go
+++ b/lib/b.go
@@ -7,0 +8,2 @@
+	x := 1
+	_ = x
`
	got, err := parseDiff(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := fromWant(map[string][]int{
		// Line 20 is the discriminating one: read as a header, the misleading
		// body line above would retarget the parser at a file called
		// "this line starts with a plus plus", which is not a Go file, and the
		// hunk would be dropped.
		"lib/a.go": {1, 2, 3, 4, 5, 20},
		"lib/b.go": {8, 9},
	})
	if describeChanged(got) != describeChanged(want) {
		t.Errorf("got %s, want %s", describeChanged(got), describeChanged(want))
	}
}

// TestParseDiffBodyLineThatLooksLikeAFileSection is the same trap for the other
// two structural prefixes a body line can imitate.
func TestParseDiffBodyLineThatLooksLikeAFileSection(t *testing.T) {
	input := `diff --git a/lib/a.go b/lib/a.go
--- a/lib/a.go
+++ b/lib/a.go
@@ -0,0 +1,3 @@
+diff --git a/x b/x
+@@ -1,2 +3,4 @@
+++ b/lib/injected.go
@@ -10,0 +20,1 @@
+	last := true
`
	got, err := parseDiff(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Everything belongs to lib/a.go: nothing in the body may introduce a file
	// or a hunk.
	want := fromWant(map[string][]int{"lib/a.go": {1, 2, 3, 20}})
	if describeChanged(got) != describeChanged(want) {
		t.Errorf("got %s, want %s", describeChanged(got), describeChanged(want))
	}
}

// TestParseDiffDeletedFileBodyIsSkipped pins that the body of a hunk belonging
// to an unmeasured file is consumed too. Otherwise a removed line beginning
// with "-- " or a removed "+++ …" line could be read as structure.
func TestParseDiffDeletedFileBodyIsSkipped(t *testing.T) {
	input := `diff --git a/lib/gone.go b/lib/gone.go
--- a/lib/gone.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package gone
-
-const y = 1
diff --git a/lib/kept.go b/lib/kept.go
--- a/lib/kept.go
+++ b/lib/kept.go
@@ -4,0 +5,1 @@
+	kept := true
`
	got, err := parseDiff(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := fromWant(map[string][]int{"lib/kept.go": {5}})
	if describeChanged(got) != describeChanged(want) {
		t.Errorf("got %s, want %s", describeChanged(got), describeChanged(want))
	}
}
