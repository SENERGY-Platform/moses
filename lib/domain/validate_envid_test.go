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
)

// The crash brake stores the environment id in a fixed slot, so an id longer than
// the limit or holding a control character is a 400 at the id, not a value that is
// truncated or read as another environment later.
func TestValidateBoundsTheEnvironmentId(t *testing.T) {
	withId := func(id string) Environment {
		env := validEnvironment()
		env.Id = id
		return env
	}
	if err := Validate(withId(strings.Repeat("a", MaxExternalIdLength))); err != nil {
		t.Fatalf("an id at the limit has to pass: %v", err)
	}
	assertHasPath(t, Validate(withId(strings.Repeat("a", MaxExternalIdLength+1))), "id")
	assertHasPath(t, Validate(withId("victim\x1fsuffix")), "id")
	assertHasPath(t, Validate(withId("tab\tid")), "id")
}
