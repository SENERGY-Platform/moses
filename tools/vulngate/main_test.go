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
	"os/exec"
	"strings"
	"testing"
)

// TestLoadRequirementsOnThisModule guards the failure mode that would quietly
// make this gate vacuous. If the direct set came out empty — a parser that
// never leaves the top level, an indirect rule that matches everything — then
// every finding would classify as transitive, nothing could ever block, and the
// gate would report a confident green forever.
//
// The cross-check is deliberately crude and independent of the parser: every
// go.mod line carrying an `// indirect` marker names a module that must not be
// in the direct set. No dependency is named, so ordinary go.mod churn does not
// touch this test.
func TestLoadRequirementsOnThisModule(t *testing.T) {
	reqs, err := loadRequirements()
	if err != nil {
		t.Fatalf("could not read the requirements of this module: %v", err)
	}

	if !strings.Contains(reqs.mainModule, "/") {
		t.Errorf("main module %q does not look like a module path", reqs.mainModule)
	}
	if len(reqs.direct) == 0 {
		t.Fatal("no direct requirement was found, which would make every finding classify as transitive and the gate vacuous")
	}

	src, err := os.ReadFile(goModPath(t))
	if err != nil {
		t.Fatalf("could not read go.mod: %v", err)
	}
	indirect := 0
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || !strings.Contains(trimmed, "// indirect") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		indirect++
		if reqs.direct[fields[0]] {
			t.Errorf("%q carries an `// indirect` marker but was classified direct", fields[0])
		}
		if class := reqs.classify(fields[0]); class != classTransitive {
			t.Errorf("%q carries an `// indirect` marker but classifies as %q", fields[0], class)
		}
	}
	if indirect == 0 {
		t.Skip("this go.mod has no indirect requirements, so the cross-check has nothing to check")
	}

	// And the other direction: something in the direct set must classify as
	// direct, or `actionable` would never be reached for a real module.
	for path := range reqs.direct {
		if class := reqs.classify(path); class != classDirect {
			t.Errorf("direct requirement %q classifies as %q", path, class)
		}
	}
}

// goModPath asks go where the main module's go.mod is, the same way the tool
// does.
func goModPath(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("could not ask go for the go.mod path: %v", err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" || path == os.DevNull {
		t.Skip("not inside a go module")
	}
	return path
}

func TestModuleVersionSuffix(t *testing.T) {
	tests := map[string]string{
		"":                     "",
		"v1.2.3":               "@v1.2.3",
		"v28.5.2+incompatible": "@v28.5.2+incompatible",
		"go1.25.0":             "@go1.25.0",
	}
	for version, want := range tests {
		t.Run(version, func(t *testing.T) {
			if got := moduleVersionSuffix(version); got != want {
				t.Errorf("moduleVersionSuffix(%q) = %q, want %q", version, got, want)
			}
		})
	}
}
