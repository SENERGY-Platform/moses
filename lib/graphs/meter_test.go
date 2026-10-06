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
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
)

// meterSite is one hall with a main meter, two sub-meters and a sensor; every
// asset has a device named after its id.
func meterSite() domain.Environment {
	return domain.Environment{
		Id: "env-1", Name: "Metallbau Musterstadt", Owner: "user-a", Type: domain.IndustrialSite,
		Zones: []domain.Zone{{
			Id: "zone-halle", Name: "Halle 1", Type: domain.ZoneHall,
			Assets: []domain.Asset{
				fixtureAsset("haupt", "Hauptzähler", domain.AssetMeter),
				fixtureAsset("pv", "PV", domain.AssetInverter),
				fixtureAsset("unter-1", "Unterzähler 1", domain.AssetMeter),
				fixtureAsset("unter-2", "Unterzähler 2", domain.AssetMeter),
				fixtureAsset("temperatur", "Hallentemperatur", domain.AssetSensor),
			},
		}},
	}
}

// assetById returns a pointer into the document, so a test can edit one asset.
func assetById(env *domain.Environment, id string) *domain.Asset {
	var found *domain.Asset
	var walk func(zones []domain.Zone)
	walk = func(zones []domain.Zone) {
		for i := range zones {
			for j := range zones[i].Assets {
				if zones[i].Assets[j].Id == id {
					found = &zones[i].Assets[j]
				}
			}
			walk(zones[i].Zones)
		}
	}
	walk(env.Zones)
	if found == nil {
		panic("no asset " + id)
	}
	return found
}

func device(id string) string {
	return "urn:device:" + id
}

// outgoing is the edges leaving one node, by target, in graph order.
func outgoing(graph models.Graph, from string) []models.Edge {
	result := []models.Edge{}
	for _, edge := range graph.Edges {
		if edge.FromNodeId == from {
			result = append(result, edge)
		}
	}
	return result
}

// assertParents checks the edges leaving from, in order, as target and weight.
func assertParents(t *testing.T, graph models.Graph, from string, want ...interface{}) {
	t.Helper()
	got := outgoing(graph, from)
	if len(got) != len(want)/2 {
		t.Fatalf("%s: expected %d edges, got %+v", from, len(want)/2, got)
	}
	for i, edge := range got {
		if edge.ToNodeId != want[2*i].(string) || edge.Weight != want[2*i+1].(int) {
			t.Errorf("%s: edge %d, expected %v with %v, got %s with %d", from, i, want[2*i], want[2*i+1], edge.ToNodeId, edge.Weight)
		}
	}
}

func assertAccepted(t *testing.T, graph models.Graph) {
	t.Helper()
	if err := graph.Valid(); err != nil {
		t.Fatalf("the repository would refuse this graph: %v", err)
	}
	if graph.ContainsLoop() {
		t.Fatal("the graph contains a loop")
	}
}

// The four conventions of the location graph hold for the meter graph too, and
// the graph says which of the two mirrors it is.
func TestTheMeterGraphFollowsTheFourConventions(t *testing.T) {
	env := meterSite()
	env.ExternalMeterGraphRef = "urn:infai:ses:graph:meters"
	env.ExternalGraphRef = "urn:infai:ses:graph:location"
	assetById(&env, "unter-1").MeterParents = fixtureParents("haupt")

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)

	if graph.Id != "urn:infai:ses:graph:meters" {
		t.Errorf("expected the meter graph ref as the id, got %q", graph.Id)
	}
	if graph.Owner != "user-a" {
		t.Errorf("the mirror belongs to the owner of the environment, got %q", graph.Owner)
	}
	if got := attribute(graph.Attributes, EnvironmentAttribute); got != "env-1" {
		t.Errorf("expected the environment id under %q, got %q", EnvironmentAttribute, got)
	}
	if got := attribute(graph.Attributes, GraphAttribute); got != MeterGraph {
		t.Errorf("expected %q under %q, got %q", MeterGraph, GraphAttribute, got)
	}
	root, ok := nodeById(graph, RootNodeId)
	if !ok {
		t.Fatalf("no node with the id %q", RootNodeId)
	}
	if name := attribute(root.Attributes, NameAttribute); name != "Metallbau Musterstadt (meters)" {
		t.Errorf("expected the environment name with the suffix on the root, got %q", name)
	}
	if len(outgoing(graph, RootNodeId)) != 0 {
		t.Error("the root must have no outgoing edge")
	}
	//child to parent
	edge, ok := edgeFrom(graph, device("unter-1"))
	if !ok || edge.ToNodeId != device("haupt") || edge.Id != device("unter-1")+"->"+device("haupt") {
		t.Errorf("expected an edge from the sub-meter to the main meter, got %+v", edge)
	}
	node, ok := nodeById(graph, device("unter-1"))
	if !ok || node.ResourceId != node.Id || node.ResourceType != models.GraphResourceTypeDevice {
		t.Errorf("a device node carries the device id as id and resource id, got %+v", node)
	}
	if name := attribute(node.Attributes, NameAttribute); name != "Unterzähler 1" {
		t.Errorf("expected the asset name on the device node, got %q", name)
	}
}

// Without meter_parents an asset's submetered_by is its one parent, carrying
// the whole flow.
func TestSubmeteredByIsTheMeterParentWhereMeterParentsIsEmpty(t *testing.T) {
	env := meterSite()
	assetById(&env, "unter-1").SubmeteredBy = "haupt"

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	assertParents(t, graph, device("unter-1"), device("haupt"), 100)
}

// meter_parents wins over submetered_by in the meter graph.
func TestMeterParentsOverrideSubmeteredBy(t *testing.T) {
	env := meterSite()
	assetById(&env, "unter-2").SubmeteredBy = "haupt"
	assetById(&env, "unter-2").MeterParents = fixtureParents("unter-1")

	graph := BuildMeterGraph(env)
	assertParents(t, graph, device("unter-2"), device("unter-1"), 100)
}

func TestExplicitWeightsAreTakenAsGiven(t *testing.T) {
	env := meterSite()
	assetById(&env, "unter-1").MeterParents = []domain.MeterParent{{Id: "haupt", Weight: 70}, {Id: "pv", Weight: 30}}

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	assertParents(t, graph, device("unter-1"), device("haupt"), 70, device("pv"), 30)
}

// An equal split has to sum to exactly 100 in integers: the remainder goes one
// point each to the first parents in list order.
func TestOmittedWeightsSplitEquallyWithTheRemainderFirst(t *testing.T) {
	env := meterSite()
	assetById(&env, "temperatur").Kind = domain.AssetMeter
	assetById(&env, "unter-2").MeterParents = fixtureParents("haupt", "pv", "unter-1")

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	assertParents(t, graph, device("unter-2"), device("haupt"), 34, device("pv"), 33, device("unter-1"), 33)

	assetById(&env, "unter-2").MeterParents = nil
	assetById(&env, "temperatur").MeterParents = fixtureParents("haupt", "pv", "unter-1", "unter-2")
	graph = BuildMeterGraph(env)
	assertAccepted(t, graph)
	assertParents(t, graph, device("temperatur"), device("haupt"), 25, device("pv"), 25, device("unter-1"), 25, device("unter-2"), 25)
}

func TestTheConversionAttributeIsSetOnlyWhereTheMediumChanges(t *testing.T) {
	env := meterSite()
	assetById(&env, "unter-1").MeterParents = fixtureParents("haupt!", "pv")

	graph := BuildMeterGraph(env)
	edges := outgoing(graph, device("unter-1"))
	if len(edges) != 2 {
		t.Fatalf("expected two edges, got %+v", edges)
	}
	want := []models.Attribute{{Key: ConversionAttribute, Value: "true"}}
	if !reflect.DeepEqual(edges[0].Attributes, want) {
		t.Errorf("expected the conversion attribute on the converting edge, got %+v", edges[0].Attributes)
	}
	if len(edges[1].Attributes) != 0 {
		t.Errorf("an edge without conversion carries no attributes, got %+v", edges[1].Attributes)
	}
	for _, edge := range outgoing(graph, device("haupt")) {
		if len(edge.Attributes) != 0 {
			t.Errorf("a root edge carries no attributes, got %+v", edge.Attributes)
		}
	}
}

// A group is a node of its own between its members and its parents, named by
// the group, without a resource.
func TestAMeterGroupStandsBetweenItsMembersAndItsParents(t *testing.T) {
	env := meterSite()
	env.MeterGroups = []domain.MeterGroup{{Id: "abgaenge", Name: "Abgänge", Parents: fixtureParents("haupt", "pv")}}
	assetById(&env, "unter-1").MeterParents = fixtureParents("abgaenge")
	assetById(&env, "unter-2").MeterParents = fixtureParents("abgaenge")

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	node, ok := nodeById(graph, "abgaenge")
	if !ok {
		t.Fatalf("no node for the group, got %+v", graph.Nodes)
	}
	if node.ResourceId != "" || node.ResourceType != "" || attribute(node.Attributes, NameAttribute) != "Abgänge" {
		t.Errorf("a group node is named by the group and has no resource, got %+v", node)
	}
	assertParents(t, graph, "abgaenge", device("haupt"), 50, device("pv"), 50)
	assertParents(t, graph, device("unter-1"), "abgaenge", 100)
	assertParents(t, graph, device("unter-2"), "abgaenge", 100)
}

// A group nobody references and nothing supplies is still a node, at the root.
func TestAMeterGroupWithoutParentsHangsAtTheRoot(t *testing.T) {
	env := meterSite()
	env.MeterGroups = []domain.MeterGroup{{Id: "leer", Name: "Leer"}}

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	assertParents(t, graph, "leer", RootNodeId, 100)
}

// Dropping a single parent would break the weight sum, so a node with any
// parent that cannot be resolved hangs at the root with the whole flow.
func TestAnUnresolvableParentSendsTheWholeNodeToTheRoot(t *testing.T) {
	for name, edit := range map[string]func(env *domain.Environment){
		"missing id": func(env *domain.Environment) {
			assetById(env, "unter-1").MeterParents = fixtureParents("haupt", "gibt-es-nicht")
		},
		"empty id": func(env *domain.Environment) {
			assetById(env, "unter-1").MeterParents = []domain.MeterParent{{Id: "haupt"}, {}}
		},
		"zone id": func(env *domain.Environment) {
			assetById(env, "unter-1").MeterParents = fixtureParents("haupt", "zone-halle")
		},
		"asset without a device": func(env *domain.Environment) {
			assetById(env, "pv").ExternalRef = ""
			assetById(env, "unter-1").MeterParents = fixtureParents("haupt", "pv")
		},
		"asset on the child's own device": func(env *domain.Environment) {
			assetById(env, "pv").ExternalRef = device("unter-1")
			assetById(env, "unter-1").MeterParents = fixtureParents("haupt", "pv")
		},
		"device id instead of an asset id": func(env *domain.Environment) {
			assetById(env, "unter-1").MeterParents = fixtureParents("haupt", device("pv"))
		},
		"group whose id a device holds": func(env *domain.Environment) {
			env.MeterGroups = []domain.MeterGroup{{Id: device("pv"), Name: "Kollision", Parents: fixtureParents("haupt")}}
			assetById(env, "unter-1").MeterParents = fixtureParents("haupt", device("pv"))
		},
		"weights not summing to 100": func(env *domain.Environment) {
			assetById(env, "unter-1").MeterParents = []domain.MeterParent{{Id: "haupt", Weight: 60}, {Id: "pv", Weight: 30}}
		},
		"weights partly omitted": func(env *domain.Environment) {
			assetById(env, "unter-1").MeterParents = []domain.MeterParent{{Id: "haupt", Weight: 100}, {Id: "pv"}}
		},
		"weight out of range": func(env *domain.Environment) {
			assetById(env, "unter-1").MeterParents = []domain.MeterParent{{Id: "haupt", Weight: 101}, {Id: "pv", Weight: -1}}
		},
		"itself": func(env *domain.Environment) {
			assetById(env, "unter-1").MeterParents = fixtureParents("haupt", "unter-1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := meterSite()
			edit(&env)
			graph := BuildMeterGraph(env)
			assertAccepted(t, graph)
			assertParents(t, graph, device("unter-1"), RootNodeId, 100)
		})
	}
}

// The same for a group: one unresolvable parent sends it to the root, and its
// members still hang at it.
func TestAGroupWithAnUnresolvableParentHangsAtTheRoot(t *testing.T) {
	env := meterSite()
	env.MeterGroups = []domain.MeterGroup{{Id: "abgaenge", Name: "Abgänge", Parents: fixtureParents("haupt", "gibt-es-nicht")}}
	assetById(&env, "unter-1").MeterParents = fixtureParents("abgaenge")

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	assertParents(t, graph, "abgaenge", RootNodeId, 100)
	assertParents(t, graph, device("unter-1"), "abgaenge", 100)
}

// More than 100 parents cannot be split equally into weights of at least 1.
func TestMoreThanAHundredParentsSendTheNodeToTheRoot(t *testing.T) {
	env := meterSite()
	parents := []string{}
	for i := 0; i < 101; i++ {
		id := "teil-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		env.Zones[0].Assets = append(env.Zones[0].Assets, fixtureAsset(id, id, domain.AssetMeter))
		parents = append(parents, id)
	}
	assetById(&env, "unter-1").MeterParents = fixtureParents(parents...)

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	assertParents(t, graph, device("unter-1"), RootNodeId, 100)
}

// Two assets publishing through one device are one node with one set of
// parents. Carriers stating different parents are not ranked, so the device
// hangs at the root; a carrier stating nothing does not object.
func TestCarriersOfOneDeviceHaveToAgree(t *testing.T) {
	t.Run("different parents", func(t *testing.T) {
		env := meterSite()
		assetById(&env, "unter-2").ExternalRef = device("unter-1")
		assetById(&env, "unter-1").MeterParents = fixtureParents("haupt")
		assetById(&env, "unter-2").MeterParents = fixtureParents("pv")

		graph := BuildMeterGraph(env)
		assertAccepted(t, graph)
		assertParents(t, graph, device("unter-1"), RootNodeId, 100)
	})
	t.Run("different weights", func(t *testing.T) {
		env := meterSite()
		assetById(&env, "unter-2").ExternalRef = device("unter-1")
		assetById(&env, "unter-1").MeterParents = fixtureParents("haupt", "pv")
		assetById(&env, "unter-2").MeterParents = []domain.MeterParent{{Id: "haupt", Weight: 70}, {Id: "pv", Weight: 30}}

		graph := BuildMeterGraph(env)
		assertParents(t, graph, device("unter-1"), RootNodeId, 100)
	})
	t.Run("one states nothing", func(t *testing.T) {
		env := meterSite()
		assetById(&env, "unter-2").ExternalRef = device("unter-1")
		assetById(&env, "unter-2").MeterParents = fixtureParents("haupt")

		graph := BuildMeterGraph(env)
		assertAccepted(t, graph)
		assertParents(t, graph, device("unter-1"), device("haupt"), 100)
	})
	t.Run("the same parents written differently", func(t *testing.T) {
		env := meterSite()
		assetById(&env, "unter-2").ExternalRef = device("unter-1")
		assetById(&env, "unter-1").SubmeteredBy = "haupt"
		assetById(&env, "unter-2").MeterParents = fixtureParents("haupt")

		graph := BuildMeterGraph(env)
		assertParents(t, graph, device("unter-1"), device("haupt"), 100)
	})
}

// Two parents that are carried by one device would be two edges between the
// same pair of nodes, which the repository refuses.
func TestTwoParentsOnOneDeviceSendTheNodeToTheRoot(t *testing.T) {
	env := meterSite()
	assetById(&env, "pv").ExternalRef = device("haupt")
	assetById(&env, "unter-1").MeterParents = fixtureParents("haupt", "pv")

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	assertParents(t, graph, device("unter-1"), RootNodeId, 100)
}

// meterCycle is acyclic per asset - unter-1 is metered by unter-2, which is
// metered by temperatur - and still a cycle of devices, because temperatur
// publishes through the device of unter-1.
func meterCycle() domain.Environment {
	env := meterSite()
	assetById(&env, "unter-1").MeterParents = fixtureParents("unter-2")
	assetById(&env, "unter-2").MeterParents = fixtureParents("haupt", "temperatur")
	assetById(&env, "temperatur").ExternalRef = device("unter-1")
	return env
}

// The repository refuses a graph with a loop, so of each device cycle the node
// earliest in document order goes to the root, and nothing else changes.
func TestADeviceCycleSendsItsFirstNodeToTheRoot(t *testing.T) {
	graph := BuildMeterGraph(meterCycle())

	assertAccepted(t, graph)
	assertParents(t, graph, device("unter-1"), RootNodeId, 100)
	assertParents(t, graph, device("unter-2"), device("haupt"), 50, device("unter-1"), 50)
	if again := BuildMeterGraph(meterCycle()); !reflect.DeepEqual(graph, again) {
		t.Fatalf("breaking the cycle has to pick the same node every time:\n%+v\n%+v", graph, again)
	}
}

// Where the walk enters a cycle does not decide: the member earliest in
// document order loses its parents, devices counting before groups, even when
// it sits in the middle of the walk. Validation refuses this document; the
// build has to survive it all the same.
func TestTheEarliestMemberOfACycleIsBrokenWhereverTheWalkEntersIt(t *testing.T) {
	env := meterSite()
	env.MeterGroups = []domain.MeterGroup{{Id: "abgaenge", Name: "Abgänge", Parents: fixtureParents("unter-2")}}
	//haupt leads into unter-2 -> unter-1 -> abgaenge -> unter-2
	assetById(&env, "haupt").MeterParents = fixtureParents("unter-2")
	assetById(&env, "unter-2").MeterParents = fixtureParents("unter-1")
	assetById(&env, "unter-1").MeterParents = fixtureParents("abgaenge")

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	assertParents(t, graph, device("unter-1"), RootNodeId, 100)
	assertParents(t, graph, device("unter-2"), device("unter-1"), 100)
	assertParents(t, graph, "abgaenge", device("unter-2"), 100)
	assertParents(t, graph, device("haupt"), device("unter-2"), 100)
}

// Two cycles reached in one walk are both broken, each at its own earliest
// member.
func TestEveryCycleIsBroken(t *testing.T) {
	env := meterCycle()
	env.Zones = append(env.Zones, domain.Zone{
		Id: "zone-b", Name: "Halle 2", Type: domain.ZoneHall,
		Assets: []domain.Asset{
			fixtureAsset("x", "X", domain.AssetMeter, "y"),
			fixtureAsset("y", "Y", domain.AssetMeter, "z"),
			fixtureAsset("z", "Z", domain.AssetMeter),
		},
	})
	assetById(&env, "z").ExternalRef = device("x")
	//the walk from haupt runs into the second cycle before it reaches the first
	assetById(&env, "haupt").MeterParents = fixtureParents("y")

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	assertParents(t, graph, device("unter-1"), RootNodeId, 100)
	assertParents(t, graph, device("x"), RootNodeId, 100)
	assertParents(t, graph, device("y"), device("x"), 100)
	assertParents(t, graph, device("haupt"), device("y"), 100)
}

// A sensor, an actuator or a machine that takes part in no edge is not part of
// a meter graph; a meter or an inverter is, at the root if nothing supplies it.
func TestOnlyMetersInvertersAndDevicesOnAnEdgeAreNodes(t *testing.T) {
	env := meterSite()
	env.Zones[0].Assets = append(env.Zones[0].Assets,
		fixtureAsset("maschine", "Maschine", domain.AssetMachine),
		fixtureAsset("stellglied", "Stellglied", domain.AssetActuator),
		submeteredBy(fixtureAsset("maschine-2", "Maschine 2", domain.AssetMachine), "unter-1"),
		fixtureAsset("sensor-2", "Sensor 2", domain.AssetSensor),
		//a device for nothing but the root has no place
		fixtureAsset("ohne-geraet", "Ohne Gerät", domain.AssetMeter),
	)
	assetById(&env, "ohne-geraet").ExternalRef = ""
	assetById(&env, "unter-2").MeterParents = fixtureParents("sensor-2")

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	for _, absent := range []string{"temperatur", "maschine", "stellglied", "ohne-geraet"} {
		if _, ok := nodeById(graph, device(absent)); ok {
			t.Errorf("%s takes part in no edge and must not be a node", absent)
		}
	}
	if _, ok := nodeById(graph, ""); ok {
		t.Error("an asset without a device must not become a node")
	}
	for _, root := range []string{"haupt", "pv", "unter-1"} {
		assertParents(t, graph, device(root), RootNodeId, 100)
	}
	assertParents(t, graph, device("maschine-2"), device("unter-1"), 100)
	//a sensor somebody names as parent is a node, at the root
	assertParents(t, graph, device("sensor-2"), RootNodeId, 100)
	assertParents(t, graph, device("unter-2"), device("sensor-2"), 100)
}

// A device whose carriers state meter parents was meant to be in the meter
// graph, whatever its kind: where the statement cannot be followed it hangs at
// the root like any other node.
func TestADeviceStatingParentsThatCannotBeFollowedHangsAtTheRoot(t *testing.T) {
	for name, edit := range map[string]func(env *domain.Environment){
		"unknown parent": func(env *domain.Environment) {
			assetById(env, "temperatur").MeterParents = fixtureParents("gibt-es-nicht")
		},
		"parent on its own device": func(env *domain.Environment) {
			env.Zones[0].Assets = append(env.Zones[0].Assets, fixtureAsset("fuehler-2", "Fühler 2", domain.AssetSensor))
			assetById(env, "fuehler-2").ExternalRef = device("temperatur")
			assetById(env, "temperatur").MeterParents = fixtureParents("fuehler-2")
		},
		// two machines on one device, each naming another parent
		"conflicting carriers": func(env *domain.Environment) {
			env.Zones[0].Assets = append(env.Zones[0].Assets, fixtureAsset("maschine-2", "Maschine 2", domain.AssetMachine))
			assetById(env, "temperatur").Kind = domain.AssetMachine
			assetById(env, "maschine-2").ExternalRef = device("temperatur")
			assetById(env, "temperatur").MeterParents = fixtureParents("haupt")
			assetById(env, "maschine-2").MeterParents = fixtureParents("pv")
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := meterSite()
			edit(&env)
			graph := BuildMeterGraph(env)
			assertAccepted(t, graph)
			if _, ok := nodeById(graph, device("temperatur")); !ok {
				t.Fatal("a device whose carriers state parents has to be a node")
			}
			assertParents(t, graph, device("temperatur"), RootNodeId, 100)
		})
	}
}

// Ids the document cannot keep from colliding with the root or each other
// cost the colliding node, never the graph.
func TestCollidingIdsCostTheNodeNotTheGraph(t *testing.T) {
	env := meterSite()
	assetById(&env, "pv").ExternalRef = RootNodeId
	env.MeterGroups = []domain.MeterGroup{
		{Id: RootNodeId, Name: "Wurzel", Parents: fixtureParents("haupt")},
		{Id: "abgaenge", Name: "Abgänge", Parents: fixtureParents("haupt")},
		{Id: "abgaenge", Name: "Doppelt", Parents: fixtureParents("unter-2")},
		{Id: "", Name: "Ohne Id", Parents: fixtureParents("haupt")},
	}
	assetById(&env, "unter-1").MeterParents = fixtureParents("abgaenge")

	graph := BuildMeterGraph(env)
	assertAccepted(t, graph)
	root, _ := nodeById(graph, RootNodeId)
	if attribute(root.Attributes, NameAttribute) != "Metallbau Musterstadt (meters)" {
		t.Errorf("the root must stay the root, got %+v", root)
	}
	group, _ := nodeById(graph, "abgaenge")
	if attribute(group.Attributes, NameAttribute) != "Abgänge" {
		t.Errorf("the first group keeps the id, got %+v", group)
	}
	assertParents(t, graph, "abgaenge", device("haupt"), 100)
	assertParents(t, graph, device("unter-1"), "abgaenge", 100)
}

// The same document has to give the same graph, edge order included: the
// repository stores what it is sent, and a reordering would read as a change.
func TestBuildingTheMeterGraphTwiceProducesTheSameGraph(t *testing.T) {
	first := BuildMeterGraph(musterwerke())
	for i := 0; i < 20; i++ {
		if again := BuildMeterGraph(musterwerke()); !reflect.DeepEqual(first, again) {
			t.Fatalf("two builds of one document differ:\n%+v\n%+v", first, again)
		}
	}
}

// meter_parents and meter_groups are not part of the location graph: it is
// built exactly as before for a document that carries them.
func TestTheLocationGraphIgnoresMeterParentsAndGroups(t *testing.T) {
	for name, env := range map[string]domain.Environment{
		"musterwerke": musterwerke(),
		"meter cycle": meterCycle(),
	} {
		stripped := env
		stripped.MeterGroups = nil
		stripped.ExternalMeterGraphRef = ""
		stripped.Zones = stripMeterParents(env.Zones)
		env.ExternalMeterGraphRef = "urn:infai:ses:graph:meters"
		if with, without := Build(env), Build(stripped); !reflect.DeepEqual(with, without) {
			t.Errorf("%s: the location graph changed with the meter model:\n%+v\n%+v", name, with, without)
		}
	}
}

func stripMeterParents(zones []domain.Zone) []domain.Zone {
	result := []domain.Zone{}
	for _, zone := range zones {
		copied := zone
		copied.Assets = append([]domain.Asset{}, zone.Assets...)
		for i := range copied.Assets {
			copied.Assets[i].MeterParents = nil
		}
		copied.Zones = stripMeterParents(zone.Zones)
		result = append(result, copied)
	}
	return result
}

// randomMeterDocument is a document of shared devices, groups and parent lists
// drawn without regard for validation: cycles, unknown ids, broken weights.
func randomMeterDocument(random *rand.Rand) domain.Environment {
	env := domain.Environment{Id: "env-random", Name: "Zufall", Owner: "user-a", Type: domain.IndustrialSite}
	assets := 2 + random.IntN(25)
	groups := random.IntN(5)
	ids := []string{"gibt-es-nicht", RootNodeId, ""}
	for i := 0; i < assets; i++ {
		ids = append(ids, fmt.Sprintf("a%d", i))
	}
	for i := 0; i < groups; i++ {
		ids = append(ids, fmt.Sprintf("g%d", i))
	}
	parents := func() []domain.MeterParent {
		result := []domain.MeterParent{}
		count := random.IntN(4)
		explicit := random.IntN(3) == 0
		for k := 0; k < count; k++ {
			parent := domain.MeterParent{Id: ids[random.IntN(len(ids))], Conversion: random.IntN(2) == 0}
			if explicit {
				parent.Weight = random.IntN(101)
			}
			result = append(result, parent)
		}
		return result
	}
	kinds := []domain.AssetKind{domain.AssetMeter, domain.AssetInverter, domain.AssetMachine, domain.AssetSensor, domain.AssetActuator}
	zones := []domain.Zone{{Id: "z0", Name: "Z0", Type: domain.ZoneHall}, {Id: "z1", Name: "Z1", Type: domain.ZoneHall}}
	for i := 0; i < assets; i++ {
		asset := domain.Asset{Id: fmt.Sprintf("a%d", i), Name: fmt.Sprintf("A%d", i), Kind: kinds[random.IntN(len(kinds))]}
		//few devices for many assets, so sharing folds acyclic statements into cycles
		if random.IntN(6) > 0 {
			asset.ExternalRef = fmt.Sprintf("urn:device:%d", random.IntN(1+assets/2))
		}
		if random.IntN(2) == 0 {
			asset.MeterParents = parents()
		}
		if random.IntN(3) == 0 {
			asset.SubmeteredBy = ids[random.IntN(len(ids))]
		}
		zone := &zones[random.IntN(len(zones))]
		zone.Assets = append(zone.Assets, asset)
	}
	env.Zones = zones
	for i := 0; i < groups; i++ {
		env.MeterGroups = append(env.MeterGroups, domain.MeterGroup{Id: ids[3+assets+random.IntN(groups)], Name: "G", Parents: parents()})
	}
	return env
}

// Whatever a document says, the repository has to accept the meter graph: a
// refused graph costs the whole mirror. Seeded, so a failure reproduces.
func TestEveryRandomDocumentGivesAnAcceptedMeterGraph(t *testing.T) {
	random := rand.New(rand.NewPCG(4863, 1))
	for i := 0; i < 5000; i++ {
		env := randomMeterDocument(random)
		graph := BuildMeterGraph(env)
		if err := graph.Valid(); err != nil {
			t.Fatalf("document %d: the repository would refuse the meter graph: %v\n%+v", i, err, env)
		}
		if again := BuildMeterGraph(env); !reflect.DeepEqual(graph, again) {
			t.Fatalf("document %d: two builds differ", i)
		}
	}
}
