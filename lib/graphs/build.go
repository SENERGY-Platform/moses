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

// Package graphs maps an environment onto the graph model of the
// device-repository, so a simulated site is consumable by everything that reads
// the platform's graphs: the graph view, the energy flow evaluations.
//
// Build is pure. The mapping is the whole contract with those consumers, and a
// contract that can be checked by a table test is worth more than one spread
// over a handler.
package graphs

import (
	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
)

const (
	// RootNodeId is the id of the node every top level zone points at. Fixed by
	// the frontend, which finds the root of a graph by this id rather than by
	// looking for the node without outgoing edges.
	RootNodeId = "root"

	// NameAttribute carries the display name of a node. The graph itself has no
	// name field, so the frontend shows the name attribute of the root node as
	// the name of the whole graph.
	NameAttribute = "name"

	// EnvironmentAttribute marks a graph as the mirror of a moses environment
	// and names the environment it mirrors.
	//
	// Provenance only, never a lookup path: the repository's attribute filter
	// ANDs multiple keys, and its json variant is double-encoded by the go
	// client, so a search by this attribute is not something to build on. The
	// way back to the graph is Environment.ExternalGraphRef.
	EnvironmentAttribute = "moses/environment"

	// EdgeWeight is the share of the flow an edge carries, in percent.
	//
	// Weights exist for the apportioning case - one meter supplying two areas,
	// split 70/30 - which a pure location topology does not have: every node
	// here has exactly one parent and therefore passes on everything it has.
	// 100 rather than 0 because the repository rejects a graph whose edge weight
	// is outside 1..100, and requires the outgoing weights of a node to sum to
	// either 0 (no outgoing edge at all) or 100. An unweighted topology is
	// therefore spelled "every edge carries all of it", not "no edge carries
	// anything".
	EdgeWeight = 100
)

// Build maps an environment onto the graph that mirrors it.
//
// Four conventions of the frontend are contract here, none of them expressed in
// a schema, all of them pinned by build_test.go:
//   - an edge points from the child to the parent (FromNodeId is the child),
//   - the root node has the id "root",
//   - the display name of a node is its "name" attribute, and the one of the
//     root node names the whole graph,
//   - a device node carries the device id in both Id and ResourceId.
//
// The mapping runs in two phases. The walk (addZone) places every node, every
// zone edge, and collects per device what the assets carrying it ask for;
// addDeviceEdges then decides the single outgoing edge of every device node.
// The split exists because the document models sub-metering per asset while the
// graph has one node per device, and two assets may share one device: what a
// device hangs under is a decision over all of its carriers together, and
// whether the resulting edges close a loop is only knowable once every one of
// them has been seen.
//
// The id of the returned graph is Environment.ExternalGraphRef, which is empty
// for an environment that was never mirrored - the repository then assigns one
// (see SetGraph in its client). Build never invents an id: a graph built twice
// from the same document has to be the same graph, or the mirror would create a
// new one on every save.
func Build(env domain.Environment) models.Graph {
	graph := models.Graph{
		Id: env.ExternalGraphRef,
		// the owner of the mirror is the owner of the environment, not whoever
		// saved it: the repository requires an owner, and an admin editing
		// somebody else's environment must not take their graph over
		Owner:      env.Owner,
		Attributes: []models.Attribute{{Key: EnvironmentAttribute, Value: env.Id}},
		Nodes: []models.Node{{
			Id:         RootNodeId,
			Attributes: named(env.Name),
		}},
		Edges: []models.Edge{},
	}
	// node ids have to be unique for the repository to accept the graph, and
	// the document cannot guarantee it: asset ids are unique, the device
	// references behind them are not (two assets may publish through one
	// device), and a zone id of "root" collides with the root node.
	taken := map[string]bool{RootNodeId: true}
	// refs indexes every asset's platform device by asset id, over the whole
	// document rather than the branch currently being walked: a
	// submetered_by target can sit anywhere - a sibling asset, a different
	// zone, a zone not reached yet by the recursion below.
	refs := externalRefs(env)
	devices := &deviceEdges{byRef: map[string]*deviceInfo{}}
	for _, zone := range env.Zones {
		addZone(&graph, taken, zone, RootNodeId, refs, devices)
	}
	addDeviceEdges(&graph, devices)
	return graph
}

// externalRefs maps the id of every asset that has a platform device to that
// device, so addZone can resolve a submetered_by target without caring where in
// the tree that target sits relative to the asset being mapped right now.
//
// An asset without a device contributes no entry, which makes a lookup miss and
// a deviceless target the same answer - "" - and rightly so: neither has a node
// an edge could attach to.
func externalRefs(env domain.Environment) map[string]string {
	refs := map[string]string{}
	var walk func(zones []domain.Zone)
	walk = func(zones []domain.Zone) {
		for _, zone := range zones {
			for _, asset := range zone.Assets {
				if asset.Id != "" && asset.ExternalRef != "" {
					refs[asset.Id] = asset.ExternalRef
				}
			}
			walk(zone.Zones)
		}
	}
	walk(env.Zones)
	return refs
}

// deviceInfo is what the walk collects about one device node, so that
// addDeviceEdges can decide the one edge leaving it.
type deviceInfo struct {
	// zone is the node this device falls back to: the zone of the FIRST asset
	// that carried it. The first carrier decides, so that the placement of a
	// device in a document that did not change does not move when a second
	// asset elsewhere starts publishing through it.
	zone string
	// wants holds the resolved device of every submetered_by target named by
	// an asset carrying this device - one entry per carrier that names one,
	// already filtered to targets that have a device of their own and are not
	// this very device.
	wants []string
}

// deviceEdges accumulates the devices of the walk, in the order their nodes
// were created. The order is document order and is what makes every decision
// below deterministic: a map would hand the same document a different answer
// on the next save.
type deviceEdges struct {
	byRef map[string]*deviceInfo
	order []string
}

// first records a device node the walk has just created, under the zone it
// sits in.
func (this *deviceEdges) first(ref string, zoneNodeId string) {
	this.byRef[ref] = &deviceInfo{zone: zoneNodeId}
	this.order = append(this.order, ref)
}

// want records that one carrier of this device asks to hang under target.
//
// A device with no entry is one whose id was already taken by a zone: the walk
// created no node for it, so there is nothing an edge could leave, and the wish
// is dropped with the node.
func (this *deviceEdges) want(ref string, target string) {
	if info, known := this.byRef[ref]; known {
		info.wants = append(info.wants, target)
	}
}

// candidate is the device node this one hangs under instead of its zone, or ""
// when it hangs under its zone after all: either because no carrier asked for
// anything, or because two carriers of the same device asked for different
// targets. The device is one node and can only have one parent, and there is
// nothing in the document that makes one carrier's wish outrank the other's -
// picking either would be an invented rule, and picking by document order would
// silently discard a modelling statement. The zone is the honest answer, and
// the only one that stays right when the two assets are later split onto
// devices of their own.
func (this *deviceInfo) candidate() string {
	target := ""
	for _, want := range this.wants {
		if target == "" {
			target = want
			continue
		}
		if want != target {
			return ""
		}
	}
	return target
}

// addDeviceEdges appends the one outgoing edge of every device node, after the
// walk rather than during it.
//
// A device's edge ordinarily points at its zone. A sub-metered one points at
// the device of the asset named by submetered_by instead, so that edge reads as
// "feeds into that meter" rather than "sits in this zone" - the meter tree
// consumers of the graph subtract sub-metering along. Five cases fall back to
// the zone edge:
//
//   - no carrier of the device is sub-metered at all (SubmeteredBy == ""),
//   - the target has no platform device of its own - the same deviceless case
//     addZone already leaves out for the asset itself,
//   - the target shares this device - two assets are allowed to publish through
//     one device, and an edge from that device to itself would be a self-loop
//     the repository rejects the whole graph over,
//   - two carriers of the device name targets that resolve to different devices
//     (see candidate),
//   - the edge would close a cycle of device edges (see below).
//
// The first three are filtered while collecting, in addZone.
func addDeviceEdges(graph *models.Graph, devices *deviceEdges) {
	parents := map[string]string{}
	for _, ref := range devices.order {
		parents[ref] = devices.byRef[ref].candidate()
	}

	// Sub-metering is checked for cycles per asset in lib/domain, but the graph
	// hangs devices, not assets, and a device shared by several assets folds
	// several asset edges into one: A on device X is metered by B on device Y,
	// and B is metered by C, which publishes through device X again. No asset
	// meters itself there, not even indirectly, and the devices still ask for
	// X->Y and Y->X. The repository refuses a graph containing a loop outright,
	// so leaving it in would cost the whole mirror, not one edge.
	//
	// One pass is enough. Each device has at most one candidate parent, so the
	// cycles are node-disjoint, and dropping a candidate can only affect the
	// cycle its device is on. Every cycle is therefore still intact when its
	// first member in document order is reached, is found there, and is broken
	// there - the earliest member is the one that loses its submetering edge,
	// which makes the same document break the same edge every time.
	for _, ref := range devices.order {
		if parents[ref] == "" {
			continue
		}
		if leadsBackTo(ref, parents) {
			parents[ref] = ""
		}
	}

	for _, ref := range devices.order {
		parent := parents[ref]
		if parent == "" {
			parent = devices.byRef[ref].zone
		}
		graph.Edges = append(graph.Edges, edgeTo(ref, parent))
	}
}

// leadsBackTo reports whether following the candidate parents from start
// arrives back at start.
//
// visited is what terminates the walk, not what answers the question: a chain
// may run into a cycle that start is not itself part of, and start then keeps
// its parent - the cycle is broken at one of its own members instead.
func leadsBackTo(start string, parents map[string]string) bool {
	visited := map[string]bool{start: true}
	for at := parents[start]; at != ""; at = parents[at] {
		if at == start {
			return true
		}
		if visited[at] {
			return false
		}
		visited[at] = true
	}
	return false
}

// addZone adds one zone and everything below it, below the node named by parent.
//
// A zone whose id is unusable contributes no node of its own and its content
// attaches to its parent instead. Dropping the subtree would lose devices, and
// keeping a duplicate node id would have the repository refuse the whole graph
// over one pathological id.
//
// Assets contribute their node here and their edge in addDeviceEdges. The node
// belongs to the first asset carrying the device, which is what keeps an
// unchanged document mapping to an unchanged graph; the edge is a decision over
// every carrier of that device together and cannot be made from inside the walk.
func addZone(graph *models.Graph, taken map[string]bool, zone domain.Zone, parent string, refs map[string]string, devices *deviceEdges) {
	nodeId := parent
	if zone.Id != "" && !taken[zone.Id] {
		nodeId = zone.Id
		taken[nodeId] = true
		graph.Nodes = append(graph.Nodes, models.Node{Id: nodeId, Attributes: named(zone.Name)})
		graph.Edges = append(graph.Edges, edgeTo(nodeId, parent))
	}
	for _, asset := range zone.Assets {
		// an asset without a platform device is left out on purpose: it is a
		// helper inside the simulation - a computed total, a placeholder - and
		// carries nothing a consumer of the graph could read. A node for it
		// would be a location with no data behind it.
		if asset.ExternalRef == "" {
			continue
		}
		if !taken[asset.ExternalRef] {
			taken[asset.ExternalRef] = true
			graph.Nodes = append(graph.Nodes, models.Node{
				Id:           asset.ExternalRef,
				ResourceId:   asset.ExternalRef,
				ResourceType: models.GraphResourceTypeDevice,
				Attributes:   named(asset.Name),
			})
			devices.first(asset.ExternalRef, nodeId)
		}
		// deliberately not part of the branch above: a second asset on the same
		// device contributes no node, but its submetered_by is a statement
		// about that device all the same and would otherwise be lost to the
		// order the assets happen to appear in.
		if asset.SubmeteredBy == "" {
			continue
		}
		if target := refs[asset.SubmeteredBy]; target != "" && target != asset.ExternalRef {
			devices.want(asset.ExternalRef, target)
		}
	}
	for _, sub := range zone.Zones {
		addZone(graph, taken, sub, nodeId, refs, devices)
	}
}

// edgeTo builds the edge from a child to its parent. The id is derived from both
// ends rather than generated, so that a graph built twice from one document
// carries the same edge ids - an edge id that changed on every save would make
// every mirror look like a change. It is unique because a node has at most one
// parent, so it appears as the source of at most one edge.
func edgeTo(child string, parent string) models.Edge {
	return models.Edge{
		Id:         child + "->" + parent,
		FromNodeId: child,
		ToNodeId:   parent,
		Weight:     EdgeWeight,
	}
}

func named(name string) []models.Attribute {
	return []models.Attribute{{Key: NameAttribute, Value: name}}
}
