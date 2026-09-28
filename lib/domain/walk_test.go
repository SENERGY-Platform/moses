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

// The platform check reports under the paths WalkAssets hands out, and one answer
// merges its problems with Validate's, so both have to name the same field.
func TestWalkAssetsHandsOutThePathsValidateReportsUnder(t *testing.T) {
	env := validEnvironment()
	//no device type anywhere, so Validate reports every asset, and an unknown
	//direction on every channel
	broken := func(name string) Asset {
		return Asset{Name: name, Kind: AssetMeter, Channels: []Channel{
			{Name: "a", Direction: "x", Source: Source{Kind: SourceScript, Script: &ScriptSource{Code: "1;"}}},
			{Name: "b", Direction: "x", Source: Source{Kind: SourceScript, Script: &ScriptSource{Code: "1;"}}},
		}}
	}
	env.Zones[0].Assets = []Asset{broken("eins"), broken("zwei")}
	env.Zones[0].Zones = []Zone{{Name: "Raum", Type: ZoneRoom, Assets: []Asset{broken("drei")},
		Zones: []Zone{{Name: "Nische", Type: ZoneRoom, Assets: []Asset{broken("vier")}}}}}
	env.Zones = append(env.Zones, Zone{Name: "Halle 2", Type: ZoneHall, Assets: []Asset{broken("fünf")}})

	reported := map[string]bool{}
	for _, path := range problemPaths(t, Validate(env)) {
		reported[path] = true
	}
	visited := []string{}
	WalkAssets(env, func(path string, asset Asset) {
		visited = append(visited, asset.Name)
		if !reported[path+".external_type_id"] {
			t.Errorf("%s: Validate reports nothing at %s.external_type_id", asset.Name, path)
		}
		for i := range asset.Channels {
			if !reported[ChannelPath(path, i)+".direction"] {
				t.Errorf("%s: Validate reports nothing at %s.direction", asset.Name, ChannelPath(path, i))
			}
		}
	})
	if got := strings.Join(visited, " "); got != "vier drei eins zwei fünf" {
		t.Errorf("expected every asset, sub-zones first as Validate walks, got %q", got)
	}
}

// Validate stops below MaxZoneDepth, and so does the walk: an asset there is not
// checked against the platform either. One at the deepest allowed level is, or
// a device attached there would skip the platform check.
func TestWalkAssetsStopsWhereValidateStops(t *testing.T) {
	env := validEnvironment()
	deepest := &env.Zones[0]
	for i := 1; i < MaxZoneDepth; i++ {
		deepest.Zones = []Zone{{Name: "tief", Type: ZoneRoom}}
		deepest = &deepest.Zones[0]
	}
	//no device type on either, so Validate reports every asset it reaches
	deepest.Assets = []Asset{{Name: "am Rand", Kind: AssetMeter}}
	deepest.Zones = []Zone{{Name: "zu tief", Type: ZoneRoom, Assets: []Asset{{Name: "zu tief", Kind: AssetMeter}}}}
	reported := map[string]bool{}
	for _, path := range problemPaths(t, Validate(env)) {
		reported[path] = true
	}
	visited := map[string]bool{}
	WalkAssets(env, func(path string, asset Asset) {
		visited[asset.Name] = true
		if asset.Name == "zu tief" {
			t.Errorf("an asset below the depth limit was visited at %s", path)
		}
		if asset.ExternalTypeId == "" && !reported[path+".external_type_id"] {
			t.Errorf("Validate did not reach %s, which the walk visited", path)
		}
	})
	if !visited["am Rand"] {
		t.Error("the asset at the deepest allowed level was not visited")
	}
}

// The count decides whether the platform is asked at all, so it has to agree
// with Validate exactly at the limit.
func TestWalkAssetsCountsNodesLikeValidate(t *testing.T) {
	env := validEnvironment()
	//one zone, one asset and one channel so far
	filler := MaxNodes - 3
	for filler > 0 {
		asset := Asset{Name: "Füller", Kind: AssetMeter, ExternalTypeId: "dt"}
		filler--
		if filler > 0 {
			asset.Channels = []Channel{{Name: "k", Direction: Actuator,
				Source: Source{Kind: SourceScript, Script: &ScriptSource{Code: "1;"}}}}
			filler--
		}
		env.Zones[0].Assets = append(env.Zones[0].Assets, asset)
	}
	if nodes := WalkAssets(env, func(string, Asset) {}); nodes != MaxNodes {
		t.Fatalf("expected %d nodes, got %d", MaxNodes, nodes)
	}
	if err := Validate(env); err != nil {
		for _, path := range problemPaths(t, err) {
			if path == "" {
				t.Fatalf("Validate counts more than the walk: %v", err)
			}
		}
	}
	env.Zones = append(env.Zones, Zone{Name: "eine zu viel", Type: ZoneHall})
	if nodes := WalkAssets(env, func(string, Asset) {}); nodes != MaxNodes+1 {
		t.Fatalf("expected %d nodes, got %d", MaxNodes+1, nodes)
	}
	assertHasPath(t, Validate(env), "")
}

// Validation reads every platform id in a query url, so an oversized one has to
// be a 400 at its field and not a query the gateway refuses.
func TestValidateBoundsTheLengthOfPlatformIds(t *testing.T) {
	withIds := func(length int) Environment {
		env := validEnvironment()
		id := strings.Repeat("x", length)
		asset := &env.Zones[0].Assets[0]
		asset.ExternalTypeId, asset.ExternalRef, asset.Channels[0].ExternalRef = id, id, id
		return env
	}
	if err := Validate(withIds(MaxExternalIdLength)); err != nil {
		t.Fatalf("ids at the limit have to pass: %v", err)
	}
	paths := problemPaths(t, Validate(withIds(MaxExternalIdLength+1)))
	expected := []string{
		"zones[0].assets[0].channels[0].external_ref",
		"zones[0].assets[0].external_ref",
		"zones[0].assets[0].external_type_id",
	}
	if strings.Join(paths, " ") != strings.Join(expected, " ") {
		t.Errorf("expected problems at %v, got %v", expected, paths)
	}
}
