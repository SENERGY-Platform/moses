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

package domain

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// meterList is one list of meter parents: an asset's meter_parents or a meter
// group's parents.
type meterList struct {
	path    string
	owner   string
	group   bool
	site    int
	parents []MeterParent
}

// meterEdge is one edge of the relation the cycle check walks. fallback marks
// a submetered_by standing in for empty meter_parents.
type meterEdge struct {
	target   string
	path     string
	fallback bool
}

// maxCycleProblems bounds the cycles reported one by one; the rest are counted
// in one more problem, so an adversarial document cannot inflate the answer.
const maxCycleProblems = 100

// maxMeterProblems bounds all problems of the meter checks together; the rest
// are counted in one more problem, so the answer does not grow with the number
// of parents a document lists.
const maxMeterProblems = 1000

type meterSiteRef struct {
	path string
	site int
}

// meterLink is a group naming another group as its parent.
type meterLink struct {
	from, to, path string
}

// shortId quotes an id for a message, cut to a bounded length so that a long
// id repeated across problems cannot inflate the answer.
func shortId(id string) string {
	const limit = 64
	if len(id) <= limit {
		return strconv.Quote(id)
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(id[cut]) {
		cut--
	}
	return strconv.Quote(id[:cut]) + "..."
}

// checkMeterGraph is the second pass over meter_parents and meter_groups.
// submeterParents are the submetered_by references that passed their own
// checks; they are the meter parents of every asset without meter_parents.
func (this *validator) checkMeterGraph(env Environment, submeterParents map[string]submeterRef) {
	// already reported by Validate; the passes below are not worth running on a
	// document beyond the budget
	if this.nodes > MaxNodes {
		return
	}
	groups := map[string]bool{}
	lists := append([]meterList{}, this.meterLists...)
	for i, group := range env.MeterGroups {
		path := fmt.Sprintf("meter_groups[%d]", i)
		if group.Id == "" {
			//no id is assigned to a group: one nobody can name has no members
			this.meterFail(path+".id", "must not be empty: members and parents name a meter group by its id")
		} else {
			// still registered, so its members are not also told it does not exist
			this.checkGroupId(path+".id", group.Id)
			groups[group.Id] = true
		}
		if strings.TrimSpace(group.Name) == "" {
			this.meterFail(path+".name", "must not be empty")
		}
		if len(group.Parents) == 0 {
			this.meterFail(path+".parents", "a meter group needs at least one parent, the asset or group that supplies it")
		}
		lists = append(lists, meterList{path: path + ".parents", owner: group.Id, group: true, parents: group.Parents})
	}

	edges := map[string][]meterEdge{}
	groupParents := map[string][]meterSiteRef{}
	groupMembers := map[string][]meterSiteRef{}
	links := []meterLink{}
	explicit := map[string]bool{}
	for _, list := range lists {
		if !list.group {
			explicit[list.owner] = true
		}
		links = this.checkMeterList(list, groups, edges, groupParents, groupMembers, links)
	}
	this.checkGroupSites(env, groupParents, groupMembers, links)

	for assetId, ref := range submeterParents {
		if !explicit[assetId] {
			edges[assetId] = append(edges[assetId], meterEdge{target: ref.target, path: ref.path, fallback: true})
		}
	}
	this.checkMeterCycles(edges)
	if this.meterUnlisted > 0 {
		this.fail(this.meterFirstUnlisted, "%d further problems of meter parents and meter groups are not listed, beyond the %d above", this.meterUnlisted, maxMeterProblems)
	}
}

// checkGroupId refuses a group id that would be a second node of the meter
// graph under one id, or that would make two edge ids collide.
func (this *validator) checkGroupId(path string, id string) {
	switch {
	case strings.Contains(id, "->"):
		//edge ids are child->parent, so "a->b"->"c" and "a"->"b->c" would collide
		this.meterFail(path, "must not contain \"->\", which separates the two ends of an edge id in the meter graph")
	case id == meterRootId:
		this.meterFail(path, "must not be %q, the id of the root node of the meter graph", meterRootId)
	case this.deviceRefs[id]:
		this.meterFail(path, "must not be the external_ref of an asset: the device and the group would be one node of the meter graph")
	default:
		//claimId, but within the bound of the meter problems
		if previous, taken := this.ids[id]; taken {
			this.meterFail(path, "duplicate id %v, already used at %v", shortId(id), previous)
			return
		}
		this.ids[id] = path
	}
}

// meterFail reports a problem of the meter checks while fewer than
// maxMeterProblems were reported, and counts it otherwise.
func (this *validator) meterFail(path string, format string, args ...interface{}) {
	if this.meterProblems < maxMeterProblems {
		this.meterProblems++
		this.fail(path, format, args...)
		return
	}
	if this.meterUnlisted == 0 {
		this.meterFirstUnlisted = path
	}
	this.meterUnlisted++
}

// checkGroupSites holds every group to one top level zone, since a group adds
// up quantities of one site. A group takes the site of its first asset parent
// or member and passes it on to the groups it is linked to; every reference
// that leaves that site is refused where it is written.
func (this *validator) checkGroupSites(env Environment, groupParents map[string][]meterSiteRef, groupMembers map[string][]meterSiteRef, links []meterLink) {
	sites := map[string]int{}
	queue := []string{}
	for _, group := range env.MeterGroups {
		if _, done := sites[group.Id]; done || group.Id == "" {
			continue
		}
		refs := append(append([]meterSiteRef{}, groupParents[group.Id]...), groupMembers[group.Id]...)
		if len(refs) == 0 {
			continue
		}
		sites[group.Id] = refs[0].site
		queue = append(queue, group.Id)
		for _, ref := range refs {
			if ref.site != refs[0].site {
				this.meterFail(ref.path, "a meter group must stay within one top level zone, and %s already has a member or parent in another one (%s)", shortId(group.Id), refs[0].path)
			}
		}
	}
	linked := map[string][]string{}
	for _, link := range links {
		linked[link.from] = append(linked[link.from], link.to)
		linked[link.to] = append(linked[link.to], link.from)
	}
	for i := 0; i < len(queue); i++ {
		for _, next := range linked[queue[i]] {
			if _, known := sites[next]; !known {
				sites[next] = sites[queue[i]]
				queue = append(queue, next)
			}
		}
	}
	for _, link := range links {
		from, fromKnown := sites[link.from]
		to, toKnown := sites[link.to]
		if fromKnown && toKnown && from != to {
			this.meterFail(link.path, "a meter group must stay within one top level zone, and %s belongs to another one than %s", shortId(link.to), shortId(link.from))
		}
	}
}

// checkMeterList checks one list of parents and records the edges, group
// parents, group members and links between groups it contributes.
func (this *validator) checkMeterList(list meterList, groups map[string]bool, edges map[string][]meterEdge,
	groupParents map[string][]meterSiteRef, groupMembers map[string][]meterSiteRef, links []meterLink) []meterLink {
	if len(list.parents) > MaxMeterParents {
		this.meterFail(list.path, "at most %d parents, got %d: an equal split over more would give some of them a weight of 0", MaxMeterParents, len(list.parents))
		return links
	}
	if _, ok := MeterWeights(list.parents); !ok {
		this.meterFail(list.path, "the weights must either all be omitted, for an equal split, or all lie between 1 and 100 and sum to exactly 100")
	}
	seen := map[string]int{}
	for j, parent := range list.parents {
		path := fmt.Sprintf("%s[%d]", list.path, j)
		if parent.Id == "" {
			this.meterFail(path, "must name an asset or a meter group by its id")
			continue
		}
		if list.owner != "" && parent.Id == list.owner {
			this.meterFail(path, "cannot name itself as its own meter parent")
			continue
		}
		if first, duplicate := seen[parent.Id]; duplicate {
			this.meterFail(path, "%s is already named at %s[%d], and two parents cannot be one node", shortId(parent.Id), list.path, first)
			continue
		}
		seen[parent.Id] = j
		site, isAsset := this.assetSites[parent.Id]
		switch {
		case isAsset && list.group:
			if list.owner != "" {
				groupParents[list.owner] = append(groupParents[list.owner], meterSiteRef{path: path, site: site})
			}
		case isAsset:
			if site != list.site {
				this.meterFail(path, "a meter parent must stay within the same top level zone: a meter tree is modelled per site, as with submetered_by")
				continue
			}
		case groups[parent.Id]:
			if !list.group {
				groupMembers[parent.Id] = append(groupMembers[parent.Id], meterSiteRef{path: path, site: list.site})
			} else if list.owner != "" {
				links = append(links, meterLink{from: list.owner, to: parent.Id, path: path})
			}
		default:
			this.meterFail(path, "the referenced asset or meter group %s does not exist in this environment", shortId(parent.Id))
			continue
		}
		if list.owner != "" {
			edges[list.owner] = append(edges[list.owner], meterEdge{target: parent.Id, path: path})
		}
	}
	return links
}

// checkMeterCycles refuses a cycle over the meter parents of assets and
// groups, reported at the edge that closes it. A cycle made of submetered_by
// alone is left to checkSubmeterCycles, which already reports it.
//
// Three-colour depth first search over several parents per node. The number of
// explicit edges on the current path is kept as a prefix count, so each closing
// edge is judged in constant time and no cycle is spelled out in full.
func (this *validator) checkMeterCycles(edges map[string][]meterEdge) {
	const white, gray, black = 0, 1, 2
	color := map[string]int{}
	depth := map[string]int{}
	// explicitBefore[d] counts the edges that are no fallback among the first d
	// edges of the current path
	explicitBefore := []int{0}
	reported, unlisted, firstUnlisted := 0, 0, ""

	var walk func(id string)
	walk = func(id string) {
		color[id] = gray
		depth[id] = len(explicitBefore) - 1
		for _, edge := range edges[id] {
			explicit := explicitBefore[len(explicitBefore)-1]
			if !edge.fallback {
				explicit++
			}
			switch color[edge.target] {
			case white:
				explicitBefore = append(explicitBefore, explicit)
				walk(edge.target)
				explicitBefore = explicitBefore[:len(explicitBefore)-1]
			case gray:
				onCycle := explicit - explicitBefore[depth[edge.target]]
				if onCycle == 0 {
					continue
				}
				if reported == maxCycleProblems {
					if unlisted == 0 {
						firstUnlisted = edge.path
					}
					unlisted++
					continue
				}
				reported++
				message := "meter parents form a cycle: %s leads back to %s"
				if length := len(explicitBefore) - depth[edge.target]; onCycle < length {
					message = "meter parents form a cycle, with submetered_by standing in where meter_parents is empty: %s leads back to %s"
				}
				this.meterFail(edge.path, message, shortId(id), shortId(edge.target))
			}
		}
		color[id] = black
	}

	// sorted for the same reason as in checkSubmeterCycles: the same document
	// has to report the same cycles on every save
	ids := make([]string, 0, len(edges))
	for id := range edges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if color[id] == white {
			walk(id)
		}
	}
	if unlisted > 0 {
		this.meterFail(firstUnlisted, "%d further cycles of meter parents are not listed, beyond the %d above", unlisted, maxCycleProblems)
	}
}
