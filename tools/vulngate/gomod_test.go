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
	"sort"
	"strings"
	"testing"
)

func TestParseGoModDirectRequirements(t *testing.T) {
	tests := []struct {
		name       string
		src        string
		mainModule string
		direct     []string
		wantErr    bool
	}{
		{
			name: "block with indirect markers",
			src: `module example.com/svc

go 1.25.0

require (
	example.com/direct v1.0.0
	example.com/indirect v2.0.0 // indirect
)
`,
			mainModule: "example.com/svc",
			direct:     []string{"example.com/direct"},
		},
		{
			name: "several require blocks, as the go tooling writes them",
			src: `module example.com/svc

require (
	example.com/a v1.0.0
	example.com/b v1.0.0 // indirect
)

require (
	example.com/c v1.0.0
)

require example.com/d v1.0.0

require example.com/e v1.0.0 // indirect
`,
			mainModule: "example.com/svc",
			direct:     []string{"example.com/a", "example.com/c", "example.com/d"},
		},
		{
			// The exact trap the classification rests on: a whole-line comment
			// must not be read as the suffix comment of the line below it.
			name: "standalone indirect comment does not mark the next line",
			src: `module example.com/svc

require (
	// indirect
	example.com/a v1.0.0
)
`,
			mainModule: "example.com/svc",
			direct:     []string{"example.com/a"},
		},
		{
			name: "indirect with a trailing reason",
			src: `module example.com/svc

require (
	example.com/a v1.0.0 // indirect; pulled in by b
	example.com/b v1.0.0
)
`,
			mainModule: "example.com/svc",
			direct:     []string{"example.com/b"},
		},
		{
			// "indirect" has to be the whole first word. A comment that merely
			// starts with those letters is prose, and dropping the requirement
			// would make a direct dependency invisible to the gate.
			name: "indirectly is not indirect",
			src: `module example.com/svc

require (
	example.com/a v1.0.0 // indirectly needed, keep
	example.com/b v1.0.0 // indirect
)
`,
			mainModule: "example.com/svc",
			direct:     []string{"example.com/a"},
		},
		{
			name: "other blocks are skipped entirely",
			src: `module example.com/svc

require example.com/a v1.0.0

replace (
	example.com/replaced v1.0.0 => example.com/fork v1.1.0
)

exclude (
	example.com/excluded v0.1.0
)

retract (
	v1.0.1
)

tool (
	example.com/tooling/cmd/gen
)

godebug (
	default=go1.25
)
`,
			mainModule: "example.com/svc",
			direct:     []string{"example.com/a"},
		},
		{
			name: "single line replace and exclude are not requirements",
			src: `module example.com/svc

require example.com/a v1.0.0
replace example.com/b v1.0.0 => example.com/fork v1.0.0
exclude example.com/c v1.0.0
`,
			mainModule: "example.com/svc",
			direct:     []string{"example.com/a"},
		},
		{
			name: "comment on the block opening line",
			src: `module example.com/svc

require ( // the direct ones
	example.com/a v1.0.0
)
`,
			mainModule: "example.com/svc",
			direct:     []string{"example.com/a"},
		},
		{
			name: "quoted module path",
			src: `module "example.com/svc"

require (
	"example.com/a" v1.0.0
)
`,
			mainModule: "example.com/svc",
			direct:     []string{"example.com/a"},
		},
		{
			// A `//` inside a quoted token is part of the token. Treating it as
			// the start of a comment would truncate the path.
			name: "double slash inside a quoted token is not a comment",
			src: `module example.com/svc

require (
	"example.com/a//b" v1.0.0
)
`,
			mainModule: "example.com/svc",
			direct:     []string{"example.com/a//b"},
		},
		{
			name:    "no module line",
			src:     "require example.com/a v1.0.0\n",
			wantErr: true,
		},
		{
			name:    "require without a version",
			src:     "module example.com/svc\n\nrequire (\n\texample.com/a\n)\n",
			wantErr: true,
		},
		{
			name:       "no requirements at all",
			src:        "module example.com/svc\n\ngo 1.25.0\n",
			mainModule: "example.com/svc",
			direct:     nil,
		},
		{
			name:       "windows line endings",
			src:        "module example.com/svc\r\n\r\nrequire (\r\n\texample.com/a v1.0.0\r\n\texample.com/b v1.0.0 // indirect\r\n)\r\n",
			mainModule: "example.com/svc",
			direct:     []string{"example.com/a"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reqs, err := parseGoMod([]byte(test.src))
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got requirements %+v", reqs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if reqs.mainModule != test.mainModule {
				t.Errorf("main module: got %q, want %q", reqs.mainModule, test.mainModule)
			}
			got := make([]string, 0, len(reqs.direct))
			for path := range reqs.direct {
				got = append(got, path)
			}
			sort.Strings(got)
			want := append([]string(nil), test.direct...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("direct requirements: got %v, want %v", got, want)
			}
		})
	}
}

func TestRequirementsClassify(t *testing.T) {
	reqs := &requirements{
		mainModule: "example.com/svc",
		direct:     map[string]bool{"example.com/direct": true},
	}
	tests := []struct {
		module string
		want   string
	}{
		{"example.com/direct", classDirect},
		{"example.com/transitive", classTransitive},
		{"example.com/svc", classDirect},
		{stdlibModulePath, classStdlib},
		{toolchainModulePath, classStdlib},
		// The vulnerability database names the Go distribution with the
		// pseudo paths above; a module literally called "std" is a third party
		// module and stays transitive unless required directly.
		{"std", classTransitive},
		{"", classTransitive},
	}
	for _, test := range tests {
		t.Run(test.module, func(t *testing.T) {
			if got := reqs.classify(test.module); got != test.want {
				t.Errorf("classify(%q) = %q, want %q", test.module, got, test.want)
			}
		})
	}
}

func TestActionable(t *testing.T) {
	if !actionable(classDirect) || !actionable(classStdlib) {
		t.Error("direct and stdlib findings must be actionable")
	}
	if actionable(classTransitive) {
		t.Error("a transitive finding must not be actionable: the criterion excludes it")
	}
}

func TestSplitComment(t *testing.T) {
	tests := []struct {
		line    string
		code    string
		comment string
	}{
		{"require a v1.0.0", "require a v1.0.0", ""},
		{"\ta v1.0.0 // indirect", "\ta v1.0.0 ", "// indirect"},
		{"// whole line", "", "// whole line"},
		{`"a//b" v1.0.0 // indirect`, `"a//b" v1.0.0 `, "// indirect"},
		{"`a//b` v1.0.0 // indirect", "`a//b` v1.0.0 ", "// indirect"},
		{`"a\"//b" v1.0.0`, `"a\"//b" v1.0.0`, ""},
		{"a v1.0.0 // indirect // twice", "a v1.0.0 ", "// indirect // twice"},
		{"a v1.0.0 /", "a v1.0.0 /", ""},
	}
	for _, test := range tests {
		t.Run(test.line, func(t *testing.T) {
			code, comment := splitComment(test.line)
			if code != test.code || comment != test.comment {
				t.Errorf("splitComment(%q) = (%q, %q), want (%q, %q)", test.line, code, comment, test.code, test.comment)
			}
		})
	}
}

func TestIsIndirect(t *testing.T) {
	tests := []struct {
		comment string
		want    bool
	}{
		{"// indirect", true},
		{"//indirect", true},
		{"//   indirect   ", true},
		{"// indirect; via sarama", true},
		{"", false},
		{"// indirectly", false},
		{"// not indirect", false},
		{"// direct", false},
	}
	for _, test := range tests {
		t.Run(test.comment, func(t *testing.T) {
			if got := isIndirect(test.comment); got != test.want {
				t.Errorf("isIndirect(%q) = %v, want %v", test.comment, got, test.want)
			}
		})
	}
}
