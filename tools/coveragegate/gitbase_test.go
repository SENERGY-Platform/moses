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

// fakeRefs is a repository described by its refs, so the base selection rules
// can be checked without creating one.
type fakeRefs struct {
	// refs maps a ref as it is asked for to the full name it resolves to.
	refs   map[string]string
	branch string
	remote []string
}

func (this *fakeRefs) exists(ref string) bool {
	_, ok := this.refs[ref]
	return ok
}

func (this *fakeRefs) fullName(ref string) string { return this.refs[ref] }
func (this *fakeRefs) currentBranch() string      { return this.branch }
func (this *fakeRefs) remotes() []string          { return this.remote }
func (this *fakeRefs) mergeBase(ref string) string {
	if _, ok := this.refs[ref]; !ok {
		return ""
	}
	return "base-of-" + ref
}

func TestSelectBase(t *testing.T) {
	tests := []struct {
		name       string
		repo       *fakeRefs
		configured string
		wantName   string
		wantMerge  string
	}{
		{
			// The situation in this repository: origin/HEAD is a symbolic ref
			// pointing at the default branch.
			name: "origin/HEAD wins and is dereferenced",
			repo: &fakeRefs{
				refs: map[string]string{
					"origin/HEAD":       "refs/remotes/origin/master",
					"refs/heads/master": "refs/heads/master",
				},
				branch: "chore/modernize-toolchain",
				remote: []string{"origin"},
			},
			wantName:  "origin/master",
			wantMerge: "base-of-origin/HEAD",
		},
		{
			// rev-parse --symbolic-full-name answers for a branch that does not
			// exist, so existence has to be checked first. Were it not, main
			// would be chosen over master here and the merge base would be
			// empty.
			name: "a non-existent main does not shadow master",
			repo: &fakeRefs{
				refs: map[string]string{
					"refs/heads/master": "refs/heads/master",
				},
				branch: "feature",
			},
			wantName:  "master",
			wantMerge: "base-of-refs/heads/master",
		},
		{
			// On master itself, master must not be its own base — otherwise
			// the measured range is empty and the gate sees nothing.
			name: "the current branch is skipped",
			repo: &fakeRefs{
				refs: map[string]string{
					"refs/heads/master":  "refs/heads/master",
					"refs/heads/develop": "refs/heads/develop",
				},
				branch: "master",
			},
			wantName:  "develop",
			wantMerge: "base-of-refs/heads/develop",
		},
		{
			// The other half of the same rule: after a push, the branch's own
			// remote copy would make the range empty. A credential that failed
			// the gate before the push has to keep failing after it.
			name: "the current branch's remote copy is skipped",
			repo: &fakeRefs{
				refs: map[string]string{
					"origin/HEAD":        "refs/remotes/origin/feature",
					"refs/heads/develop": "refs/heads/develop",
				},
				branch: "feature",
				remote: []string{"origin"},
			},
			wantName:  "develop",
			wantMerge: "base-of-refs/heads/develop",
		},
		{
			name: "a second remote's copy of the current branch is skipped too",
			repo: &fakeRefs{
				refs: map[string]string{
					"origin/HEAD":        "refs/remotes/upstream/feature",
					"refs/heads/develop": "refs/heads/develop",
				},
				branch: "feature",
				remote: []string{"origin", "upstream"},
			},
			wantName:  "develop",
			wantMerge: "base-of-refs/heads/develop",
		},
		{
			// The integration-branch case, and the only one where @{u} belongs:
			// on master with unpushed commits, @{u} is origin/master.
			name: "the upstream is the last resort",
			repo: &fakeRefs{
				refs: map[string]string{
					"refs/heads/master": "refs/heads/master",
					"@{u}":              "refs/remotes/origin/master",
				},
				branch: "master",
			},
			wantName:  "origin/master",
			wantMerge: "base-of-@{u}",
		},
		{
			name: "a configured base ref goes first",
			repo: &fakeRefs{
				refs: map[string]string{
					"release":           "refs/heads/release",
					"origin/HEAD":       "refs/remotes/origin/master",
					"refs/heads/master": "refs/heads/master",
				},
				branch: "fix/on-release",
			},
			configured: "release",
			wantName:   "release",
			wantMerge:  "base-of-release",
		},
		{
			name: "nothing resolves",
			repo: &fakeRefs{
				refs:   map[string]string{},
				branch: "feature",
			},
			wantName:  "",
			wantMerge: "",
		},
		{
			// A detached head has no own branch to exclude, so the first
			// candidate that exists is taken.
			name: "detached head",
			repo: &fakeRefs{
				refs: map[string]string{
					"refs/heads/master": "refs/heads/master",
				},
				branch: "",
			},
			wantName:  "master",
			wantMerge: "base-of-refs/heads/master",
		},
		{
			name: "the candidate order is main before master",
			repo: &fakeRefs{
				refs: map[string]string{
					"refs/heads/main":   "refs/heads/main",
					"refs/heads/master": "refs/heads/master",
				},
				branch: "feature",
			},
			wantName:  "main",
			wantMerge: "base-of-refs/heads/main",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := selectBase(test.repo, test.configured)
			if got.name != test.wantName || got.mergeBase != test.wantMerge {
				t.Errorf("selectBase() = {%q, %q}, want {%q, %q}",
					got.name, got.mergeBase, test.wantName, test.wantMerge)
			}
		})
	}
}

func TestIsOwnRef(t *testing.T) {
	tests := []struct {
		name    string
		full    string
		branch  string
		remotes []string
		want    bool
	}{
		{"the branch itself", "refs/heads/feature", "feature", nil, true},
		{"its remote copy", "refs/remotes/origin/feature", "feature", []string{"origin"}, true},
		{"another branch", "refs/heads/master", "feature", []string{"origin"}, false},
		{"another branch's remote copy", "refs/remotes/origin/master", "feature", []string{"origin"}, false},
		{"a remote that is not configured", "refs/remotes/other/feature", "feature", []string{"origin"}, false},
		{"detached head", "refs/heads/feature", "", []string{"origin"}, false},
		{"empty ref", "", "feature", []string{"origin"}, false},
		// A slash in a branch name must still match exactly.
		{"branch with a slash", "refs/heads/chore/x", "chore/x", nil, true},
		{"prefix of the branch name", "refs/heads/feat", "feature", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isOwnRef(test.full, test.branch, test.remotes); got != test.want {
				t.Errorf("isOwnRef(%q, %q, %v) = %v, want %v", test.full, test.branch, test.remotes, got, test.want)
			}
		})
	}
}

func TestReadBaseRefSetting(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		want    string
		wantErr bool
	}{
		{name: "absent", src: "test=enforce\n", want: ""},
		{name: "double quoted", src: `BASE_REF="release"` + "\n", want: "release"},
		{name: "single quoted", src: "BASE_REF='release'\n", want: "release"},
		{name: "unquoted", src: "BASE_REF=release\n", want: "release"},
		{name: "unquoted with a trailing comment", src: "BASE_REF=release # cut from release\n", want: "release"},
		{name: "quoted with a trailing comment", src: `BASE_REF="release" # cut from release` + "\n", want: "release"},
		{name: "indented", src: "  BASE_REF=release\n", want: "release"},
		{name: "exported", src: "export BASE_REF=release\n", want: "release"},
		{name: "commented out", src: "# BASE_REF=release\n", want: ""},
		{name: "empty value", src: "BASE_REF=\n", want: ""},
		{name: "empty quoted value", src: `BASE_REF=""` + "\n", want: ""},
		// Shell semantics: the later assignment wins.
		{name: "assigned twice", src: "BASE_REF=first\nBASE_REF=second\n", want: "second"},
		// A name that is not a name: git would read it as an option, and some
		// options even verify successfully.
		{name: "leading dash is refused", src: "BASE_REF=--git-dir=/etc\n", wantErr: true},
		{name: "leading dash, quoted, is refused", src: `BASE_REF="-upstream"` + "\n", wantErr: true},
		// A key that merely starts the same way is a different key.
		{name: "similar key is not read", src: "BASE_REFS=release\n", want: ""},
		{name: "branch name with a slash", src: "BASE_REF=release/2026-08\n", want: "release/2026-08"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := readBaseRefSetting([]byte(test.src))
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

// TestReadBaseRefSettingOnThisRepository pins that the real configuration of
// this repository pins no base, so the default chain applies.
func TestReadBaseRefSettingOnThisRepository(t *testing.T) {
	got, err := readBaseRefSetting([]byte(strings.Join([]string{
		"# Quality gate configuration for this repository.",
		"build=enforce",
		"CRITICAL_PATHS='lib/repo/* lib/state/persistence.go'",
		"STACK=go",
	}, "\n")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want no configured base", got)
	}
}

func TestReadableRefName(t *testing.T) {
	tests := []struct {
		full, fallback, want string
	}{
		{"refs/heads/master", "x", "master"},
		{"refs/remotes/origin/master", "x", "origin/master"},
		{"refs/tags/v1.0.0", "x", "v1.0.0"},
		{"refs/heads/chore/modernize-toolchain", "x", "chore/modernize-toolchain"},
		{"", "@{u}", "@{u}"},
	}
	for _, test := range tests {
		t.Run(test.full, func(t *testing.T) {
			if got := readableRefName(test.full, test.fallback); got != test.want {
				t.Errorf("readableRefName(%q, %q) = %q, want %q", test.full, test.fallback, got, test.want)
			}
		})
	}
}
