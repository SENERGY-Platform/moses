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
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

func channelOf(id string, source domain.Source) domain.Channel {
	return domain.Channel{Id: id, Direction: domain.Sensor, Source: source}
}

func sensorWith(id string, characteristic string) domain.Channel {
	return domain.Channel{Id: id, Direction: domain.Sensor, CharacteristicId: characteristic,
		Source: domain.Source{Kind: domain.SourceProfile, Profile: &domain.ProfileSource{Base: 1}}}
}

func aggregateWith(id string, characteristic string) domain.Channel {
	return domain.Channel{Id: id, Direction: domain.Sensor, CharacteristicId: characteristic,
		Source: domain.Source{Kind: domain.SourceAggregate}}
}

func edgesVia(g Graph, via Via) []Edge {
	result := []Edge{}
	for _, edge := range g.Edges {
		if edge.Via == via {
			result = append(result, edge)
		}
	}
	return result
}

func expectEdges(t *testing.T, actual []Edge, expected ...Edge) {
	t.Helper()
	if len(expected) == 0 {
		expected = []Edge{}
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("expected edges\n%+v\ngot\n%+v", expected, actual)
	}
}

func nodeById(g Graph, id string) (Node, bool) {
	for _, node := range g.Nodes {
		if node.Id == id {
			return node, true
		}
	}
	return Node{}, false
}

func formulaSite(inputs map[string]string) domain.Environment {
	return domain.Environment{
		Context: map[string]interface{}{"price": 0.3},
		Zones: []domain.Zone{{Id: "z", Name: "Halle", Assets: []domain.Asset{
			{Id: "m", Name: "Zähler", Channels: []domain.Channel{channelOf("ch-m", domain.Source{
				Kind: domain.SourceFormula, Formula: &domain.FormulaSource{Expression: "1", Inputs: inputs},
			}), sensorWith("ch-m-own", "")}},
			{Id: "p", Name: "Anlage", Channels: []domain.Channel{sensorWith("ch-p", "")}},
		}}},
	}
}

// The runtime reads zone.<k> from the asset's own zone and asset.<k> from the
// asset itself (resolveInput); neither prefix names another zone or asset.
func TestFormulaInputsAreEdgesAsTheRuntimeResolvesThem(t *testing.T) {
	g := Derive(formulaSite(map[string]string{
		"c": "context.price", "c2": "context.price", "z": "zone.temp", "a": "asset.p.energy_kwh",
		"x": "channel.ch-p", "o": "channel.ch-m-own",
	}))
	expectEdges(t, edgesVia(g, ViaFormula),
		Edge{From: "asset:p", To: "asset:m", Kind: EdgeReads, Via: ViaFormula, Channel: "ch-m", Key: "ch-p", Count: 1},
		Edge{From: "context:price", To: "asset:m", Kind: EdgeReads, Via: ViaFormula, Channel: "ch-m", Key: "price", Count: 2},
		Edge{From: "zone:z", To: "asset:m", Kind: EdgeReads, Via: ViaFormula, Channel: "ch-m", Key: "temp", Count: 1},
	)
	if len(g.Unresolved) != 0 {
		t.Errorf("expected nothing unresolved, got %+v", g.Unresolved)
	}
}

func TestAFormulaInputNamingNothingKnownIsUnresolved(t *testing.T) {
	g := Derive(formulaSite(map[string]string{"a": "channel.ghost", "b": "weather.temp"}))
	expectEdges(t, edgesVia(g, ViaFormula))
	expected := []Unresolved{
		{Asset: "m", Channel: "ch-m", Expression: "channel.ghost", Reason: "the formula reads a channel the document does not carry"},
		{Asset: "m", Channel: "ch-m", Expression: "weather.temp", Reason: "the formula input names no context, zone, asset or channel"},
	}
	if !reflect.DeepEqual(g.Unresolved, expected) {
		t.Errorf("expected %+v, got %+v", expected, g.Unresolved)
	}
}

func TestAScheduleGatesAndScalesOnContextKeys(t *testing.T) {
	env := domain.Environment{
		ContextSources: map[string]domain.Source{"shift": {Kind: domain.SourceProfile, Profile: &domain.ProfileSource{}}},
		Zones: []domain.Zone{{Id: "z", Assets: []domain.Asset{{Id: "st", Channels: []domain.Channel{channelOf("ch-st", domain.Source{
			Kind: domain.SourceSchedule, Schedule: &domain.ScheduleSource{
				StateKey: "programme", Gate: &domain.ScheduleGate{ContextKey: "shift"}, ScaleBy: "day_type",
				States: []domain.ScheduleState{{Name: "run", DurationSeconds: 60}},
			},
		})}}}}},
	}
	g := Derive(env)
	expectEdges(t, g.Edges,
		Edge{From: "context:day_type", To: "asset:st", Kind: EdgeScales, Via: ViaSchedule, Channel: "ch-st", Key: "day_type", Count: 1},
		Edge{From: "context:shift", To: "asset:st", Kind: EdgeGates, Via: ViaSchedule, Channel: "ch-st", Key: "shift", Count: 1},
	)
	//day_type is declared nowhere: it still gets a node, marked as neither static nor driven
	node, ok := nodeById(g, "context:day_type")
	if !ok || *node.Static || *node.SourceKind != "" || *node.ExternalRef != "" {
		t.Errorf("expected an undeclared context node, got %+v", node)
	}
}

func submeterSite() domain.Environment {
	return domain.Environment{Zones: []domain.Zone{{Id: "site", Assets: []domain.Asset{
		{Id: "main", Channels: []domain.Channel{aggregateWith("ch-main-w", "watt"), aggregateWith("ch-main-kwh", " kwh ")}},
		{Id: "sub-1", SubmeteredBy: "main", Channels: []domain.Channel{sensorWith("ch-1-w", "watt "), sensorWith("ch-1-kwh", "kwh"), sensorWith("ch-1-v", "volt")}},
		{Id: "self-ref", SubmeteredBy: "self-ref", Channels: []domain.Channel{sensorWith("ch-s-w", "watt")}},
		{Id: "lost", SubmeteredBy: "ghost"},
	}, Zones: []domain.Zone{{Id: "hall", Assets: []domain.Asset{
		{Id: "sub-2", SubmeteredBy: "main", Channels: []domain.Channel{sensorWith("ch-2-w", "watt"), sensorWith("ch-2-w-b", "watt")}},
	}}}}}}
}

func TestSubmeteringAndAggregatesAreEdgesFromTheChildToTheMeter(t *testing.T) {
	g := Derive(submeterSite())
	expectEdges(t, edgesVia(g, ViaSubmeteredBy),
		Edge{From: "asset:sub-1", To: "asset:main", Kind: EdgeSubmeters, Via: ViaSubmeteredBy, Count: 1},
		Edge{From: "asset:sub-2", To: "asset:main", Kind: EdgeSubmeters, Via: ViaSubmeteredBy, Count: 1},
	)
	//characteristics compare trimmed, and two matching channels of one child count twice
	expectEdges(t, edgesVia(g, ViaAggregate),
		Edge{From: "asset:sub-1", To: "asset:main", Kind: EdgeAggregates, Via: ViaAggregate, Channel: "ch-main-kwh", Count: 1},
		Edge{From: "asset:sub-1", To: "asset:main", Kind: EdgeAggregates, Via: ViaAggregate, Channel: "ch-main-w", Count: 1},
		Edge{From: "asset:sub-2", To: "asset:main", Kind: EdgeAggregates, Via: ViaAggregate, Channel: "ch-main-w", Count: 2},
	)
	expected := []Unresolved{{Asset: "lost", Expression: "ghost", Reason: "submetered_by names no asset of the document"}}
	if !reflect.DeepEqual(g.Unresolved, expected) {
		t.Errorf("expected %+v, got %+v", expected, g.Unresolved)
	}
	inputs := AggregateInputs(submeterSite())
	if !reflect.DeepEqual(inputs, map[string][]string{"ch-main-w": {"ch-1-w", "ch-2-w", "ch-2-w-b"}, "ch-main-kwh": {"ch-1-kwh"}}) {
		t.Errorf("unexpected aggregate inputs %v", inputs)
	}
}

func TestTheTimelineIsOneEdgePerTargetCountingItsChanges(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := domain.Environment{
		Context:        map[string]interface{}{"holiday": 0.0},
		ContextSources: map[string]domain.Source{"temp": {Kind: domain.SourceDataset, Dataset: &domain.DatasetSource{Ref: "export-1"}}},
		Timeline: []domain.DatedChange{
			{At: at, Target: "context.holiday", Value: 1},
			{At: at.Add(time.Hour), Target: "context.holiday", Value: 0},
			{At: at, Target: "context_source.temp.dataset.scale", Value: 2},
			{At: at, Target: "channel.ch-a.profile.base", Value: 3},
			{At: at, Target: "channel.ch-a.profile.spread_percent", Value: 3},
			{At: at, Target: "channel.ghost.profile.base", Value: 3},
			{At: at, Target: "weather.temp", Value: 3},
		},
		Zones: []domain.Zone{{Id: "z", Assets: []domain.Asset{{Id: "a", Channels: []domain.Channel{sensorWith("ch-a", "")}}}}},
	}
	g := Derive(env)
	expectEdges(t, g.Edges,
		Edge{From: "timeline", To: "asset:a", Kind: EdgeDatedChange, Via: ViaTimeline, Channel: "ch-a", Key: "channel.ch-a.profile.base", Count: 1},
		Edge{From: "timeline", To: "asset:a", Kind: EdgeDatedChange, Via: ViaTimeline, Channel: "ch-a", Key: "channel.ch-a.profile.spread_percent", Count: 1},
		Edge{From: "timeline", To: "context:holiday", Kind: EdgeDatedChange, Via: ViaTimeline, Key: "context.holiday", Count: 2},
		Edge{From: "timeline", To: "context:temp", Kind: EdgeDatedChange, Via: ViaTimeline, Key: "context_source.temp.dataset.scale", Count: 1},
	)
	expected := []Unresolved{
		{Channel: "ghost", Expression: "channel.ghost.profile.base", Reason: "the timeline targets a channel the document does not carry"},
		{Expression: "weather.temp", Reason: "the timeline target is none of the forms docs/dated-changes.md lists"},
	}
	if !reflect.DeepEqual(g.Unresolved, expected) {
		t.Errorf("expected %+v, got %+v", expected, g.Unresolved)
	}
	if node, ok := nodeById(g, "timeline"); !ok || node.Label != "Dated changes" {
		t.Errorf("expected the timeline node, got %+v", node)
	}
	if _, ok := nodeById(Derive(domain.Environment{}), "timeline"); ok {
		t.Error("expected no timeline node for a document without a timeline")
	}
}

func TestContextNodesSayWhatDrivesTheKey(t *testing.T) {
	env := domain.Environment{
		Context: map[string]interface{}{"shift": 0.0, "price": 0.3},
		ContextSources: map[string]domain.Source{
			"price": {Kind: domain.SourceDataset, Dataset: &domain.DatasetSource{Ref: "export-price"}},
			"sun":   {Kind: domain.SourceProfile, Profile: &domain.ProfileSource{}},
			"odd":   {Kind: domain.SourceScript, Script: &domain.ScriptSource{}},
		},
	}
	g := Derive(env)
	for _, expected := range []struct {
		id                      string
		static                  bool
		sourceKind, externalRef string
	}{
		{"context:odd", false, "", ""},
		{"context:price", false, "dataset", "export-price"},
		{"context:shift", true, "", ""},
		{"context:sun", false, "profile", ""},
	} {
		node, ok := nodeById(g, expected.id)
		if !ok || node.Kind != NodeContextKey || node.Label != strings.TrimPrefix(expected.id, "context:") ||
			*node.Static != expected.static || *node.SourceKind != expected.sourceKind || *node.ExternalRef != expected.externalRef {
			t.Errorf("%s: expected %+v, got %+v", expected.id, expected, node)
		}
	}
	if len(g.Nodes) != 4 {
		t.Errorf("expected four context nodes and nothing else, got %+v", g.Nodes)
	}
}

// The runtime ignores a zone without an id with everything below it, a
// duplicate asset id and zones below the depth limit; so does the graph.
func TestTheDocumentIsIndexedTheWayTheRuntimeIndexesIt(t *testing.T) {
	deep := domain.Zone{Id: "deep-9", Assets: []domain.Asset{{Id: "too-deep"}}}
	for depth := domain.MaxZoneDepth; depth >= 2; depth-- {
		deep = domain.Zone{Id: "deep-" + string(rune('0'+depth)), Zones: []domain.Zone{deep}}
	}
	env := domain.Environment{Zones: []domain.Zone{
		{Id: "site", Name: "Standort", Assets: []domain.Asset{{Id: "a", Name: "Erst", Kind: domain.AssetMeter}, {Id: "a", Name: "Zweit"}, {Name: "ohne id"}},
			Zones: []domain.Zone{{Id: "hall", Assets: []domain.Asset{{Id: "h", Name: "Halle", Kind: domain.AssetMachine}}}, deep}},
		{Name: "no id", Assets: []domain.Asset{{Id: "hidden"}}},
	}}
	g := Derive(env)
	ids := []string{}
	for _, node := range g.Nodes {
		ids = append(ids, node.Id)
	}
	if !reflect.DeepEqual(ids, []string{"asset:a", "asset:h"}) {
		t.Fatalf("expected exactly the assets the runtime runs, got %v", ids)
	}
	a, _ := nodeById(g, "asset:a")
	h, _ := nodeById(g, "asset:h")
	if a.Label != "Erst" || *a.Zone != "site" || *a.Site != "site" || *a.AssetKind != "meter" {
		t.Errorf("unexpected asset node %+v", a)
	}
	if *h.Zone != "hall" || *h.Site != "site" || *h.AssetKind != "machine" {
		t.Errorf("unexpected asset node %+v", h)
	}
}

func TestEachNodeKindCarriesItsOwnFieldsInTheJson(t *testing.T) {
	env := scriptSite("moses.zone.state.get('t')")
	env.Timeline = []domain.DatedChange{{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Target: "context.shift", Value: 1}}
	raw, err := json.Marshal(Derive(env))
	if err != nil {
		t.Fatal(err)
	}
	decoded := struct {
		Nodes []map[string]interface{} `json:"nodes"`
		Edges []map[string]interface{} `json:"edges"`
	}{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	keys := map[string][]string{}
	for _, node := range decoded.Nodes {
		names := []string{}
		for name := range node {
			names = append(names, name)
		}
		keys[node["kind"].(string)] = sortedStrings(names)
	}
	expected := map[string][]string{
		"context_key": {"external_ref", "id", "kind", "label", "source_kind", "static"},
		"asset":       {"asset_kind", "id", "kind", "label", "site", "zone"},
		"zone":        {"id", "kind", "label", "site", "zone"},
		"timeline":    {"id", "kind", "label"},
	}
	if !reflect.DeepEqual(keys, expected) {
		t.Errorf("expected the fields %v, got %v", expected, keys)
	}
	for _, edge := range decoded.Edges {
		names := []string{}
		for name := range edge {
			names = append(names, name)
		}
		if !reflect.DeepEqual(sortedStrings(names), []string{"channel", "count", "from", "key", "kind", "to", "via"}) {
			t.Errorf("expected every edge field to be present, got %v", edge)
		}
	}

	empty, err := json.Marshal(Derive(domain.Environment{}))
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{"nodes":[],"edges":[],"unresolved":[]}` {
		t.Errorf("expected empty lists rather than null, got %s", empty)
	}
}

func sortedStrings(values []string) []string {
	result := append([]string{}, values...)
	sort.Strings(result)
	return result
}

func TestNodesAndEdgesAreSorted(t *testing.T) {
	g := Derive(submeterSite())
	for i := 1; i < len(g.Nodes); i++ {
		if g.Nodes[i-1].Id >= g.Nodes[i].Id {
			t.Errorf("nodes out of order at %d: %s, %s", i, g.Nodes[i-1].Id, g.Nodes[i].Id)
		}
	}
	for i := 1; i < len(g.Edges); i++ {
		if !edgeLess(g.Edges[i-1], g.Edges[i]) {
			t.Errorf("edges out of order at %d: %+v, %+v", i, g.Edges[i-1], g.Edges[i])
		}
	}
}
