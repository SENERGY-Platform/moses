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

package graphs

import (
	"sort"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// fixtureAsset is an asset of the Musterwerke fixture: a device named after
// its id, and the parents written as "id" or "id!" for a conversion edge.
func fixtureAsset(id string, name string, kind domain.AssetKind, parents ...string) domain.Asset {
	return domain.Asset{
		Id: id, Name: name, Kind: kind,
		ExternalTypeId: "urn:infai:ses:device-type:meter",
		ExternalRef:    "urn:device:" + id,
		MeterParents:   fixtureParents(parents...),
	}
}

func fixtureParents(parents ...string) []domain.MeterParent {
	result := []domain.MeterParent{}
	for _, parent := range parents {
		if parent[len(parent)-1] == '!' {
			result = append(result, domain.MeterParent{Id: parent[:len(parent)-1], Conversion: true})
		} else {
			result = append(result, domain.MeterParent{Id: parent})
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func submeteredBy(asset domain.Asset, target string) domain.Asset {
	asset.SubmeteredBy = target
	return asset
}

// musterwerke is the planned meter model of the Musterwerke: two sites, each
// with electricity, gas, heat and water, meter groups for the jointly supplied
// outgoing feeders and heating circuits, and conversion edges where gas becomes
// heat or electricity. The UV meters also carry a submetered_by to their site's
// RLM meter, as the location graph's distribution does.
func musterwerke() domain.Environment {
	meter, inverter, machine := domain.AssetMeter, domain.AssetInverter, domain.AssetMachine
	return domain.Environment{
		Id: "env-musterwerke", Name: "Musterwerke", Owner: "user-a", Type: domain.IndustrialSite,
		Zones: []domain.Zone{{
			Id: "zone-werk-a", Name: "Werk A", Type: domain.ZoneSite,
			Assets: []domain.Asset{
				fixtureAsset("a-rlm", "RLM-Hauptzähler A", meter),
				fixtureAsset("a-pv", "PV-Wechselrichter", inverter),
				fixtureAsset("a-batterie", "Batteriespeicher", inverter, "a-rlm", "a-pv"),
				fixtureAsset("a-bhkw-strom", "Erzeugungszähler Biogas-BHKW A", meter, "a-biogas!"),
				submeteredBy(fixtureAsset("a-uv-fertigung", "UV Fertigung", meter, "g-abgaenge-a"), "a-rlm"),
				fixtureAsset("a-druckluft", "Druckluftstation", meter, "g-abgaenge-a"),
				fixtureAsset("a-hallentechnik", "Hallentechnik", meter, "g-abgaenge-a"),
				fixtureAsset("a-buero", "Bürotrakt", meter, "g-abgaenge-a"),
				fixtureAsset("a-grundlast", "Grundlast Rest", meter, "g-abgaenge-a"),
				fixtureAsset("a-speicher-eigen", "Speicher Eigenbedarf", meter, "g-abgaenge-a"),
				fixtureAsset("a-biogas-eigen", "Eigenstrom Biogasanlage", meter, "g-abgaenge-a"),
				fixtureAsset("a-gas-ha", "Gaszähler Hauptanschluss A", meter),
				fixtureAsset("a-gas-prozess", "Gasunterzähler Prozess A", meter, "a-gas-ha"),
				fixtureAsset("a-biogas", "Biogaszähler BHKW", meter),
				fixtureAsset("a-fackel", "Fackel-Gaszähler", meter),
				fixtureAsset("a-waerme-bhkw", "Wärmezähler Biogas-BHKW", meter, "a-biogas!"),
				fixtureAsset("a-waerme-fermenter", "Wärmezähler Fermenterheizung", meter, "a-waerme-bhkw"),
				fixtureAsset("a-waerme-halle", "Wärmezähler Fertigungshalle", meter, "g-heizkreise-a"),
				fixtureAsset("a-waerme-buero", "Wärmezähler Bürotrakt", meter, "g-heizkreise-a"),
				fixtureAsset("a-wasser-ha", "Wasserzähler Hauptanschluss A", meter),
				fixtureAsset("a-kaltwasser", "Kaltwasserzähler Sanitär", meter, "a-wasser-ha"),
				fixtureAsset("a-kss-abzug", "Abzugszähler KSS", meter, "a-wasser-ha"),
				fixtureAsset("a-warmwasser", "Warmwasserzähler A", meter, "a-wasser-ha", "g-heizkreise-a!"),
			},
			Zones: []domain.Zone{{
				Id: "zone-a-fertigung", Name: "Fertigungshalle", Type: domain.ZoneHall,
				Assets: []domain.Asset{
					submeteredBy(fixtureAsset("a-baz-1", "BAZ 1", machine), "a-uv-fertigung"),
					submeteredBy(fixtureAsset("a-baz-2", "BAZ 2", machine), "a-uv-fertigung"),
					submeteredBy(fixtureAsset("a-baz-3", "BAZ 3", machine), "a-uv-fertigung"),
					submeteredBy(fixtureAsset("a-dreh-1", "Drehmaschine 1", machine), "a-uv-fertigung"),
					submeteredBy(fixtureAsset("a-dreh-2", "Drehmaschine 2", machine), "a-uv-fertigung"),
					submeteredBy(fixtureAsset("a-kss", "KSS-Zentrale", machine), "a-uv-fertigung"),
					submeteredBy(fixtureAsset("a-saegen", "Sägen", machine), "a-uv-fertigung"),
					submeteredBy(fixtureAsset("a-kompressor-22", "Kompressor 22 kW", meter), "a-druckluft"),
					submeteredBy(fixtureAsset("a-kompressor-11", "Kompressor 11 kW", meter), "a-druckluft"),
					submeteredBy(fixtureAsset("a-hallenlicht", "Hallenbeleuchtung A", meter), "a-hallentechnik"),
					submeteredBy(fixtureAsset("a-lueftung", "Lüftung/Absaugung", meter), "a-hallentechnik"),
				},
			}},
		}, {
			Id: "zone-werk-b", Name: "Werk B", Type: domain.ZoneSite,
			Assets: []domain.Asset{
				fixtureAsset("b-rlm", "RLM-Hauptzähler B", meter),
				fixtureAsset("b-bhkw-strom", "Erzeugungszähler BHKW B", meter, "b-gas-bhkw!"),
				fixtureAsset("b-bhkw-heiz", "BHKW Heizzentrale", inverter, "b-bhkw-strom"),
				submeteredBy(fixtureAsset("b-uv-montage", "UV Montagehalle", meter, "g-abgaenge-b"), "b-rlm"),
				submeteredBy(fixtureAsset("b-uv-lager", "UV Lager", meter, "g-abgaenge-b"), "b-rlm"),
				fixtureAsset("b-verwaltung", "Verwaltungsgebäude", meter, "g-abgaenge-b"),
				fixtureAsset("b-druckluft", "Druckluft und Technik", meter, "g-abgaenge-b"),
				submeteredBy(fixtureAsset("b-prueffeld", "Prüffeld", meter), "b-uv-montage"),
				submeteredBy(fixtureAsset("b-montageplaetze", "Montagearbeitsplätze", meter), "b-uv-montage"),
				submeteredBy(fixtureAsset("b-hallenlicht", "Hallenbeleuchtung B", meter), "b-uv-montage"),
				submeteredBy(fixtureAsset("b-stapler", "Stapler", meter), "b-uv-lager"),
				submeteredBy(fixtureAsset("b-lagerlicht", "Lagerbeleuchtung", meter), "b-uv-lager"),
				submeteredBy(fixtureAsset("b-serverraum", "Serverraum", machine), "b-verwaltung"),
				submeteredBy(fixtureAsset("b-buero-it", "Büro-IT", machine), "b-verwaltung"),
				submeteredBy(fixtureAsset("b-kantine", "Kantine", machine), "b-verwaltung"),
				fixtureAsset("b-gas-ha", "Gaszähler Hauptanschluss B", meter),
				fixtureAsset("b-gas-bhkw", "Gasunterzähler BHKW B", meter, "b-gas-ha"),
				fixtureAsset("b-waerme-bhkw", "Wärmezähler BHKW B", meter, "b-gas-bhkw!"),
				fixtureAsset("b-waerme-sammler", "Wärmezähler Sammler", meter, "b-gas-ha!", "b-waerme-bhkw"),
				fixtureAsset("b-waerme-montage", "Wärmezähler Montagehalle", meter, "b-waerme-sammler"),
				fixtureAsset("b-waerme-lager", "Wärmezähler Lager", meter, "b-waerme-sammler"),
				fixtureAsset("b-waerme-verwaltung", "Wärmezähler Verwaltung", meter, "b-waerme-sammler"),
				fixtureAsset("b-wasser-ha", "Wasserzähler Hauptanschluss B", meter),
				fixtureAsset("b-warmwasser", "Warmwasserzähler Heizzentrale", meter, "b-wasser-ha", "b-waerme-sammler!"),
			},
		}},
		MeterGroups: []domain.MeterGroup{
			{Id: "g-abgaenge-a", Name: "Abgänge A", Parents: fixtureParents("a-rlm", "a-bhkw-strom", "a-pv", "a-batterie")},
			{Id: "g-heizkreise-a", Name: "Heizkreise A", Parents: fixtureParents("a-gas-ha!", "a-waerme-bhkw")},
			{Id: "g-abgaenge-b", Name: "Abgänge B", Parents: fixtureParents("b-rlm", "b-bhkw-strom")},
		},
	}
}

type expectedEdge struct {
	from, to   string
	weight     int
	conversion bool
}

// musterwerkeEdges is the meter graph the Musterwerke model asks for, written
// by asset and group id; "root" is the root node.
func musterwerkeEdges() []expectedEdge {
	return []expectedEdge{
		{"a-rlm", "root", 100, false},
		{"a-pv", "root", 100, false},
		{"a-batterie", "a-rlm", 50, false},
		{"a-batterie", "a-pv", 50, false},
		{"a-bhkw-strom", "a-biogas", 100, true},
		{"g-abgaenge-a", "a-rlm", 25, false},
		{"g-abgaenge-a", "a-bhkw-strom", 25, false},
		{"g-abgaenge-a", "a-pv", 25, false},
		{"g-abgaenge-a", "a-batterie", 25, false},
		{"a-uv-fertigung", "g-abgaenge-a", 100, false},
		{"a-druckluft", "g-abgaenge-a", 100, false},
		{"a-hallentechnik", "g-abgaenge-a", 100, false},
		{"a-buero", "g-abgaenge-a", 100, false},
		{"a-grundlast", "g-abgaenge-a", 100, false},
		{"a-speicher-eigen", "g-abgaenge-a", 100, false},
		{"a-biogas-eigen", "g-abgaenge-a", 100, false},
		{"a-baz-1", "a-uv-fertigung", 100, false},
		{"a-baz-2", "a-uv-fertigung", 100, false},
		{"a-baz-3", "a-uv-fertigung", 100, false},
		{"a-dreh-1", "a-uv-fertigung", 100, false},
		{"a-dreh-2", "a-uv-fertigung", 100, false},
		{"a-kss", "a-uv-fertigung", 100, false},
		{"a-saegen", "a-uv-fertigung", 100, false},
		{"a-kompressor-22", "a-druckluft", 100, false},
		{"a-kompressor-11", "a-druckluft", 100, false},
		{"a-hallenlicht", "a-hallentechnik", 100, false},
		{"a-lueftung", "a-hallentechnik", 100, false},
		{"a-gas-ha", "root", 100, false},
		{"a-gas-prozess", "a-gas-ha", 100, false},
		{"a-biogas", "root", 100, false},
		{"a-fackel", "root", 100, false},
		{"a-waerme-bhkw", "a-biogas", 100, true},
		{"a-waerme-fermenter", "a-waerme-bhkw", 100, false},
		{"g-heizkreise-a", "a-gas-ha", 50, true},
		{"g-heizkreise-a", "a-waerme-bhkw", 50, false},
		{"a-waerme-halle", "g-heizkreise-a", 100, false},
		{"a-waerme-buero", "g-heizkreise-a", 100, false},
		{"a-wasser-ha", "root", 100, false},
		{"a-kaltwasser", "a-wasser-ha", 100, false},
		{"a-kss-abzug", "a-wasser-ha", 100, false},
		{"a-warmwasser", "a-wasser-ha", 50, false},
		{"a-warmwasser", "g-heizkreise-a", 50, true},
		{"b-rlm", "root", 100, false},
		{"b-bhkw-strom", "b-gas-bhkw", 100, true},
		{"b-bhkw-heiz", "b-bhkw-strom", 100, false},
		{"g-abgaenge-b", "b-rlm", 50, false},
		{"g-abgaenge-b", "b-bhkw-strom", 50, false},
		{"b-uv-montage", "g-abgaenge-b", 100, false},
		{"b-uv-lager", "g-abgaenge-b", 100, false},
		{"b-verwaltung", "g-abgaenge-b", 100, false},
		{"b-druckluft", "g-abgaenge-b", 100, false},
		{"b-prueffeld", "b-uv-montage", 100, false},
		{"b-montageplaetze", "b-uv-montage", 100, false},
		{"b-hallenlicht", "b-uv-montage", 100, false},
		{"b-stapler", "b-uv-lager", 100, false},
		{"b-lagerlicht", "b-uv-lager", 100, false},
		{"b-serverraum", "b-verwaltung", 100, false},
		{"b-buero-it", "b-verwaltung", 100, false},
		{"b-kantine", "b-verwaltung", 100, false},
		{"b-gas-ha", "root", 100, false},
		{"b-gas-bhkw", "b-gas-ha", 100, false},
		{"b-waerme-bhkw", "b-gas-bhkw", 100, true},
		{"b-waerme-sammler", "b-gas-ha", 50, true},
		{"b-waerme-sammler", "b-waerme-bhkw", 50, false},
		{"b-waerme-montage", "b-waerme-sammler", 100, false},
		{"b-waerme-lager", "b-waerme-sammler", 100, false},
		{"b-waerme-verwaltung", "b-waerme-sammler", 100, false},
		{"b-wasser-ha", "root", 100, false},
		{"b-warmwasser", "b-wasser-ha", 50, false},
		{"b-warmwasser", "b-waerme-sammler", 50, true},
	}
}

// fixtureNodeId maps an id of the fixture onto its node: an asset onto its
// device, a group and the root onto themselves.
func fixtureNodeId(id string) string {
	if id == RootNodeId || id[:2] == "g-" {
		return id
	}
	return "urn:device:" + id
}

func edgeKey(edge expectedEdge) string {
	return edge.from + " -> " + edge.to
}

// The Musterwerke model is the case the meter graph exists for, so its whole
// edge set is pinned: every child, every parent, every weight and every
// conversion.
func TestTheMeterGraphOfTheMusterwerke(t *testing.T) {
	env := musterwerke()
	if err := domain.Validate(env); err != nil {
		t.Fatalf("the Musterwerke model has to be a valid document: %v", err)
	}
	graph := BuildMeterGraph(env)
	if err := graph.Valid(); err != nil {
		t.Fatalf("the repository would refuse the Musterwerke meter graph: %v", err)
	}
	if graph.ContainsLoop() {
		t.Fatal("the Musterwerke meter graph contains a loop")
	}

	want := map[string]expectedEdge{}
	for _, edge := range musterwerkeEdges() {
		edge.from, edge.to = fixtureNodeId(edge.from), fixtureNodeId(edge.to)
		want[edgeKey(edge)] = edge
	}
	got := map[string]expectedEdge{}
	for _, edge := range graph.Edges {
		mapped := expectedEdge{from: edge.FromNodeId, to: edge.ToNodeId, weight: edge.Weight,
			conversion: attribute(edge.Attributes, ConversionAttribute) == "true"}
		got[edgeKey(mapped)] = mapped
	}
	if len(graph.Edges) != len(got) {
		t.Errorf("an edge appears twice: %d edges, %d distinct", len(graph.Edges), len(got))
	}
	keys := []string{}
	for key := range want {
		keys = append(keys, key)
	}
	for key := range got {
		if _, expected := want[key]; !expected {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if want[key] != got[key] {
			t.Errorf("%s: expected %+v, got %+v", key, want[key], got[key])
		}
	}
	//every asset of the fixture has a device and takes part, plus the groups and the root
	assets := 0
	domain.WalkAssets(env, func(string, domain.Asset) { assets++ })
	if expected := assets + len(env.MeterGroups) + 1; len(graph.Nodes) != expected {
		t.Errorf("expected %d nodes, got %d", expected, len(graph.Nodes))
	}
}

// meter_parents overrides submetered_by in the meter graph only: the location
// graph still hangs a UV meter under its RLM meter.
func TestTheMusterwerkeLocationGraphStillFollowsSubmeteredBy(t *testing.T) {
	graph := Build(musterwerke())

	for _, uv := range []struct{ uv, rlm string }{
		{"a-uv-fertigung", "a-rlm"}, {"b-uv-montage", "b-rlm"}, {"b-uv-lager", "b-rlm"},
	} {
		edge, ok := edgeFrom(graph, fixtureNodeId(uv.uv))
		if !ok || edge.ToNodeId != fixtureNodeId(uv.rlm) {
			t.Errorf("%s: expected the location edge to the RLM meter, got %+v", uv.uv, edge)
		}
	}
	meters := BuildMeterGraph(musterwerke())
	edge, _ := edgeFrom(meters, fixtureNodeId("a-uv-fertigung"))
	if edge.ToNodeId != "g-abgaenge-a" {
		t.Errorf("expected meter_parents to place the UV meter under its group, got %q", edge.ToNodeId)
	}
}

// A group of site B supplied by the feeder group of site A would put B's
// meters under A's grid connection; validation refuses the link.
func TestTheMusterwerkeRefuseAGroupLinkAcrossSites(t *testing.T) {
	env := musterwerke()
	env.MeterGroups = append(env.MeterGroups, domain.MeterGroup{Id: "g-b-sub", Name: "Unterverteilung B", Parents: fixtureParents("g-abgaenge-a")})
	assetById(&env, "b-prueffeld").MeterParents = fixtureParents("g-b-sub")

	err := domain.Validate(env)
	if err == nil || !strings.Contains(err.Error(), "meter_groups[3].parents[0]") {
		t.Fatalf("expected the link to the other site's group to be refused, got %v", err)
	}
}
