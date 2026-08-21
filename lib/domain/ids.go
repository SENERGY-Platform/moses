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

import "github.com/google/uuid"

// AssignIds fills in an id wherever the document left one empty, so that an
// author (or an agent) can hand in a tree without inventing uuids. Existing ids
// are kept: they are what makes a second PUT of the same document an update
// rather than a replacement, and what keeps runtime state attached.
func AssignIds(env *Environment) {
	if env.Id == "" {
		env.Id = uuid.NewString()
	}
	for i := range env.Zones {
		assignZoneIds(&env.Zones[i])
	}
}

func assignZoneIds(zone *Zone) {
	if zone.Id == "" {
		zone.Id = uuid.NewString()
	}
	for i := range zone.Zones {
		assignZoneIds(&zone.Zones[i])
	}
	for i := range zone.Assets {
		asset := &zone.Assets[i]
		if asset.Id == "" {
			asset.Id = uuid.NewString()
		}
		for j := range asset.Channels {
			if asset.Channels[j].Id == "" {
				asset.Channels[j].Id = uuid.NewString()
			}
		}
	}
}
