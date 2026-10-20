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

func edgesFrom(graph models.Graph, from string) int {
	count := 0
	for _, edge := range graph.Edges {
		if edge.FromNodeId == from {
			count++
		}
	}
	return count
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

// The submetered_by index is built fresh on every call to Build from a plain
// map, whose iteration order Go deliberately randomizes - a build that leaked
// that randomness into the graph would fail this intermittently rather than
// every time.
func TestBuildingASubmeteredEnvironmentTwiceProducesTheSameGraph(t *testing.T) {
	first := Build(environmentWithSubmetering())
	second := Build(environmentWithSubmetering())

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two builds of one submetered document differ:\n%+v\n%+v", first, second)
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
		"submetered":   environmentWithSubmetering(),
		"device cycle": environmentWithADeviceCycle(),
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

// A sub-metered asset's edge attaches to the device of the asset named by
// submetered_by instead of its zone, so the mirrored graph reads as a meter
// tree for that edge.
func TestASubmeteredAssetAttachesToTheMetersDeviceNode(t *testing.T) {
	env := nestedEnvironment()
	// the compressor is measured again by the meter in the side room: what
	// the meter reads already contains what the compressor draws
	env.Zones[0].Assets[0].SubmeteredBy = "asset-zaehler"

	graph := Build(env)

	edge, ok := edgeFrom(graph, "urn:device:kompressor")
	if !ok {
		t.Fatal("no edge out of the sub-metered device")
	}
	if edge.ToNodeId != "urn:device:zaehler" {
		t.Errorf("expected the edge to attach to the meter's device, got %q", edge.ToNodeId)
	}
	if edge.Id != "urn:device:kompressor->urn:device:zaehler" {
		t.Errorf("expected the edge id still derived child->parent, got %q", edge.Id)
	}
}

// The target of a submetered_by reference can sit in a zone the recursion has
// not reached yet, or never will on the path to the asset that names it - the
// index Build builds upfront over the whole document has to find it anyway.
func TestASubmeterReferenceAcrossZonesFindsTheDevice(t *testing.T) {
	env := nestedEnvironment()
	env.Zones = append(env.Zones, domain.Zone{
		Id: "zone-technik", Name: "Technikraum", Type: domain.ZoneRoom,
		Assets: []domain.Asset{{
			Id: "asset-hauptzaehler", Name: "Hauptzähler", Kind: domain.AssetMeter,
			ExternalRef: "urn:device:hauptzaehler",
		}},
	})
	env.Zones[0].Assets[0].SubmeteredBy = "asset-hauptzaehler"

	graph := Build(env)

	edge, ok := edgeFrom(graph, "urn:device:kompressor")
	if !ok {
		t.Fatal("no edge out of the sub-metered device")
	}
	if edge.ToNodeId != "urn:device:hauptzaehler" {
		t.Errorf("expected the edge to attach to the device in the other zone, got %q", edge.ToNodeId)
	}
}

// A submetered_by target without a platform device of its own has nothing for
// the edge to attach to, so the asset falls back to its zone like an
// unsubmetered one would.
func TestASubmeterReferenceToADevicelessAssetFallsBackToTheZone(t *testing.T) {
	env := nestedEnvironment()
	// asset-summe has no external_ref
	env.Zones[0].Assets[0].SubmeteredBy = "asset-summe"

	graph := Build(env)

	edge, ok := edgeFrom(graph, "urn:device:kompressor")
	if !ok {
		t.Fatal("no edge out of the compressor")
	}
	if edge.ToNodeId != "zone-halle" {
		t.Errorf("expected the fallback to the zone, got %q", edge.ToNodeId)
	}
}

// Two assets are allowed to share one platform device. If the submetered_by
// target happens to be one of those, the edge would point the shared device
// at itself - a self-loop the repository would reject the whole graph over -
// so this falls back to the zone too.
func TestASubmeteredAssetSharingTheTargetsDeviceFallsBackToTheZone(t *testing.T) {
	env := nestedEnvironment()
	env.Zones[0].Zones[0].Assets[0].ExternalRef = "urn:device:kompressor"
	env.Zones[0].Assets[0].SubmeteredBy = "asset-zaehler"

	graph := Build(env)

	edge, ok := edgeFrom(graph, "urn:device:kompressor")
	if !ok {
		t.Fatal("no edge out of the compressor")
	}
	if edge.ToNodeId != "zone-halle" {
		t.Errorf("expected the fallback to the zone instead of a self-loop, got %q", edge.ToNodeId)
	}
	if graph.ContainsLoop() {
		t.Error("must not contain a loop")
	}
}

// environmentWithADeviceCycle is free of sub-metering cycles as the document
// models them - A is metered by B, B by C, C by nobody - and still asks for a
// cycle of device edges: C publishes through the same platform device as A, so
// the two asset edges fold into one pair of device nodes pointing at each
// other. Nothing in lib/domain can see this, because it reasons about assets
// while the graph hangs devices.
func environmentWithADeviceCycle() domain.Environment {
	return domain.Environment{
		Id: "env-ring", Name: "Ringschluss", Owner: "user-a", Type: domain.IndustrialSite,
		Zones: []domain.Zone{{
			Id: "zone-halle", Name: "Halle 1", Type: domain.ZoneHall,
			Assets: []domain.Asset{{
				Id: "asset-a", Name: "Strang A", Kind: domain.AssetMachine,
				ExternalRef: "urn:device:x", SubmeteredBy: "asset-b",
			}, {
				Id: "asset-b", Name: "Zähler B", Kind: domain.AssetMeter,
				ExternalRef: "urn:device:y", SubmeteredBy: "asset-c",
			}, {
				//publishes through the device of A
				Id: "asset-c", Name: "Strang C", Kind: domain.AssetMeter,
				ExternalRef: "urn:device:x",
			}},
		}},
	}
}

// The repository rejects a graph containing a loop outright, so a device cycle
// would cost the whole mirror rather than one edge. Exactly one edge of the
// cycle falls back to its zone, and which one is decided by document order:
// the earliest device involved is the one that loses its sub-metering edge, so
// the same document always breaks the same edge.
func TestADeviceCycleThroughASharedDeviceFallsBackToTheZone(t *testing.T) {
	graph := Build(environmentWithADeviceCycle())

	if err := graph.Valid(); err != nil {
		t.Errorf("the repository would refuse this graph: %v", err)
	}
	if graph.ContainsLoop() {
		t.Fatalf("the mirror must not contain a loop, got %+v", graph.Edges)
	}
	first, ok := edgeFrom(graph, "urn:device:x")
	if !ok {
		t.Fatal("no edge out of the shared device")
	}
	if first.ToNodeId != "zone-halle" {
		t.Errorf("expected the first device of the cycle to fall back to its zone, got %q", first.ToNodeId)
	}
	second, ok := edgeFrom(graph, "urn:device:y")
	if !ok {
		t.Fatal("no edge out of the second device of the cycle")
	}
	if second.ToNodeId != "urn:device:x" {
		t.Errorf("expected only one edge of the cycle to be given up, got %q", second.ToNodeId)
	}
	if again := Build(environmentWithADeviceCycle()); !reflect.DeepEqual(graph, again) {
		t.Fatalf("breaking the cycle has to pick the same edge every time:\n%+v\n%+v", graph, again)
	}
}

// The node of a shared device belongs to the first asset that carries it, but
// a later carrier's submetered_by is a statement about that same device and
// has to count too - it is not the node that is at stake, it is where the node
// hangs.
func TestALaterCarriersSubmeteringStillCounts(t *testing.T) {
	env := nestedEnvironment()
	//a second asset on the compressor's device, listed after it, and the only
	//one of the two that says where that device hangs
	env.Zones[0].Assets = append(env.Zones[0].Assets, domain.Asset{
		Id: "asset-teilstrang", Name: "Teilstrang", Kind: domain.AssetMachine,
		ExternalRef: "urn:device:kompressor", SubmeteredBy: "asset-zaehler",
	})

	graph := Build(env)

	edge, ok := edgeFrom(graph, "urn:device:kompressor")
	if !ok {
		t.Fatal("no edge out of the shared device")
	}
	if edge.ToNodeId != "urn:device:zaehler" {
		t.Errorf("expected the later carrier's submetering to place the device, got %q", edge.ToNodeId)
	}
	if count := edgesFrom(graph, "urn:device:kompressor"); count != 1 {
		t.Errorf("expected exactly one edge out of the shared device, got %d: %+v", count, graph.Edges)
	}
}

// Two assets publishing through one device, each sub-metered by a different
// meter. The device is one node and can have one parent; nothing in the
// document ranks one carrier's statement above the other's, so neither is
// followed and the device stays in its zone.
func TestConflictingCarrierTargetsFallBackToTheZone(t *testing.T) {
	env := nestedEnvironment()
	env.Zones[0].Assets[0].SubmeteredBy = "asset-zaehler"
	env.Zones[0].Assets = append(env.Zones[0].Assets,
		domain.Asset{
			Id: "asset-hauptzaehler", Name: "Hauptzähler", Kind: domain.AssetMeter,
			ExternalRef: "urn:device:hauptzaehler",
		},
		domain.Asset{
			Id: "asset-nebenstrang", Name: "Nebenstrang", Kind: domain.AssetMachine,
			ExternalRef: "urn:device:kompressor", SubmeteredBy: "asset-hauptzaehler",
		})

	graph := Build(env)

	edge, ok := edgeFrom(graph, "urn:device:kompressor")
	if !ok {
		t.Fatal("no edge out of the contested device")
	}
	if edge.ToNodeId != "zone-halle" {
		t.Errorf("expected the contested device to stay in its zone, got %q", edge.ToNodeId)
	}
	//the two meters themselves are untouched by the conflict
	if other, _ := edgeFrom(graph, "urn:device:hauptzaehler"); other.ToNodeId != "zone-halle" {
		t.Errorf("expected the uninvolved meter to keep its zone edge, got %q", other.ToNodeId)
	}
	if other, _ := edgeFrom(graph, "urn:device:zaehler"); other.ToNodeId != "zone-nebenraum" {
		t.Errorf("expected the uninvolved meter to keep its zone edge, got %q", other.ToNodeId)
	}
}

// An external_ref is free to collide with a zone id - nothing in the document
// forbids it - and the zone that got there first keeps the node. The asset
// behind the colliding ref then has no device node of its own, so its
// submetered_by has nothing to hang: it must not add a second outgoing edge to
// the zone's node, which the repository would refuse (the outgoing weights of a
// node have to sum to 0 or 100, and two edges make 200).
func TestADeviceRefCollidingWithAZoneIdAddsNoSecondEdge(t *testing.T) {
	env := nestedEnvironment()
	env.Zones[0].Zones[0].Assets = append(env.Zones[0].Zones[0].Assets, domain.Asset{
		Id: "asset-kollision", Name: "Kollision", Kind: domain.AssetMachine,
		ExternalRef: "zone-halle", SubmeteredBy: "asset-zaehler",
	})

	graph := Build(env)

	if err := graph.Valid(); err != nil {
		t.Errorf("the repository would refuse this graph: %v", err)
	}
	if count := edgesFrom(graph, "zone-halle"); count != 1 {
		t.Errorf("expected the zone to keep its one edge, got %d: %+v", count, graph.Edges)
	}
	edge, ok := edgeFrom(graph, "zone-halle")
	if !ok {
		t.Fatal("the zone lost its edge")
	}
	if edge.ToNodeId != RootNodeId {
		t.Errorf("expected the zone to keep pointing at the root, got %q", edge.ToNodeId)
	}
}

// environmentWithSubmetering is a plain valid case for the repository model
// test below: one asset feeding into another's device, no fallback involved.
func environmentWithSubmetering() domain.Environment {
	env := nestedEnvironment()
	env.Zones[0].Assets[0].SubmeteredBy = "asset-zaehler"
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
