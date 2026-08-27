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

// A reference to an asset defined later in the document is fine - the same
// forward-reference allowance a formula's channel references get, and for the
// same reason: the whole tree is indexed before references are checked.
func TestValidateAcceptsAForwardSubmeterReference(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].SubmeteredBy = "asset-meter-2"
	env.Zones[0].Assets = append(env.Zones[0].Assets, Asset{
		Id: "asset-meter-2", Name: "Hauptzähler", Kind: AssetMeter,
		ExternalTypeId: "urn:infai:ses:device-type:abc",
	})
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsASubmeterReferenceToAMissingAsset(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].SubmeteredBy = "does-not-exist"
	err := Validate(env)
	assertHasPath(t, err, "zones[0].assets[0].submetered_by")
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("expected the message to name the missing id, got %v", err)
	}
}

// A zone id happens to be a valid, unique id in the document, but it is not an
// asset - submetered_by must resolve through assetMeta, not the document wide
// id set channelRefs and claimId already share.
func TestValidateRejectsASubmeterReferenceToANonAsset(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].SubmeteredBy = env.Zones[0].Id
	assertHasPath(t, Validate(env), "zones[0].assets[0].submetered_by")
}

func TestValidateRejectsASelfSubmeteredAsset(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].SubmeteredBy = env.Zones[0].Assets[0].Id
	assertHasPath(t, Validate(env), "zones[0].assets[0].submetered_by")
}

// A→B→A: both ends of the cycle are reported, not just whichever one the walk
// happened to start from.
func TestValidateRejectsASubmeterCycle(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].SubmeteredBy = "asset-b"
	env.Zones[0].Assets = append(env.Zones[0].Assets, Asset{
		Id: "asset-b", Name: "Zähler B", Kind: AssetMeter,
		ExternalTypeId: "urn:infai:ses:device-type:abc",
		SubmeteredBy:   "asset-meter",
	})
	err := Validate(env)
	assertHasPath(t, err, "zones[0].assets[0].submetered_by")
	assertHasPath(t, err, "zones[0].assets[1].submetered_by")
}

// The mirrored graph is one location tree per site; a submeter edge leaving
// its top level zone would point that tree at a device belonging to another.
func TestValidateRejectsASubmeterReferenceAcrossSites(t *testing.T) {
	env := validEnvironment()
	env.Zones = append(env.Zones, Zone{
		Id: "zone-other", Name: "Halle 2", Type: ZoneHall,
		Assets: []Asset{{
			Id: "asset-other", Name: "Zähler 2", Kind: AssetMeter,
			ExternalTypeId: "urn:infai:ses:device-type:abc",
		}},
	})
	env.Zones[0].Assets[0].SubmeteredBy = "asset-other"
	assertHasPath(t, Validate(env), "zones[0].assets[0].submetered_by")
}

// The counter-proof: a reference across sub-zones of the same top level zone
// is fine. Only crossing into a different top level zone is refused.
func TestValidateAllowsASubmeterReferenceAcrossSubZonesOfTheSameSite(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Zones = []Zone{{
		Id: "zone-sub", Name: "Nebenraum", Type: ZoneRoom,
		Assets: []Asset{{
			Id: "asset-sub", Name: "Unterzähler", Kind: AssetMeter,
			ExternalTypeId: "urn:infai:ses:device-type:abc",
			SubmeteredBy:   "asset-meter",
		}},
	}}
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

// A submetered asset needs no platform device of its own: Validate never
// looks at external_ref, only the graph mirror falls back to the zone for
// this case (lib/graphs/build_test.go).
func TestValidateAllowsASubmeteredAssetWithoutADevice(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets = append(env.Zones[0].Assets, Asset{
		Id: "asset-rest", Name: "Rest", Kind: AssetMeter,
		ExternalTypeId: "urn:infai:ses:device-type:abc",
		SubmeteredBy:   "asset-meter",
	})
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}
