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

// The header govulncheck emits first. Reused by the stream tests.
const configMessage = `{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck",` +
	`"scanner_version":"v1.7.0","db":"https://vuln.go.dev","scan_level":"symbol","scan_mode":"source"}}`

func TestParseStream(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantFindings int
		wantErr      string
	}{
		{
			// The real output is concatenated objects, pretty printed and with
			// no separators and no enclosing array.
			name: "concatenated objects with progress, osv and sbom messages",
			input: configMessage + "\n" +
				`{"progress":{"message":"Fetching vulnerabilities from the database..."}}` + "\n" +
				`{"osv":{"id":"GO-2026-4887","affected":[{"package":{"name":"github.com/docker/docker","ecosystem":"Go"}}]}}` + "\n" +
				`{"finding":{"osv":"GO-2026-4887","trace":[{"module":"github.com/docker/docker"}]}}` + "\n" +
				`{"SBOM":{"go_version":"go1.26.6"}}` + "\n",
			wantFindings: 1,
		},
		{
			name: "pretty printed without newlines between objects",
			input: configMessage + `{
  "finding": {
    "osv": "GO-1",
    "trace": [
      {"module": "example.com/a", "version": "v1.0.0", "package": "example.com/a", "function": "Boom"}
    ]
  }
}{"finding":{"osv":"GO-2","trace":[{"module":"example.com/b"}]}}`,
			wantFindings: 2,
		},
		{
			// A scanner killed halfway through must not look like a clean scan
			// that found nothing.
			name:    "truncated stream",
			input:   configMessage + `{"finding":{"osv":"GO-1","trace":[{"modu`,
			wantErr: "not a valid json stream",
		},
		{
			name:    "no config message",
			input:   `{"finding":{"osv":"GO-1","trace":[{"module":"example.com/a"}]}}`,
			wantErr: "no config message",
		},
		{
			name:    "empty output",
			input:   "",
			wantErr: "no config message",
		},
		{
			name:    "not json at all",
			input:   "go: downloading golang.org/x/vuln\npanic: boom\n",
			wantErr: "not a valid json stream",
		},
		{
			name:         "config only, nothing found",
			input:        configMessage,
			wantFindings: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := parseStream(strings.NewReader(test.input))
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got none", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(result.findings) != test.wantFindings {
				t.Errorf("findings: got %d, want %d", len(result.findings), test.wantFindings)
			}
			if result.config == nil || result.config.ScannerVersion != "v1.7.0" {
				t.Errorf("config not carried through: %+v", result.config)
			}
		})
	}
}

func TestFindingCalled(t *testing.T) {
	tests := []struct {
		name string
		f    finding
		want bool
	}{
		{
			name: "module level finding is not a call",
			f:    finding{OSV: "GO-1", Trace: []frame{{Module: "example.com/a", Version: "v1.0.0"}}},
			want: false,
		},
		{
			name: "package level finding is not a call",
			f:    finding{OSV: "GO-1", Trace: []frame{{Module: "example.com/a", Package: "example.com/a/pkg"}}},
			want: false,
		},
		{
			name: "symbol level finding is a call",
			f: finding{OSV: "GO-1", Trace: []frame{
				{Module: "example.com/a", Package: "example.com/a/pkg", Function: "Boom"},
				{Module: "example.com/svc", Package: "example.com/svc/lib", Function: "New"},
			}},
			want: true,
		},
		{
			name: "empty trace is not a call",
			f:    finding{OSV: "GO-1"},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.f.called(); got != test.want {
				t.Errorf("called() = %v, want %v", got, test.want)
			}
		})
	}
}

// TestVulnerableFrameIsTheFirstFrame pins the trace direction. govulncheck
// orders the frames from the vulnerable symbol outward to the entry point, so
// the vulnerable module is trace[0] and the *last* frame is always this
// repository. Reading the last frame instead would report every finding as a
// vulnerability in our own code — and, since our own module is never a
// requirement of itself, silently reclassify all of them.
func TestVulnerableFrameIsTheFirstFrame(t *testing.T) {
	// Shape taken verbatim from a real run of this repository.
	f := finding{
		OSV: "GO-2026-4887",
		Trace: []frame{
			{Module: "github.com/docker/docker", Version: "v28.5.2+incompatible", Package: "github.com/docker/docker/api/types/filters", Function: "init"},
			{Module: "github.com/testcontainers/testcontainers-go", Version: "v0.40.0", Package: "github.com/testcontainers/testcontainers-go", Function: "init"},
			{Module: "github.com/SENERGY-Platform/moses", Package: "github.com/SENERGY-Platform/moses/lib/test/server", Function: "init"},
		},
	}
	vf, ok := f.vulnerableFrame()
	if !ok {
		t.Fatal("expected a vulnerable frame")
	}
	if vf.Module != "github.com/docker/docker" {
		t.Errorf("vulnerable module: got %q, want the sink module github.com/docker/docker", vf.Module)
	}
	if vf.Version != "v28.5.2+incompatible" {
		t.Errorf("vulnerable version: got %q, want v28.5.2+incompatible", vf.Version)
	}
	if last := f.Trace[len(f.Trace)-1].Module; last == vf.Module {
		t.Fatal("fixture is useless: the first and last frame name the same module")
	}
}

func TestCollectCalled(t *testing.T) {
	reqs := &requirements{
		mainModule: "example.com/svc",
		direct:     map[string]bool{"example.com/direct": true},
	}

	call := func(osv string, fixed string, module string, version string) finding {
		return finding{OSV: osv, FixedVersion: fixed, Trace: []frame{
			{Module: module, Version: version, Package: module + "/pkg", Function: "Boom"},
			{Module: "example.com/svc", Package: "example.com/svc/lib", Function: "New"},
		}}
	}

	tests := []struct {
		name         string
		findings     []finding
		wantIDs      []string
		wantBlocking []string
		wantErr      string
	}{
		{
			name: "a fixable direct finding blocks",
			findings: []finding{
				call("GO-1", "v1.2.0", "example.com/direct", "v1.0.0"),
			},
			wantIDs:      []string{"GO-1"},
			wantBlocking: []string{"GO-1"},
		},
		{
			// The case this whole tool exists for.
			name: "an unfixable transitive finding does not block",
			findings: []finding{
				call("GO-1", "", "example.com/transitive", "v1.0.0"),
			},
			wantIDs:      []string{"GO-1"},
			wantBlocking: nil,
		},
		{
			name: "a fixable transitive finding does not block either",
			findings: []finding{
				call("GO-1", "v1.2.0", "example.com/transitive", "v1.0.0"),
			},
			wantIDs:      []string{"GO-1"},
			wantBlocking: nil,
		},
		{
			name: "an unfixable direct finding does not block",
			findings: []finding{
				call("GO-1", "", "example.com/direct", "v1.0.0"),
			},
			wantIDs:      []string{"GO-1"},
			wantBlocking: nil,
		},
		{
			name: "a fixable standard library finding blocks",
			findings: []finding{
				call("GO-1", "v1.25.4", stdlibModulePath, "v1.25.0"),
			},
			wantIDs:      []string{"GO-1"},
			wantBlocking: []string{"GO-1"},
		},
		{
			name: "an unfixable standard library finding does not block",
			findings: []finding{
				call("GO-1", "", stdlibModulePath, "v1.25.0"),
			},
			wantIDs:      []string{"GO-1"},
			wantBlocking: nil,
		},
		{
			name: "a fixable toolchain finding blocks",
			findings: []finding{
				call("GO-1", "v1.25.4", toolchainModulePath, "v1.25.0"),
			},
			wantIDs:      []string{"GO-1"},
			wantBlocking: []string{"GO-1"},
		},
		{
			name: "module and package level findings are ignored",
			findings: []finding{
				{OSV: "GO-1", FixedVersion: "v1.2.0", Trace: []frame{{Module: "example.com/direct", Version: "v1.0.0"}}},
				{OSV: "GO-1", FixedVersion: "v1.2.0", Trace: []frame{{Module: "example.com/direct", Package: "example.com/direct/pkg"}}},
			},
			wantIDs:      nil,
			wantBlocking: nil,
		},
		{
			// govulncheck emits one symbol level finding per call stack; the
			// docker findings in this repository arrive 78 times each.
			name: "the same advisory and module collapse to one entry",
			findings: []finding{
				call("GO-1", "", "example.com/transitive", "v1.0.0"),
				call("GO-1", "", "example.com/transitive", "v1.0.0"),
				call("GO-1", "", "example.com/transitive", "v1.0.0"),
			},
			wantIDs:      []string{"GO-1"},
			wantBlocking: nil,
		},
		{
			// One advisory can affect several module paths with different fix
			// status, so the key is the pair and not the id.
			name: "one advisory across two modules stays two entries",
			findings: []finding{
				call("GO-1", "", "example.com/transitive", "v1.0.0"),
				call("GO-1", "v2.0.0", "example.com/direct", "v1.0.0"),
			},
			wantIDs:      []string{"GO-1", "GO-1"},
			wantBlocking: []string{"GO-1"},
		},
		{
			name: "report order is stable regardless of input order",
			findings: []finding{
				call("GO-3", "", "example.com/transitive", "v1.0.0"),
				call("GO-1", "", "example.com/transitive", "v1.0.0"),
				call("GO-2", "", "example.com/transitive", "v1.0.0"),
			},
			wantIDs:      []string{"GO-1", "GO-2", "GO-3"},
			wantBlocking: nil,
		},
		{
			name: "a called finding whose vulnerable frame names no module is an error",
			findings: []finding{
				{OSV: "GO-1", Trace: []frame{{Function: "Boom"}}},
			},
			wantErr: "names no module",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			vulns, err := collectCalled(test.findings, reqs)
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got %+v", test.wantErr, vulns)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotIDs := []string{}
			gotBlocking := []string{}
			for i := range vulns {
				gotIDs = append(gotIDs, vulns[i].id)
				if vulns[i].blocking() {
					gotBlocking = append(gotBlocking, vulns[i].id)
				}
			}
			if strings.Join(gotIDs, ",") != strings.Join(test.wantIDs, ",") {
				t.Errorf("called findings: got %v, want %v", gotIDs, test.wantIDs)
			}
			if strings.Join(gotBlocking, ",") != strings.Join(test.wantBlocking, ",") {
				t.Errorf("blocking findings: got %v, want %v", gotBlocking, test.wantBlocking)
			}
		})
	}
}

// TestCollectCalledDedupeKeepsAFix guards the pessimistic merge: if the same
// advisory and module arrive both with and without a fix version, the fix must
// win, whatever the order.
func TestCollectCalledDedupeKeepsAFix(t *testing.T) {
	reqs := &requirements{mainModule: "example.com/svc", direct: map[string]bool{"example.com/direct": true}}
	frames := []frame{
		{Module: "example.com/direct", Version: "v1.0.0", Package: "example.com/direct/pkg", Function: "Boom"},
		{Module: "example.com/svc", Package: "example.com/svc/lib", Function: "New"},
	}
	for _, order := range [][]finding{
		{{OSV: "GO-1", Trace: frames}, {OSV: "GO-1", FixedVersion: "v1.2.0", Trace: frames}},
		{{OSV: "GO-1", FixedVersion: "v1.2.0", Trace: frames}, {OSV: "GO-1", Trace: frames}},
	} {
		vulns, err := collectCalled(order, reqs)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(vulns) != 1 {
			t.Fatalf("got %d entries, want 1", len(vulns))
		}
		if vulns[0].fixed != "v1.2.0" || !vulns[0].blocking() {
			t.Errorf("fix was dropped by the merge: %+v", vulns[0])
		}
	}
}
