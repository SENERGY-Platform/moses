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
	"reflect"
	"testing"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
)

// ---------------------------------------------------------------------------
// The mapping is a contract with applications nobody here controls, and none of
// it is expressed in a schema. Every test below pins one clause of it: change
// the mapping and a graph view somewhere else stops finding the root, the names
// or the devices.
// ---------------------------------------------------------------------------

// nestedEnvironment is a site with two levels of zones and three assets, one of
// which has no platform device.
func nestedEnvironment() domain.Environment {
	return domain.Environment{
		Id:    "env-1",
		Name:  "Metallbau Musterstadt",
		Owner: "user-a",
		Type:  domain.IndustrialSite,
		Zones: []domain.Zone{{
			Id: "zone-halle", Name: "Halle 1", Type: domain.ZoneHall,
			Assets: []domain.Asset{{
				Id: "asset-kompressor", Name: "Kompressor 1", Kind: domain.AssetMachine,
				ExternalRef: "urn:device:kompressor",
			}, {
				//no device: a helper inside the simulation
				Id: "asset-summe", Name: "Summe Halle", Kind: domain.AssetMeter,
			}},
			Zones: []domain.Zone{{
				Id: "zone-nebenraum", Name: "Nebenraum", Type: domain.ZoneRoom,
				Assets: []domain.Asset{{
					Id: "asset-zaehler", Name: "Zähler", Kind: domain.AssetMeter,
					ExternalRef: "urn:device:zaehler",
				}},
			}},
		}},
	}
}

func nodeById(graph models.Graph, id string) (models.Node, bool) {
	for _, node := range graph.Nodes {
		if node.Id == id {
			return node, true
		}
	}
	return models.Node{}, false
}

func edgeFrom(graph models.Graph, from string) (models.Edge, bool) {
	for _, edge := range graph.Edges {
		if edge.FromNodeId == from {
			return edge, true
		}
	}
	return models.Edge{}, false
}

func attribute(attributes []models.Attribute, key string) string {
	for _, attr := range attributes {
		if attr.Key == key {
			return attr.Value
		}
	}
	return ""
}

// The root node is found by its literal id, not by looking for the node without
// outgoing edges, and its name attribute is the name of the whole graph.
func TestTheRootNodeCarriesTheEnvironmentName(t *testing.T) {
	graph := Build(nestedEnvironment())

	root, ok := nodeById(graph, "root")
	if !ok {
		t.Fatalf("no node with the id %q, got %+v", RootNodeId, graph.Nodes)
	}
	if name := attribute(root.Attributes, NameAttribute); name != "Metallbau Musterstadt" {
		t.Errorf("expected the environment name as the name attribute of the root, got %q", name)
	}
	if _, hasOutgoing := edgeFrom(graph, RootNodeId); hasOutgoing {
		t.Error("the root is where the graph ends, it must have no outgoing edge")
	}
	if root.ResourceType != "" || root.ResourceId != "" {
		t.Errorf("the root is structure, not a resource, got type %q and resource %q", root.ResourceType, root.ResourceId)
	}
}

// An edge points from the child to the parent. Reversing this reverses the tree
// for every reader.
func TestEdgesPointFromTheChildToTheParent(t *testing.T) {
	graph := Build(nestedEnvironment())

	for _, expected := range []struct{ from, to string }{
		{"zone-halle", RootNodeId},
		{"zone-nebenraum", "zone-halle"},
		{"urn:device:kompressor", "zone-halle"},
		{"urn:device:zaehler", "zone-nebenraum"},
	} {
		edge, ok := edgeFrom(graph, expected.from)
		if !ok {
			t.Errorf("no edge out of %q, got %+v", expected.from, graph.Edges)
			continue
		}
		if edge.ToNodeId != expected.to {
			t.Errorf("expected %q to point at %q, it points at %q", expected.from, expected.to, edge.ToNodeId)
		}
	}
	if len(graph.Edges) != 4 {
		t.Errorf("expected exactly one edge per node below the root, got %d: %+v", len(graph.Edges), graph.Edges)
	}
}

// A device node is addressed by the device id on both fields: the frontend
// looks a node up by its id, the repository resolves the device by resource id.
func TestADeviceNodeCarriesTheDeviceIdAsIdAndResourceId(t *testing.T) {
	graph := Build(nestedEnvironment())

	node, ok := nodeById(graph, "urn:device:kompressor")
	if !ok {
		t.Fatalf("no node for the provisioned asset, got %+v", graph.Nodes)
	}
	if node.ResourceId != node.Id {
		t.Errorf("expected id and resource id to be the same device id, got %q and %q", node.Id, node.ResourceId)
	}
	if node.ResourceType != models.GraphResourceTypeDevice {
		t.Errorf("expected the resource type %q, got %q", models.GraphResourceTypeDevice, node.ResourceType)
	}
	if name := attribute(node.Attributes, NameAttribute); name != "Kompressor 1" {
		t.Errorf("expected the asset name as the name attribute, got %q", name)
	}
}

// An asset without a platform device carries nothing a consumer could read, so
// it is left out rather than mapped to an empty node.
func TestAnAssetWithoutADeviceIsLeftOut(t *testing.T) {
	graph := Build(nestedEnvironment())

	for _, node := range graph.Nodes {
		if attribute(node.Attributes, NameAttribute) == "Summe Halle" {
			t.Fatalf("an asset without a device must not become a node, got %+v", node)
		}
	}
	//root, two zones, two devices
	if len(graph.Nodes) != 5 {
		t.Errorf("expected 5 nodes, got %d: %+v", len(graph.Nodes), graph.Nodes)
	}
}

// The graph is rebuilt on every save and compared against what is stored, so a
// build that differed from the last one would make every save look like a
// change - and, with a generated id, would create a second graph each time.
func TestBuildingTwiceProducesTheSameGraph(t *testing.T) {
	first := Build(nestedEnvironment())
	second := Build(nestedEnvironment())

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two builds of one document differ:\n%+v\n%+v", first, second)
	}
	if first.Id != "" {
		t.Errorf("an environment that was never mirrored has no graph id yet, got %q", first.Id)
	}
	//a stored ref is carried into the graph, which is what makes the next
	//SetGraph an update of that graph instead of a new one
	env := nestedEnvironment()
	env.ExternalGraphRef = "urn:infai:ses:graph:1"
	if built := Build(env); built.Id != "urn:infai:ses:graph:1" {
		t.Errorf("expected the stored ref as the graph id, got %q", built.Id)
	}
}

// The repository refuses a graph its own Valid rejects, and the request would
// then fail with a 400 nobody sees except in a warning. Every shape this package
// can produce has to pass it.
func TestEveryBuiltGraphIsAcceptedByTheRepositoryModel(t *testing.T) {
	empty := nestedEnvironment()
	empty.Zones = nil

	deviceless := nestedEnvironment()
	deviceless.Zones[0].Assets = nil
	deviceless.Zones[0].Zones[0].Assets = nil

	deep := nestedEnvironment()
	deep.Zones = append(deep.Zones, domain.Zone{Id: "zone-buero", Name: "Büro", Type: domain.ZoneUnit})

	for name, env := range map[string]domain.Environment{
		"nested":       nestedEnvironment(),
		"no zones":     empty,
		"no devices":   deviceless,
		"empty zone":   deep,
		"shared ref":   environmentWithOneDeviceOnTwoAssets(),
		"zone id root": environmentWithAZoneCalledRoot(),
	} {
		graph := Build(env)
		if err := graph.Valid(); err != nil {
			t.Errorf("%s: the repository would refuse this graph: %v", name, err)
		}
		if graph.ContainsLoop() {
			t.Errorf("%s: a location topology cannot contain a loop", name)
		}
	}
}

// A location topology is unweighted, which the repository spells as "every edge
// carries all of it": a weight of 0 is refused outright, and the outgoing
// weights of a node have to sum to 100.
func TestEveryEdgeCarriesTheWholeFlow(t *testing.T) {
	graph := Build(nestedEnvironment())

	for _, edge := range graph.Edges {
		if edge.Weight != 100 {
			t.Errorf("expected the whole flow on %q, got the weight %d", edge.Id, edge.Weight)
		}
	}
}

// Edge ids are derived from both ends, so they survive a rebuild unchanged.
func TestEdgeIdsAreDerivedFromTheNodesTheyConnect(t *testing.T) {
	graph := Build(nestedEnvironment())

	edge, ok := edgeFrom(graph, "zone-nebenraum")
	if !ok {
		t.Fatal("no edge out of the sub zone")
	}
	if edge.Id != "zone-nebenraum->zone-halle" {
		t.Errorf("expected a derived edge id, got %q", edge.Id)
	}
}

// The environment id is on the graph as provenance, so a graph found elsewhere
// can be traced back to the simulation that owns it.
func TestTheGraphNamesTheEnvironmentItMirrors(t *testing.T) {
	graph := Build(nestedEnvironment())

	if got := attribute(graph.Attributes, EnvironmentAttribute); got != "env-1" {
		t.Errorf("expected the environment id under %q, got %q", EnvironmentAttribute, got)
	}
	if graph.Owner != "user-a" {
		t.Errorf("the mirror belongs to the owner of the environment, got %q", graph.Owner)
	}
}

// environmentWithOneDeviceOnTwoAssets is the case the device cleanup already
// contemplates: two assets publishing through one platform device. Asset ids are
// unique document wide, the device references behind them are not.
func environmentWithOneDeviceOnTwoAssets() domain.Environment {
	env := nestedEnvironment()
	env.Zones[0].Zones[0].Assets[0].ExternalRef = "urn:device:kompressor"
	return env
}

// A shared device would otherwise appear as two nodes with one id, which the
// repository refuses - taking the whole mirror down over a modelling detail.
func TestOneDeviceOnTwoAssetsBecomesOneNode(t *testing.T) {
	graph := Build(environmentWithOneDeviceOnTwoAssets())

	found := 0
	for _, node := range graph.Nodes {
		if node.ResourceId == "urn:device:kompressor" {
			found++
		}
	}
	if found != 1 {
		t.Errorf("expected the shared device once, got %d nodes: %+v", found, graph.Nodes)
	}
	//the second asset contributes nothing, so its zone keeps only its own edge
	if _, ok := nodeById(graph, "zone-nebenraum"); !ok {
		t.Error("the zone of the skipped asset must still be in the graph")
	}
}

// environmentWithAZoneCalledRoot is legal input: zone ids are unique and
// non-empty, nothing says they may not be the word the root node uses.
func environmentWithAZoneCalledRoot() domain.Environment {
	env := nestedEnvironment()
	env.Zones[0].Id = RootNodeId
	return env
}

// A zone that cannot have a node of its own hands its content to its parent.
// Dropping the subtree would lose devices; a duplicate node id would have the
// repository refuse the whole graph.
func TestAZoneWhoseIdIsTakenPassesItsContentToItsParent(t *testing.T) {
	graph := Build(environmentWithAZoneCalledRoot())

	if count := len(graph.Nodes); count != 4 {
		t.Errorf("expected the colliding zone to contribute no node, got %d: %+v", count, graph.Nodes)
	}
	edge, ok := edgeFrom(graph, "urn:device:kompressor")
	if !ok {
		t.Fatal("the device of the colliding zone was lost")
	}
	if edge.ToNodeId != RootNodeId {
		t.Errorf("expected the device to attach to the parent of the colliding zone, got %q", edge.ToNodeId)
	}
	sub, ok := edgeFrom(graph, "zone-nebenraum")
	if !ok {
		t.Fatal("the sub zone of the colliding zone was lost")
	}
	if sub.ToNodeId != RootNodeId {
		t.Errorf("expected the sub zone to attach to the parent of the colliding zone, got %q", sub.ToNodeId)
	}
}
