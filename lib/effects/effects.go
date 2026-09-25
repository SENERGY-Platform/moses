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

// Package effects derives from an environment document which context keys,
// assets and zones drive which others. It is a pure function of the document:
// nothing is run, and what cannot be read off the document is reported as
// unresolved instead of guessed. See docs/effects.md.
package effects

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/formula"
)

type NodeKind string

const (
	NodeContextKey NodeKind = "context_key"
	NodeAsset      NodeKind = "asset"
	NodeZone       NodeKind = "zone"
	NodeTimeline   NodeKind = "timeline"
)

type EdgeKind string

const (
	EdgeReads       EdgeKind = "reads"
	EdgeWrites      EdgeKind = "writes"
	EdgeGates       EdgeKind = "gates"
	EdgeScales      EdgeKind = "scales"
	EdgeSubmeters   EdgeKind = "submeters"
	EdgeAggregates  EdgeKind = "aggregates"
	EdgeDatedChange EdgeKind = "dated_change"
)

type Via string

const (
	ViaScript       Via = "script"
	ViaFormula      Via = "formula"
	ViaSchedule     Via = "schedule"
	ViaAggregate    Via = "aggregate"
	ViaSubmeteredBy Via = "submetered_by"
	ViaTimeline     Via = "timeline"
)

// TimelineNodeId is the one node every dated change starts from.
const TimelineNodeId = "timeline"

// Graph is the effect graph of one environment. Edges point in the direction
// data flows, from the producer to the consumer.
type Graph struct {
	Nodes      []Node       `json:"nodes"`
	Edges      []Edge       `json:"edges"`
	Unresolved []Unresolved `json:"unresolved"`
}

// Node is a context key, an asset, a zone or the timeline. The pointer fields
// belong to one kind each and are left out for every other kind.
type Node struct {
	Id    string   `json:"id" example:"asset:a-pv-a"`
	Kind  NodeKind `json:"kind" enums:"context_key,asset,zone,timeline" example:"asset"`
	Label string   `json:"label" example:"PV-Wechselrichter"`

	// Static is set on a context key: true when the key is declared in context
	// and no context source drives it.
	Static *bool `json:"static,omitempty" example:"false"`
	// SourceKind is set on a context key: the kind of the context source driving
	// it, empty when none does.
	SourceKind *string `json:"source_kind,omitempty" enums:"profile,dataset," example:"dataset"`
	// ExternalRef is set on a context key: the ref of a dataset source, else empty.
	ExternalRef *string `json:"external_ref,omitempty" example:""`

	// Zone is set on an asset (the zone it sits in) and on a zone (its own id).
	Zone *string `json:"zone,omitempty" example:"z-standort-a"`
	// Site is set on an asset and on a zone: the top level zone above it.
	Site *string `json:"site,omitempty" example:"z-standort-a"`
	// AssetKind is set on an asset.
	AssetKind *string `json:"asset_kind,omitempty" example:"inverter"`
}

// Edge is one effect. Count is how many references in the document make it;
// two edges that agree in every other field are merged by adding their counts.
type Edge struct {
	From string   `json:"from" example:"asset:a-pv-a"`
	To   string   `json:"to" example:"asset:a-speicher-a"`
	Kind EdgeKind `json:"kind" enums:"reads,writes,gates,scales,submeters,aggregates,dated_change" example:"reads"`
	Via  Via      `json:"via" enums:"script,formula,schedule,aggregate,submetered_by,timeline" example:"script"`
	// Channel is the channel whose source makes the edge: the reading or writing
	// script or formula, the gated or scaled schedule, the aggregate, the channel
	// a dated change targets. Empty for submetered_by and context targets.
	Channel string `json:"channel" example:"ch-a-bat-soc"`
	// Key is the state or context key that is read or written, the channel id a
	// formula reads, or the target of a dated change.
	Key   string `json:"key" example:"pv_w"`
	Count int    `json:"count" example:"1"`
}

// Unresolved is a reference the document makes that could not be turned into
// an edge without guessing.
type Unresolved struct {
	Asset      string `json:"asset" example:"a-rlm-a"`
	Channel    string `json:"channel" example:"ch-a-rlm-a-180"`
	Expression string `json:"expression" example:"E.get(k + '_v')"`
	Reason     string `json:"reason" example:"the key is computed at run time"`
}

type edgeKey struct {
	from, to string
	kind     EdgeKind
	via      Via
	channel  string
	key      string
}

func (this edgeKey) less(other edgeKey) bool {
	return edgeLess(Edge{From: this.from, To: this.to, Kind: this.kind, Via: this.via, Channel: this.channel, Key: this.key},
		Edge{From: other.from, To: other.to, Kind: other.kind, Via: other.via, Channel: other.channel, Key: other.key})
}

// limits bound what one document can make the derivation do: a request must
// not be able to allocate without bound. A variable so tests can lower it.
var limits = struct {
	rowsPerScript, rowsPerDocument             int
	edgesPerScript, edgesPerDocument           int
	unresolvedPerScript, unresolvedPerDocument int
}{
	rowsPerScript: 50000, rowsPerDocument: 500000,
	edgesPerScript: 10000, edgesPerDocument: 100000,
	unresolvedPerScript: 500, unresolvedPerDocument: 10000,
}

type zoneEntry struct {
	id, name, site string
}

type assetEntry struct {
	asset  domain.Asset
	zoneId string
	site   string
}

// builder indexes the document the way the runtime does (lib/runtime,
// newGeneration): zones without an id, duplicate ids and zones below
// MaxZoneDepth are skipped with everything they contain, so the graph shows
// what actually runs.
type builder struct {
	env          domain.Environment
	zones        map[string]*zoneEntry
	assets       map[string]*assetEntry
	assetOrder   []*assetEntry
	channelOwner map[string]string

	// governed are the context keys a dated change targets; the runtime drops a
	// script's set on them (jsContextStateApi).
	governed map[string]bool

	contextRefs map[string]bool
	zoneRefs    map[string]bool
	edges       map[edgeKey]int
	unresolved  []Unresolved

	// counted is the number of unresolved entries, without the truncation
	// entries; rows the table rows resolved so far.
	counted   int
	rows      int
	truncated bool
}

func newBuilder(env domain.Environment) *builder {
	result := &builder{
		env:          env,
		zones:        map[string]*zoneEntry{},
		assets:       map[string]*assetEntry{},
		channelOwner: map[string]string{},
		contextRefs:  map[string]bool{},
		zoneRefs:     map[string]bool{},
		edges:        map[edgeKey]int{},
		governed:     map[string]bool{},
	}
	result.addZones(env.Zones, 1, "")
	for _, change := range env.Timeline {
		if target, err := domain.ParseTimelineTarget(change.Target); err == nil && target.Kind == domain.TimelineContext {
			result.governed[target.Ref] = true
		}
	}
	return result
}

func (this *builder) addZones(zones []domain.Zone, depth int, site string) {
	if depth > domain.MaxZoneDepth {
		return
	}
	for _, zone := range zones {
		if zone.Id == "" {
			continue
		}
		if _, duplicate := this.zones[zone.Id]; duplicate {
			continue
		}
		zoneSite := site
		if depth == 1 {
			zoneSite = zone.Id
		}
		this.zones[zone.Id] = &zoneEntry{id: zone.Id, name: zone.Name, site: zoneSite}
		for _, asset := range zone.Assets {
			if asset.Id == "" {
				continue
			}
			if _, duplicate := this.assets[asset.Id]; duplicate {
				continue
			}
			entry := &assetEntry{asset: asset, zoneId: zone.Id, site: zoneSite}
			this.assets[asset.Id] = entry
			this.assetOrder = append(this.assetOrder, entry)
			for _, channel := range asset.Channels {
				if channel.Id == "" {
					continue
				}
				if _, known := this.channelOwner[channel.Id]; !known {
					this.channelOwner[channel.Id] = asset.Id
				}
			}
		}
		this.addZones(zone.Zones, depth+1, zoneSite)
	}
}

// Derive computes the effect graph of env. It never fails: a reference it
// cannot resolve becomes an entry in Unresolved, and the rest is still derived.
func Derive(env domain.Environment) Graph {
	b := newBuilder(env)
	for _, entry := range b.assetOrder {
		b.submeterEdge(entry)
		for _, channel := range entry.asset.Channels {
			switch channel.Source.Kind {
			case domain.SourceScript:
				if channel.Source.Script != nil {
					b.scriptEdges(entry, channel)
				}
			case domain.SourceFormula:
				if channel.Source.Formula != nil {
					b.formulaEdges(entry, channel)
				}
			case domain.SourceSchedule:
				if channel.Source.Schedule != nil {
					b.scheduleEdges(entry, channel)
				}
			}
		}
	}
	for _, link := range aggregateLinks(b) {
		b.addEdge(assetNodeId(link.child), assetNodeId(link.meter), EdgeAggregates, ViaAggregate, link.channel, "", 1)
	}
	b.timelineEdges()
	return b.graph()
}

func contextNodeId(key string) string { return "context:" + key }
func assetNodeId(id string) string    { return "asset:" + id }
func zoneNodeId(id string) string     { return "zone:" + id }

// addEdge merges an edge in, and registers the context key or zone an end of it
// names so that it becomes a node. Past the document cap it adds nothing new.
func (this *builder) addEdge(from, to string, kind EdgeKind, via Via, channel, key string, count int) {
	edge := edgeKey{from: from, to: to, kind: kind, via: via, channel: channel, key: key}
	if _, known := this.edges[edge]; !known {
		if len(this.edges) >= limits.edgesPerDocument {
			this.truncate()
			return
		}
		for _, end := range []string{from, to} {
			if name, ok := strings.CutPrefix(end, "context:"); ok {
				this.contextRefs[name] = true
			} else if id, ok := strings.CutPrefix(end, "zone:"); ok {
				this.zoneRefs[id] = true
			}
		}
	}
	this.edges[edge] += count
}

func (this *builder) truncate() {
	if this.truncated {
		return
	}
	this.truncated = true
	this.unresolved = append(this.unresolved, Unresolved{Reason: fmt.Sprintf(
		"the document has more effects than the analysis reports (at most %d edges and %d unresolved entries), the rest is left out",
		limits.edgesPerDocument, limits.unresolvedPerDocument)})
}

func (this *builder) addUnresolved(entry Unresolved) {
	if this.counted >= limits.unresolvedPerDocument {
		this.truncate()
		return
	}
	this.counted++
	this.unresolved = append(this.unresolved, entry)
}

func (this *builder) unresolvedAt(asset, channel, expression, reason string) {
	this.addUnresolved(Unresolved{Asset: asset, Channel: channel, Expression: snippet(expression), Reason: reason})
}

func (this *builder) submeterEdge(entry *assetEntry) {
	meter := entry.asset.SubmeteredBy
	if meter == "" || meter == entry.asset.Id {
		return
	}
	if _, known := this.assets[meter]; !known {
		this.unresolvedAt(entry.asset.Id, "", meter, "submetered_by names no asset of the document")
		return
	}
	this.addEdge(assetNodeId(entry.asset.Id), assetNodeId(meter), EdgeSubmeters, ViaSubmeteredBy, "", "", 1)
}

// formulaEdges mirrors how the runtime resolves an input (resolveInput):
// zone.<k> is the state of the asset's own zone, asset.<k> the asset's own
// state and therefore no edge.
func (this *builder) formulaEdges(entry *assetEntry, channel domain.Channel) {
	self := assetNodeId(entry.asset.Id)
	names := make([]string, 0, len(channel.Source.Formula.Inputs))
	for name := range channel.Source.Formula.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ref := channel.Source.Formula.Inputs[name]
		if key, ok := strings.CutPrefix(ref, formula.RefContext); ok && key != "" {
			this.addEdge(contextNodeId(key), self, EdgeReads, ViaFormula, channel.Id, key, 1)
			continue
		}
		if key, ok := strings.CutPrefix(ref, formula.RefZone); ok && key != "" {
			this.addEdge(zoneNodeId(entry.zoneId), self, EdgeReads, ViaFormula, channel.Id, key, 1)
			continue
		}
		if key, ok := strings.CutPrefix(ref, formula.RefAsset); ok && key != "" {
			continue
		}
		if id, ok := strings.CutPrefix(ref, formula.RefChannel); ok && id != "" {
			owner, known := this.channelOwner[id]
			if !known {
				this.unresolvedAt(entry.asset.Id, channel.Id, ref, "the formula reads a channel the document does not carry")
				continue
			}
			if owner == entry.asset.Id {
				continue
			}
			this.addEdge(assetNodeId(owner), self, EdgeReads, ViaFormula, channel.Id, id, 1)
			continue
		}
		this.unresolvedAt(entry.asset.Id, channel.Id, ref, "the formula input names no context, zone, asset or channel")
	}
}

func (this *builder) scheduleEdges(entry *assetEntry, channel domain.Channel) {
	self := assetNodeId(entry.asset.Id)
	schedule := channel.Source.Schedule
	if schedule.Gate != nil && schedule.Gate.ContextKey != "" {
		key := schedule.Gate.ContextKey
		this.addEdge(contextNodeId(key), self, EdgeGates, ViaSchedule, channel.Id, key, 1)
	}
	if schedule.ScaleBy != "" {
		this.addEdge(contextNodeId(schedule.ScaleBy), self, EdgeScales, ViaSchedule, channel.Id, schedule.ScaleBy, 1)
	}
}

func (this *builder) timelineEdges() {
	for _, change := range this.env.Timeline {
		target, err := domain.ParseTimelineTarget(change.Target)
		if err != nil {
			this.unresolvedAt("", "", change.Target, "the timeline target is none of the forms docs/dated-changes.md lists")
			continue
		}
		switch target.Kind {
		case domain.TimelineChannel:
			owner, known := this.channelOwner[target.Ref]
			if !known {
				this.unresolvedAt("", target.Ref, change.Target, "the timeline targets a channel the document does not carry")
				continue
			}
			this.addEdge(TimelineNodeId, assetNodeId(owner), EdgeDatedChange, ViaTimeline, target.Ref, change.Target, 1)
		case domain.TimelineContext, domain.TimelineContextSource:
			this.addEdge(TimelineNodeId, contextNodeId(target.Ref), EdgeDatedChange, ViaTimeline, "", change.Target, 1)
		}
	}
}

func (this *builder) graph() Graph {
	result := Graph{Nodes: []Node{}, Edges: []Edge{}, Unresolved: this.unresolved}
	if result.Unresolved == nil {
		result.Unresolved = []Unresolved{}
	}

	keys := map[string]bool{}
	for key := range this.env.Context {
		keys[key] = true
	}
	for key := range this.env.ContextSources {
		keys[key] = true
	}
	for key := range this.contextRefs {
		keys[key] = true
	}
	for key := range keys {
		_, declared := this.env.Context[key]
		source, driven := this.env.ContextSources[key]
		static := declared && !driven
		sourceKind, externalRef := "", ""
		if driven {
			switch source.Kind {
			case domain.SourceProfile:
				sourceKind = string(domain.SourceProfile)
			case domain.SourceDataset:
				sourceKind = string(domain.SourceDataset)
				if source.Dataset != nil {
					externalRef = source.Dataset.Ref
				}
			}
		}
		result.Nodes = append(result.Nodes, Node{
			Id: contextNodeId(key), Kind: NodeContextKey, Label: key,
			Static: &static, SourceKind: &sourceKind, ExternalRef: &externalRef,
		})
	}
	for _, entry := range this.assetOrder {
		zone, site, kind := entry.zoneId, entry.site, string(entry.asset.Kind)
		result.Nodes = append(result.Nodes, Node{
			Id: assetNodeId(entry.asset.Id), Kind: NodeAsset, Label: entry.asset.Name,
			Zone: &zone, Site: &site, AssetKind: &kind,
		})
	}
	for id := range this.zoneRefs {
		entry := this.zones[id]
		zone, site := entry.id, entry.site
		result.Nodes = append(result.Nodes, Node{
			Id: zoneNodeId(id), Kind: NodeZone, Label: entry.name, Zone: &zone, Site: &site,
		})
	}
	if len(this.env.Timeline) > 0 {
		result.Nodes = append(result.Nodes, Node{Id: TimelineNodeId, Kind: NodeTimeline, Label: "Dated changes"})
	}
	sort.Slice(result.Nodes, func(i, j int) bool { return result.Nodes[i].Id < result.Nodes[j].Id })

	for key, count := range this.edges {
		result.Edges = append(result.Edges, Edge{
			From: key.from, To: key.to, Kind: key.kind, Via: key.via,
			Channel: key.channel, Key: key.key, Count: count,
		})
	}
	sort.Slice(result.Edges, func(i, j int) bool { return edgeLess(result.Edges[i], result.Edges[j]) })
	return result
}

func edgeLess(a, b Edge) bool {
	for _, pair := range [][2]string{
		{a.From, b.From}, {a.To, b.To}, {string(a.Kind), string(b.Kind)},
		{string(a.Via), string(b.Via)}, {a.Channel, b.Channel}, {a.Key, b.Key},
	} {
		if pair[0] != pair[1] {
			return pair[0] < pair[1]
		}
	}
	return false
}

// maxSnippet bounds the expression of an unresolved entry, counted in runes so
// a cut never splits a character.
const maxSnippet = 80

func snippet(source string) string {
	collapsed := strings.Join(strings.Fields(source), " ")
	runes := []rune(collapsed)
	if len(runes) <= maxSnippet {
		return collapsed
	}
	return string(runes[:maxSnippet-3]) + "..."
}

func describeRow(row int) string {
	return fmt.Sprintf("row %d of the table", row)
}
