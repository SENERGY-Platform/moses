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

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// A PUT whose path id is longer than a crash-brake slot holds, or carries a
// control character, is refused with 400 at the id and writes nothing, so the id
// the brake later stores always decodes back to itself.
func TestPutRefusesAnUnusableEnvironmentId(t *testing.T) {
	cases := map[string]string{
		"too long":     "/environments/" + strings.Repeat("a", domain.MaxExternalIdLength+1),
		"control char": "/environments/victim%1Fx",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			w := newValidateWitnesses()
			resp := do(t, w.router, http.MethodPut, path, "user-a", minimalEnvironment())
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
			}
			problems := domain.ValidationError{}
			if err := json.Unmarshal(resp.Body.Bytes(), &problems); err != nil {
				t.Fatalf("expected the problem list, got %s", resp.Body.String())
			}
			found := false
			for _, problem := range problems.Problems {
				found = found || problem.Path == "id"
			}
			if !found {
				t.Fatalf("expected a problem at id, got %+v", problems.Problems)
			}
			if wrote := w.wrote(); len(wrote) != 0 {
				t.Fatalf("a refused id must write nothing, but wrote to: %v", wrote)
			}
		})
	}
}
