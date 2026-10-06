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
	"slices"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
)

const (
	// GraphAttribute says which mirror of an environment a graph is. Only the
	// meter graph carries it, so the location graph stays as it always was.
	GraphAttribute = "moses/graph"

	// MeterGraph is the value of GraphAttribute on the meter graph.
	MeterGraph = "meter"

	// ConversionAttribute marks an edge on which the medium changes, so a
	// reader knows a calorific value or an efficiency lies in between.
	ConversionAttribute = "moses/conversion"

	// MeterRootSuffix is appended to the environment name on the root node of
	// the meter graph, so the two mirrors of one site are told apart by name.
	MeterRootSuffix = " (meters)"
)

// meterNode is a device or a meter group of the meter graph, in document
// order: devices by their first carrier, then groups in list order.
type meterNode struct {
	id       string
	name     string
	device   bool
	measures bool
	// lists are the non-empty effective parent lists of a device's carriers,
	// or the parents of a group
	lists    [][]domain.MeterParent
	parents  []meterParent
	included bool
}

// meterParent is one resolved edge; target indexes the node slice.
type meterParent struct {
	target     int
	weight     int
	conversion bool
}

// BuildMeterGraph maps an environment onto its meter graph, in which an edge
// leads from a device or group to the node whose quantity contains or supplies
// it. It follows the four conventions of Build.
//
// A node whose parents cannot all be resolved hangs at the root with the whole
// flow, since dropping one parent would break the weight sum the repository
// checks. The same document always yields the same graph.
func BuildMeterGraph(env domain.Environment) models.Graph {
	graph := models.Graph{
		Id:    env.ExternalMeterGraphRef,
		Owner: env.Owner,
		Attributes: []models.Attribute{
			{Key: EnvironmentAttribute, Value: env.Id},
			{Key: GraphAttribute, Value: MeterGraph},
		},
		Nodes: []models.Node{{
			Id:         RootNodeId,
			Attributes: named(env.Name + MeterRootSuffix),
		}},
		Edges: []models.Edge{},
	}
	nodes, byId, assetIds := meterNodes(env)
	refs := externalRefs(env)
	for i := range nodes {
		nodes[i].parents = resolveMeterNode(nodes, i, byId, assetIds, refs)
	}
	// a device that measures nothing, states no parents and is nobody's parent
	// is a sensor or a machine, which is not part of a meter graph
	for i := range nodes {
		if nodes[i].measures || !nodes[i].device || len(nodes[i].lists) > 0 {
			nodes[i].included = true
		}
		for _, parent := range nodes[i].parents {
			nodes[parent.target].included = true
		}
	}
	breakMeterCycles(nodes)

	for _, node := range nodes {
		if !node.included {
			continue
		}
		mapped := models.Node{Id: node.id, Attributes: named(node.name)}
		if node.device {
			mapped.ResourceId = node.id
			mapped.ResourceType = models.GraphResourceTypeDevice
		}
		graph.Nodes = append(graph.Nodes, mapped)
	}
	for _, node := range nodes {
		if !node.included {
			continue
		}
		if len(node.parents) == 0 {
			graph.Edges = append(graph.Edges, edgeTo(node.id, RootNodeId))
			continue
		}
		for _, parent := range node.parents {
			edge := models.Edge{
				Id:         node.id + "->" + nodes[parent.target].id,
				FromNodeId: node.id,
				ToNodeId:   nodes[parent.target].id,
				Weight:     parent.weight,
			}
			if parent.conversion {
				edge.Attributes = []models.Attribute{{Key: ConversionAttribute, Value: "true"}}
			}
			graph.Edges = append(graph.Edges, edge)
		}
	}
	return graph
}

// meterNodes collects the candidate nodes and the ids of every asset. A device
// belongs to its first carrier, as in Build; a group whose id is already taken
// gets no node, so a reference to that id resolves to the node holding it.
func meterNodes(env domain.Environment) (nodes []meterNode, byId map[string]int, assetIds map[string]bool) {
	byId = map[string]int{}
	assetIds = map[string]bool{}
	var walk func(zones []domain.Zone)
	walk = func(zones []domain.Zone) {
		for _, zone := range zones {
			for _, asset := range zone.Assets {
				if asset.Id != "" {
					assetIds[asset.Id] = true
				}
				if asset.ExternalRef == "" || asset.ExternalRef == RootNodeId {
					continue
				}
				index, known := byId[asset.ExternalRef]
				if !known {
					index = len(nodes)
					byId[asset.ExternalRef] = index
					nodes = append(nodes, meterNode{id: asset.ExternalRef, name: asset.Name, device: true})
				}
				if asset.Kind == domain.AssetMeter || asset.Kind == domain.AssetInverter {
					nodes[index].measures = true
				}
				if parents := asset.EffectiveMeterParents(); len(parents) > 0 {
					nodes[index].lists = append(nodes[index].lists, parents)
				}
			}
			walk(zone.Zones)
		}
	}
	walk(env.Zones)
	for _, group := range env.MeterGroups {
		if _, taken := byId[group.Id]; taken || group.Id == "" || group.Id == RootNodeId {
			continue
		}
		byId[group.Id] = len(nodes)
		node := meterNode{id: group.Id, name: group.Name}
		if len(group.Parents) > 0 {
			node.lists = [][]domain.MeterParent{group.Parents}
		}
		nodes = append(nodes, node)
	}
	return nodes, byId, assetIds
}

// resolveMeterNode is the parents of one node, or nil for the root. Carriers
// that state nothing do not count; carriers that state different parents put
// the device at the root, since nothing ranks one statement above the other.
func resolveMeterNode(nodes []meterNode, self int, byId map[string]int, assetIds map[string]bool, refs map[string]string) []meterParent {
	var chosen []meterParent
	for i, list := range nodes[self].lists {
		resolved, ok := resolveMeterList(nodes, self, list, byId, assetIds, refs)
		if !ok {
			return nil
		}
		if i == 0 {
			chosen = resolved
		} else if !slices.Equal(chosen, resolved) {
			return nil
		}
	}
	return chosen
}

// resolveMeterList resolves every parent of one list or reports that it cannot:
// an asset parent needs a device node other than the child's own, a group parent
// a group node, and no two parents may land on the same node.
func resolveMeterList(nodes []meterNode, self int, list []domain.MeterParent, byId map[string]int, assetIds map[string]bool, refs map[string]string) ([]meterParent, bool) {
	weights, ok := domain.MeterWeights(list)
	if !ok {
		return nil, false
	}
	result := make([]meterParent, 0, len(list))
	seen := map[int]bool{}
	for i, parent := range list {
		target := -1
		if assetIds[parent.Id] {
			if index, known := byId[refs[parent.Id]]; known && nodes[index].device {
				target = index
			}
		} else if index, known := byId[parent.Id]; known && !nodes[index].device {
			target = index
		}
		if target < 0 || target == self || seen[target] {
			return nil, false
		}
		seen[target] = true
		result = append(result, meterParent{target: target, weight: weights[i], conversion: parent.Conversion})
	}
	return result, true
}

// breakMeterCycles sends nodes to the root until no cycle is left. Asset level
// cycles are refused by validation, but a device shared by several assets can
// fold acyclic statements into a cycle of devices, and the repository refuses
// a graph containing one.
//
// Depth first in document order; of each cycle found, the member earliest in
// document order loses all of its parents. Black nodes stay black across a
// break, since removing edges cannot create a cycle.
func breakMeterCycles(nodes []meterNode) {
	const white, gray, black = 0, 1, 2
	color := make([]int, len(nodes))
	position := make([]int, len(nodes))
	type frame struct{ node, next int }
	for start := range nodes {
		if color[start] != white {
			continue
		}
		color[start], position[start] = gray, 0
		stack := []frame{{node: start}}
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			parents := nodes[top.node].parents
			if top.next >= len(parents) {
				color[top.node] = black
				stack = stack[:len(stack)-1]
				continue
			}
			target := parents[top.next].target
			top.next++
			switch color[target] {
			case white:
				color[target], position[target] = gray, len(stack)
				stack = append(stack, frame{node: target})
			case gray:
				first := target
				for _, member := range stack[position[target]:] {
					first = min(first, member.node)
				}
				nodes[first].parents = nil
				// every node before start is black, so a node turned white
				// again here comes after start and the scan still reaches it
				for _, above := range stack[position[first]+1:] {
					color[above.node] = white
				}
				stack = stack[:position[first]+1]
				stack[len(stack)-1].next = 0
			}
		}
	}
}
