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

package runtime

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/effects"
)

// lib/effects duplicates the indexing and indexAggregates because it must not
// import the runtime; this pins the copy to the original on the shapes the
// runtime treats specially.
func TestTheEffectGraphIndexesAndSumsWhatTheRuntimeDoes(t *testing.T) {
	raw, err := os.ReadFile("../effects/testdata/musterwerke.json")
	if err != nil {
		t.Fatal(err)
	}
	musterwerke := domain.Environment{}
	if err := json.Unmarshal(raw, &musterwerke); err != nil {
		t.Fatal(err)
	}
	const envId = "env-effects-parity"
	nested := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic),
			aggregateChannel("ch-none", serviceOf(envId, "none"), 1, "  "),
		}},
		treeAsset{id: "a-mid", submeteredBy: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-mid", serviceOf(envId, "mid"), 1, " "+energyCharacteristic+" "),
		}},
		treeAsset{id: "a-leaf", submeteredBy: "a-mid", channels: []domain.Channel{
			measuringChannel("ch-leaf-1", serviceOf(envId, "leaf-1"), energyCharacteristic, 1),
			measuringChannel("ch-leaf-2", serviceOf(envId, "leaf-2"), energyCharacteristic+" ", 2),
			measuringChannel("", serviceOf(envId, "leaf-3"), energyCharacteristic, 3),
		}},
		treeAsset{id: "a-self", submeteredBy: "a-self", channels: []domain.Channel{
			aggregateChannel("ch-self", serviceOf(envId, "self"), 1, energyCharacteristic),
			measuringChannel("ch-self-own", serviceOf(envId, "self-own"), energyCharacteristic, 4),
		}},
		treeAsset{id: "a-total", submeteredBy: "a-mid", channels: []domain.Channel{
			measuringChannel("ch-duplicate-asset", serviceOf(envId, "dup"), energyCharacteristic, 5),
		}},
		treeAsset{id: "a-dup-channel", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total-2"), 1, otherCharacteristic),
		}},
	)
	nested.Zones = append(nested.Zones, domain.Zone{Name: "no id", Assets: []domain.Asset{{
		Id: "a-hidden", SubmeteredBy: "a-total",
		Channels: []domain.Channel{measuringChannel("ch-hidden", serviceOf(envId, "hidden"), energyCharacteristic, 6)},
	}}})

	for name, env := range map[string]domain.Environment{"musterwerke": musterwerke, "nested": nested, "zones": zoneTree()} {
		gen := newGeneration(env, nil)
		//which assets run, and in which zone, is what the whole graph is built on
		derivedAssets := map[string]string{}
		derivedZones := map[string]bool{}
		for _, node := range effects.Derive(env).Nodes {
			if node.Kind == effects.NodeAsset {
				derivedAssets[strings.TrimPrefix(node.Id, "asset:")] = *node.Zone
				derivedZones[*node.Zone] = true
			}
		}
		runtimeAssets := map[string]string{}
		for id, info := range gen.assets {
			runtimeAssets[id] = info.zoneId
		}
		if !reflect.DeepEqual(runtimeAssets, derivedAssets) {
			t.Errorf("%s: the runtime runs the assets %v, the effect graph shows %v", name, runtimeAssets, derivedAssets)
		}
		for id := range gen.zones {
			if !derivedZones[id] && name == "zones" {
				t.Errorf("%s: the runtime indexes zone %s, the effect graph places no asset in it", name, id)
			}
		}

		runtimeInputs := gen.aggregateInputs
		derived := effects.AggregateInputs(env)
		if len(runtimeInputs) == 0 {
			t.Fatalf("%s: expected the fixture to carry aggregates", name)
		}
		if len(runtimeInputs) != len(derived) {
			t.Errorf("%s: the runtime indexes %d aggregates, the effect graph %d", name, len(runtimeInputs), len(derived))
		}
		for channel, inputs := range runtimeInputs {
			got, ok := derived[channel]
			//nil and empty are the same answer: the aggregate sums nothing
			if !ok || (len(inputs) != 0 || len(got) != 0) && !reflect.DeepEqual(inputs, got) {
				t.Errorf("%s: aggregate %s sums %v in the runtime, %v in the effect graph", name, channel, inputs, got)
			}
		}
	}
}

// zoneTree has an asset in every zone, so that the zones the runtime indexes are
// visible through the assets: nesting below the depth limit, a duplicate zone id
// whose second subtree is dropped, a zone without an id and a duplicate asset.
func zoneTree() domain.Environment {
	leaf := func(zone string) domain.Asset {
		return domain.Asset{Id: "a-" + zone, SubmeteredBy: "a-site", Channels: []domain.Channel{
			measuringChannel("ch-"+zone, "", energyCharacteristic, 1)}}
	}
	deep := domain.Zone{Id: "deep-" + strconv.Itoa(domain.MaxZoneDepth+1), Assets: []domain.Asset{leaf("too-deep")}}
	for depth := domain.MaxZoneDepth; depth >= 2; depth-- {
		id := "deep-" + strconv.Itoa(depth)
		deep = domain.Zone{Id: id, Assets: []domain.Asset{leaf(id)}, Zones: []domain.Zone{deep}}
	}
	return domain.Environment{Id: "env-zones", Zones: []domain.Zone{
		{Id: "site", Assets: []domain.Asset{
			{Id: "a-site", Channels: []domain.Channel{aggregateChannel("ch-total", "", 1, energyCharacteristic)}},
		}, Zones: []domain.Zone{
			deep,
			{Id: "hall", Assets: []domain.Asset{leaf("hall"), leaf("hall")}},
			{Id: "hall", Assets: []domain.Asset{leaf("hall-copy")}, Zones: []domain.Zone{{Id: "below-copy", Assets: []domain.Asset{leaf("below-copy")}}}},
			{Name: "no id", Assets: []domain.Asset{leaf("no-id")}},
		}},
		{Id: "site-2", Assets: []domain.Asset{leaf("site-2")}},
	}}
}
