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

package migration

import (
	"sort"
	"strings"

	"github.com/SENERGY-Platform/moses/lib/state"
)

// CandidateIds returns the environment ids these worlds would be stored under,
// sorted and without duplicates.
//
// It exists so that the caller which queries the environment store and Plan(),
// which compares against the answer, cannot disagree about the id: if the caller
// asked for a different id than the plan produces, the skip detection would
// silently stop working and a second run would overwrite. The rule is that the
// environment id is the legacy world id (ref domain.FromLegacyWorld, which is
// given no map key fallback for the world itself), trimmed.
//
// A world without an id contributes nothing, because there is no id to ask
// about; Plan() blocks it (ref ErrNoLegacyId).
func CandidateIds(worlds map[string]*state.World) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, world := range worlds {
		if world == nil {
			continue
		}
		id := strings.TrimSpace(world.Id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
