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

package domain

import (
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
)

// TestValidateRefusesADeeplyNestedScript pins that a channel script deep enough
// to crash the parser is rejected at validation, at the offending field's path,
// so it never reaches the runtime.
func TestValidateRefusesADeeplyNestedScript(t *testing.T) {
	env := validEnvironment()
	deep := strings.Repeat("(", jsguard.MaxScriptDepth+1) + "1" + strings.Repeat(")", jsguard.MaxScriptDepth+1)
	env.Zones[0].Assets[0].Channels[0].Source.Script.Code = deep

	err := Validate(env)
	paths := problemPaths(t, err)
	want := "zones[0].assets[0].channels[0].source.script.code"
	found := false
	for _, p := range paths {
		if p == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a problem at %q, got %v", want, paths)
	}
}

func TestValidateAcceptsAnOrdinaryScript(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].Channels[0].Source.Script.Code =
		`var v = moses.device.state.get("x"); moses.service.send(Math.max(0, v));`
	if err := Validate(env); err != nil {
		t.Fatalf("an ordinary script was refused: %v", err)
	}
}
