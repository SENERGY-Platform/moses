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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// frame is one entry of a govulncheck finding trace.
type frame struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Package  string `json:"package"`
	Function string `json:"function"`
}

// finding is one govulncheck finding.
//
// FixedVersion is the field this gate trusts. Deriving fixability from the osv
// entries is a trap: an entry may carry a `fixed` event for a different module
// path than the trace. GO-2026-4887 here is exactly that - fixed in
// github.com/moby/moby/v2, unfixed in github.com/docker/docker, which is what
// this module requires. govulncheck filters by module path and sets the field at
// every scan level.
type finding struct {
	OSV          string  `json:"osv"`
	FixedVersion string  `json:"fixed_version"`
	Trace        []frame `json:"trace"`
}

// config is the header message govulncheck emits before anything else. Its
// presence is the proof that the scanner actually started.
type config struct {
	ScannerName    string `json:"scanner_name"`
	ScannerVersion string `json:"scanner_version"`
	DB             string `json:"db"`
	ScanLevel      string `json:"scan_level"`
}

// message is one element of the govulncheck json stream. Exactly one field is
// populated per message.
type message struct {
	Config  *config  `json:"config"`
	Finding *finding `json:"finding"`
}

// scanResult is what a parsed govulncheck stream tells us.
type scanResult struct {
	config   *config
	findings []finding
}

// parseStream reads the govulncheck json output.
//
// The output is a sequence of concatenated json objects rather than an array, so
// it is decoded in a loop. A truncated or malformed stream is an error and never
// an empty result: a scanner that died halfway through has not established that
// there is nothing to find.
func parseStream(r io.Reader) (*scanResult, error) {
	result := &scanResult{}
	decoder := json.NewDecoder(r)
	for {
		var msg message
		err := decoder.Decode(&msg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("govulncheck output is not a valid json stream: %w", err)
		}
		switch {
		case msg.Config != nil:
			result.config = msg.Config
		case msg.Finding != nil:
			result.findings = append(result.findings, *msg.Finding)
		}
	}
	if result.config == nil {
		return nil, errors.New("govulncheck output carried no config message, so the scan never started")
	}
	return result, nil
}

// called reports whether this finding describes reachable code.
//
// govulncheck emits a finding per scan level: a module level finding whose trace
// is the module alone, a package level one, and a symbol level one whose frames
// all carry a function. Only the last is a call into vulnerable code; the others
// merely say the module is required or imported.
func (this *finding) called() bool {
	for _, f := range this.Trace {
		if f.Function != "" {
			return true
		}
	}
	return false
}

// vulnerableFrame returns the frame naming the vulnerable module.
//
// It is trace[0], NOT the last frame. govulncheck builds the trace from the
// vulnerable symbol outward to the entry point in our own code
// (internal/vulncheck.traceFromEntries walks the call stack in reverse and
// marks index 0 as the sink), so the last frame is always this module. Reading
// the last frame would classify every finding as a vulnerability in our own
// code. govulncheck's own text renderer takes Trace[0] too — under a local
// variable misleadingly named lastFrame.
func (this *finding) vulnerableFrame() (frame, bool) {
	if len(this.Trace) == 0 {
		return frame{}, false
	}
	return this.Trace[0], true
}

// vulnerability is one deduplicated called finding, as the gate reports it.
type vulnerability struct {
	id      string
	module  string
	version string
	fixed   string // "" when no fix exists for this module path
	class   string
}

// blocking reports whether this vulnerability fails the gate: the criterion is
// a *fixable* vulnerability in something we require *directly*.
func (this *vulnerability) blocking() bool {
	return this.fixed != "" && actionable(this.class)
}

// collectCalled reduces the findings to the distinct called vulnerabilities.
//
// The key is the pair of advisory id and vulnerable module, not the id alone: a
// single advisory can affect more than one module path, and the fix status is
// per module. govulncheck emits one symbol level finding per call stack, so the
// same pair arrives many times over — 78 times for the docker findings in this
// repository.
func collectCalled(findings []finding, reqs *requirements) ([]vulnerability, error) {
	type key struct {
		id     string
		module string
	}
	seen := map[key]*vulnerability{}
	for i := range findings {
		f := &findings[i]
		if !f.called() {
			continue
		}
		vf, ok := f.vulnerableFrame()
		if !ok {
			return nil, fmt.Errorf("govulncheck reported finding %q with an empty trace", f.OSV)
		}
		if vf.Module == "" {
			return nil, fmt.Errorf("govulncheck reported finding %q whose vulnerable frame names no module", f.OSV)
		}
		k := key{id: f.OSV, module: vf.Module}
		if existing, found := seen[k]; found {
			// Every finding for the same module reports the same fix, but if a
			// future govulncheck ever disagreed with itself we take the
			// pessimistic view rather than the first one seen.
			if existing.fixed == "" && f.FixedVersion != "" {
				existing.fixed = f.FixedVersion
			}
			continue
		}
		seen[k] = &vulnerability{
			id:      f.OSV,
			module:  vf.Module,
			version: vf.Version,
			fixed:   f.FixedVersion,
			class:   reqs.classify(vf.Module),
		}
	}

	result := make([]vulnerability, 0, len(seen))
	for _, v := range seen {
		result = append(result, *v)
	}
	// Map iteration is random; the report has to be stable so that two runs on
	// the same tree are comparable.
	sort.Slice(result, func(i, j int) bool {
		if result[i].id != result[j].id {
			return result[i].id < result[j].id
		}
		return result[i].module < result[j].module
	})
	return result, nil
}
