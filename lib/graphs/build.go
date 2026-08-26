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
	for _, zone := range env.Zones {
		addZone(&graph, taken, zone, RootNodeId)
	}
	return graph
}

// addZone adds one zone and everything below it, below the node named by parent.
//
// A zone whose id is unusable contributes no node of its own and its content
// attaches to its parent instead. Dropping the subtree would lose devices, and
// keeping a duplicate node id would have the repository refuse the whole graph
// over one pathological id.
func addZone(graph *models.Graph, taken map[string]bool, zone domain.Zone, parent string) {
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
		if asset.ExternalRef == "" || taken[asset.ExternalRef] {
			continue
		}
		taken[asset.ExternalRef] = true
		graph.Nodes = append(graph.Nodes, models.Node{
			Id:           asset.ExternalRef,
			ResourceId:   asset.ExternalRef,
			ResourceType: models.GraphResourceTypeDevice,
			Attributes:   named(asset.Name),
		})
		graph.Edges = append(graph.Edges, edgeTo(asset.ExternalRef, nodeId))
	}
	for _, sub := range zone.Zones {
		addZone(graph, taken, sub, nodeId)
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
