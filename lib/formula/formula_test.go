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

package formula

import (
	"strings"
	"testing"
)

func TestCompileAndEvaluate(t *testing.T) {
	program, err := Compile("(last - pv) * factor", map[string]string{
		"last": "channel.ch-last", "pv": "channel.ch-pv", "factor": "context.grid_factor",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := program.Evaluate(map[string]interface{}{"last": 10.0, "pv": 4.0, "factor": 2.0})
	if err != nil || got != 12 {
		t.Errorf("expected 12, got %v (%v)", got, err)
	}
}

func TestAComparisonPublishesAFlag(t *testing.T) {
	program, err := Compile("celsius > 80", map[string]string{"celsius": "asset.celsius"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := program.Evaluate(map[string]interface{}{"celsius": 92.0}); got != 1 {
		t.Errorf("expected 1, got %v", got)
	}
	if got, _ := program.Evaluate(map[string]interface{}{"celsius": 60.0}); got != 0 {
		t.Errorf("expected 0, got %v", got)
	}
}

func TestCompileRefusals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		expression string
		inputs     map[string]string
		fragment   string
	}{
		{"empty expression", " ", nil, "must not be empty"},
		{"syntax error", "a +* b", map[string]string{"a": "asset.a", "b": "asset.b"}, "unable to compile"},
		{"undeclared variable", "a + missing", map[string]string{"a": "asset.a"}, "unknown name missing"},
		{"bad input name", "a", map[string]string{"a b": "asset.a"}, "not usable as a variable name"},
		{"bad reference", "a", map[string]string{"a": "irgendwo.a"}, "must start with"},
		{"bare reference prefix", "a", map[string]string{"a": "asset."}, "must start with"},
		{"non numeric result", `"text"`, nil, "publishes numbers"},
	} {
		_, err := Compile(tc.expression, tc.inputs)
		if err == nil || !strings.Contains(err.Error(), tc.fragment) {
			t.Errorf("%s: expected %q, got %v", tc.name, tc.fragment, err)
		}
	}
}

// Guards the property the engine was chosen for: an expression cannot loop, so
// evaluation always terminates and needs no interrupt machinery.
func TestLoopsDoNotCompile(t *testing.T) {
	if _, err := Compile("while true { 1 }", map[string]string{}); err == nil {
		t.Error("a loop construct has to be refused")
	}
}
