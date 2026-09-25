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

package effects

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// testdata/musterwerke.json is the demonstrator document of moses-demonstrator
// (musterwerke/environment.json), copied verbatim. Every expectation below was
// read off its scripts by hand.

func musterwerke(t *testing.T) domain.Environment {
	t.Helper()
	raw, err := os.ReadFile("testdata/musterwerke.json")
	if err != nil {
		t.Fatal(err)
	}
	env := domain.Environment{}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func edgesInto(g Graph, to string, via Via) []Edge {
	result := []Edge{}
	for _, edge := range g.Edges {
		if edge.To == to && edge.Via == via {
			result = append(result, edge)
		}
	}
	return result
}

func reads(from, to, channel, key string, count int) Edge {
	return Edge{From: from, To: to, Kind: EdgeReads, Via: ViaScript, Channel: channel, Key: key, Count: count}
}

func TestMusterwerkeScriptsResolveCompletely(t *testing.T) {
	g := Derive(musterwerke(t))
	if len(g.Unresolved) != 0 {
		t.Fatalf("expected every reference of the demonstrator to resolve, got %+v", g.Unresolved)
	}
	accesses, writes := 0, 0
	for _, edge := range g.Edges {
		if edge.Via == ViaScript {
			accesses += edge.Count
			if edge.Kind == EdgeWrites {
				writes++
			}
		}
	}
	//counted independently over the 56 scripts: 135 context reads, 8 literal
	//getDevice reads including the two through K, 17 + 10 rows of the two main
	//meter tables and 4 through the compressor's id list
	if accesses != 174 {
		t.Errorf("expected 174 script accesses outside the asset's own state, got %d", accesses)
	}
	//every script writes its own asset state only
	if writes != 0 {
		t.Errorf("expected no script to write foreign state, got %d edges", writes)
	}
	t.Logf("script accesses resolved: %d, unresolved: %d", accesses, len(g.Unresolved))
}

func TestMusterwerkeBatteryReadsThePvAndTheMainMeter(t *testing.T) {
	g := Derive(musterwerke(t))
	expectEdges(t, edgesInto(g, "asset:a-speicher-a", ViaScript),
		reads("asset:a-pv-a", "asset:a-speicher-a", "ch-a-bat-soc", "pv_w", 1),
		reads("asset:a-rlm-a", "asset:a-speicher-a", "ch-a-bat-soc", "p_load_w", 1),
	)
}

func TestMusterwerkeMainMetersReadTheirSubMetersThroughTheTable(t *testing.T) {
	g := Derive(musterwerke(t))
	ch := "ch-a-rlm-a-180"
	expectEdges(t, edgesInto(g, "asset:a-rlm-a", ViaScript),
		reads("asset:a-baz-1", "asset:a-rlm-a", ch, "energy_kwh", 1),
		reads("asset:a-baz-2", "asset:a-rlm-a", ch, "energy_kwh", 1),
		reads("asset:a-baz-3", "asset:a-rlm-a", ch, "energy_kwh", 1),
		reads("asset:a-buerotrakt", "asset:a-rlm-a", ch, "energy_kwh", 1),
		reads("asset:a-drehmaschine-1", "asset:a-rlm-a", ch, "energy_kwh", 1),
		reads("asset:a-drehmaschine-2", "asset:a-rlm-a", ch, "energy_kwh", 1),
		reads("asset:a-grundlast-rest-a", "asset:a-rlm-a", ch, "energy_kwh", 1),
		reads("asset:a-hallenbeleuchtung", "asset:a-rlm-a", ch, "energy_kwh", 1),
		reads("asset:a-kompressor-11", "asset:a-rlm-a", ch, "energy_kwh", 1),
		reads("asset:a-kompressor-22", "asset:a-rlm-a", ch, "energy_kwh", 1),
		//the cumulative profile keeps its reading under its channel id
		reads("asset:a-kss-zentrale", "asset:a-rlm-a", ch, "ch-a-kss-kwh", 1),
		reads("asset:a-lueftung-absaugung", "asset:a-rlm-a", ch, "energy_kwh", 1),
		reads("asset:a-pv-a", "asset:a-rlm-a", ch, "pv_kwh", 1),
		reads("asset:a-saegen-kleinmaschinen", "asset:a-rlm-a", ch, "energy_kwh", 1),
		reads("asset:a-speicher-a", "asset:a-rlm-a", ch, "e_charged_kwh", 1),
		reads("asset:a-speicher-a", "asset:a-rlm-a", ch, "e_discharged_kwh", 1),
		reads("asset:a-speicher-eigenbedarf", "asset:a-rlm-a", ch, "energy_kwh", 1),
	)
	if len(edgesInto(g, "asset:b-rlm-b", ViaScript)) != 10 {
		t.Errorf("expected the ten rows of the second main meter, got %+v", edgesInto(g, "asset:b-rlm-b", ViaScript))
	}
	//the power channel sums the sub meters by name in a formula
	if len(edgesInto(g, "asset:a-rlm-a", ViaFormula)) != 9 {
		t.Errorf("expected the nine channel inputs of the power formula, got %+v", edgesInto(g, "asset:a-rlm-a", ViaFormula))
	}
}

func TestMusterwerkeGasHeatingReadsTheWeatherAndTheCalendar(t *testing.T) {
	g := Derive(musterwerke(t))
	ch := "ch-a-gas-h-m3"
	expectEdges(t, edgesInto(g, "asset:a-gas-heizung", ViaScript),
		reads("context:f_office", "asset:a-gas-heizung", ch, "f_office", 1),
		reads("context:holiday", "asset:a-gas-heizung", ch, "holiday", 2),
		reads("context:presence", "asset:a-gas-heizung", ch, "presence", 1),
		reads("context:shift", "asset:a-gas-heizung", ch, "shift", 1),
		reads("context:temperature", "asset:a-gas-heizung", ch, "temperature", 1),
	)
}

// The CHP writes boiler_w, demand_w and the rest into its own asset state, which
// is no edge; the edges appear where other scripts read them.
func TestMusterwerkeChpStateIsReadByTheMetersAroundIt(t *testing.T) {
	g := Derive(musterwerke(t))
	out := []Edge{}
	for _, edge := range g.Edges {
		if edge.From == "asset:b-bhkw" {
			out = append(out, edge)
		}
	}
	expectEdges(t, out,
		reads("asset:b-bhkw", "asset:b-gas-bhkw", "ch-b-gas-b-m3", "gas_m3h", 1),
		reads("asset:b-bhkw", "asset:b-gas-heizung", "ch-b-gas-h-m3", "boiler_w", 1),
		Edge{From: "asset:b-bhkw", To: "asset:b-rlm-b", Kind: EdgeReads, Via: ViaFormula, Channel: "ch-b-rlm-b-p", Key: "ch-b-bhkw-p", Count: 1},
		reads("asset:b-bhkw", "asset:b-rlm-b", "ch-b-rlm-b-180", "el_kwh", 1),
		reads("asset:b-bhkw", "asset:b-wz-bhkw", "ch-b-wz-b-w", "p_th_w", 1),
		reads("asset:b-bhkw", "asset:b-wz-verwaltung", "ch-b-wz-v-w", "demand_w", 1),
	)
	expectEdges(t, edgesInto(g, "asset:b-bhkw", ViaScript),
		reads("context:holiday", "asset:b-bhkw", "ch-b-bhkw-p", "holiday", 1),
		reads("context:price", "asset:b-bhkw", "ch-b-bhkw-p", "price", 1),
		reads("context:shift", "asset:b-bhkw", "ch-b-bhkw-p", "shift", 1),
		reads("context:temperature", "asset:b-bhkw", "ch-b-bhkw-p", "temperature", 1),
	)
}

func TestMusterwerkeCompressorsReadTheMachinesAndEachOther(t *testing.T) {
	g := Derive(musterwerke(t))
	ch := "ch-a-k22-w"
	expectEdges(t, edgesInto(g, "asset:a-kompressor-22", ViaScript),
		reads("asset:a-baz-1", "asset:a-kompressor-22", ch, "air_demand", 1),
		reads("asset:a-baz-2", "asset:a-kompressor-22", ch, "air_demand", 1),
		reads("asset:a-baz-3", "asset:a-kompressor-22", ch, "air_demand", 1),
		reads("asset:a-drehmaschine-1", "asset:a-kompressor-22", ch, "air_demand", 1),
		reads("context:air_leak_m3h", "asset:a-kompressor-22", ch, "air_leak_m3h", 1),
		reads("context:f_prod", "asset:a-kompressor-22", ch, "f_prod", 1),
		reads("context:holiday", "asset:a-kompressor-22", ch, "holiday", 1),
		reads("context:maintenance", "asset:a-kompressor-22", ch, "maintenance", 1),
		reads("context:test_run", "asset:a-kompressor-22", ch, "test_run", 1),
	)
	expectEdges(t, edgesInto(g, "asset:a-kompressor-11", ViaScript),
		reads("asset:a-kompressor-22", "asset:a-kompressor-11", "ch-a-k11-w", "p_reserve_w", 1),
		reads("asset:a-kompressor-22", "asset:a-kompressor-11", "ch-a-k11-w", "reserve_kwh", 1),
	)
}

func TestMusterwerkeSchedulesTimelineAndSubmetering(t *testing.T) {
	g := Derive(musterwerke(t))
	expectEdges(t, edgesVia(g, ViaSchedule),
		Edge{From: "context:after_shift", To: "asset:b-staplerladestationen", Kind: EdgeGates, Via: ViaSchedule, Channel: "ch-b-st-w", Key: "after_shift", Count: 1},
		Edge{From: "context:day_type", To: "asset:b-prueffeld", Kind: EdgeScales, Via: ViaSchedule, Channel: "ch-b-pf-w", Key: "day_type", Count: 1},
		Edge{From: "context:shift", To: "asset:b-prueffeld", Kind: EdgeGates, Via: ViaSchedule, Channel: "ch-b-pf-w", Key: "shift", Count: 1},
	)
	counts := map[string]int{}
	for _, edge := range edgesVia(g, ViaTimeline) {
		counts[edge.Key] = edge.Count
	}
	expectedCounts := map[string]int{
		"context.break_factor": 476, "context.presence": 359, "context.day_type": 208, "context.week_factor": 52,
		"context.holiday": 18, "channel.ch-b-pf-w.schedule.gate.threshold": 18, "channel.ch-b-st-w.schedule.gate.threshold": 18,
		"channel.ch-a-kss-w.profile.base": 18, "channel.ch-a-kss-kwh.profile.base": 18, "context.maintenance": 4,
		"context.air_leak_m3h": 1, "context.light_base_a_w": 1,
	}
	if !reflect.DeepEqual(counts, expectedCounts) {
		t.Errorf("expected the dated changes per target %v, got %v", expectedCounts, counts)
	}
	fertigung := []Edge{}
	for _, edge := range edgesInto(g, "asset:a-uv-fertigung", ViaAggregate) {
		fertigung = append(fertigung, edge)
	}
	//seven machines, each summed once into the watt and once into the kWh total
	if len(fertigung) != 14 {
		t.Errorf("expected 14 aggregate edges into the production distribution, got %+v", fertigung)
	}
	if len(edgesInto(g, "asset:a-uv-fertigung", ViaSubmeteredBy)) != 7 {
		t.Errorf("expected seven sub meters of the production distribution, got %+v", edgesInto(g, "asset:a-uv-fertigung", ViaSubmeteredBy))
	}
	price, _ := nodeById(g, "context:price")
	//declared in context with a start value and driven by an export: not static
	if *price.Static || *price.SourceKind != "dataset" || *price.ExternalRef != "price-export-id-placeholder" {
		t.Errorf("unexpected price node %+v", price)
	}
	for _, node := range g.Nodes {
		if node.Kind == NodeZone {
			t.Errorf("expected no zone node, no script reads zone state: %+v", node)
		}
	}
}

func TestMusterwerkeDerivesTheSameBytesEveryTime(t *testing.T) {
	env := musterwerke(t)
	first, err := json.Marshal(Derive(env))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := json.Marshal(Derive(env))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("derivation %d differs from the first", i)
		}
	}
}
