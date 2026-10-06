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
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// meterEnvironment is two sites: site A with a main meter, a PV inverter, two
// sub-meters and a machine in a hall below, site B with a main meter of its own.
//
//	zones[0].assets[0] asset-meter (from validEnvironment, carries channel-energy)
//	zones[0].assets[1] a-pv
//	zones[0].assets[2] a-unter-1
//	zones[0].assets[3] a-unter-2
//	zones[0].zones[0].assets[0] a-maschine
//	zones[1].assets[0] b-haupt
func meterEnvironment() Environment {
	env := validEnvironment()
	asset := func(id string, kind AssetKind) Asset {
		return Asset{Id: id, Name: id, Kind: kind, ExternalTypeId: "urn:infai:ses:device-type:abc"}
	}
	env.Zones[0].Type = ZoneSite
	env.Zones[0].Assets = append(env.Zones[0].Assets,
		asset("a-pv", AssetInverter), asset("a-unter-1", AssetMeter), asset("a-unter-2", AssetMeter))
	env.Zones[0].Zones = []Zone{{Id: "zone-a-halle", Name: "Halle", Type: ZoneHall,
		Assets: []Asset{asset("a-maschine", AssetMachine)}}}
	env.Zones = append(env.Zones, Zone{Id: "zone-b", Name: "Werk B", Type: ZoneSite,
		Assets: []Asset{asset("b-haupt", AssetMeter)}})
	return env
}

func parentsOf(ids ...string) []MeterParent {
	result := []MeterParent{}
	for _, id := range ids {
		result = append(result, MeterParent{Id: id})
	}
	return result
}

const (
	pathPv       = "zones[0].assets[1]"
	pathUnter1   = "zones[0].assets[2]"
	pathUnter2   = "zones[0].assets[3]"
	pathMaschine = "zones[0].zones[0].assets[0]"
	pathBHaupt   = "zones[1].assets[0]"
)

func TestValidateMeterParents(t *testing.T) {
	cases := []struct {
		name string
		edit func(env *Environment)
		// want is every problem path, sorted, or nil for a valid document
		want []string
	}{
		{"explicit weights summing to 100", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = []MeterParent{{Id: "asset-meter", Weight: 70}, {Id: "a-pv", Weight: 30, Conversion: true}}
		}, nil},
		{"omitted weights", func(env *Environment) {
			env.Zones[0].Assets[3].MeterParents = parentsOf("asset-meter", "a-pv", "a-unter-1")
		}, nil},
		{"a forward reference and a group", func(env *Environment) {
			env.Zones[0].Assets[0].MeterParents = parentsOf("a-maschine")
			env.Zones[0].Assets[2].MeterParents = parentsOf("abgaenge")
			env.MeterGroups = []MeterGroup{{Id: "abgaenge", Name: "Abgänge", Parents: parentsOf("a-pv")}}
		}, nil},
		{"a group supplied by a group", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = parentsOf("verteilung")
			env.MeterGroups = []MeterGroup{
				{Id: "verteilung", Name: "Verteilung", Parents: parentsOf("einspeisung")},
				{Id: "einspeisung", Name: "Einspeisung", Parents: parentsOf("asset-meter", "a-pv")},
			}
		}, nil},
		{"meter_parents next to a different submetered_by", func(env *Environment) {
			env.Zones[0].Assets[3].SubmeteredBy = "asset-meter"
			env.Zones[0].Assets[3].MeterParents = parentsOf("a-unter-1")
		}, nil},
		// unter-1's submetered_by would close a cycle with unter-2, but its
		// meter_parents replace it in the meter graph
		{"a submetered_by overridden by meter_parents closes no cycle", func(env *Environment) {
			env.Zones[0].Assets[2].SubmeteredBy = "a-unter-2"
			env.Zones[0].Assets[2].MeterParents = parentsOf("asset-meter")
			env.Zones[0].Assets[3].MeterParents = parentsOf("a-unter-1")
		}, nil},
		{"an empty parent id", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = []MeterParent{{}}
		}, []string{pathUnter1 + ".meter_parents[0]"}},
		{"an unknown parent", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = parentsOf("asset-meter", "gibt-es-nicht")
		}, []string{pathUnter1 + ".meter_parents[1]"}},
		{"a zone as parent", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = parentsOf("zone-hall")
		}, []string{pathUnter1 + ".meter_parents[0]"}},
		{"a channel as parent", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = parentsOf("channel-energy")
		}, []string{pathUnter1 + ".meter_parents[0]"}},
		{"itself", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = parentsOf("asset-meter", "a-unter-1")
		}, []string{pathUnter1 + ".meter_parents[1]"}},
		{"a parent named twice", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = parentsOf("asset-meter", "a-pv", "asset-meter")
		}, []string{pathUnter1 + ".meter_parents[2]"}},
		{"a parent in another top level zone", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = parentsOf("b-haupt")
		}, []string{pathUnter1 + ".meter_parents[0]"}},
		{"weights partly omitted", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = []MeterParent{{Id: "asset-meter", Weight: 100}, {Id: "a-pv"}}
		}, []string{pathUnter1 + ".meter_parents"}},
		{"weights summing to 90", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = []MeterParent{{Id: "asset-meter", Weight: 60}, {Id: "a-pv", Weight: 30}}
		}, []string{pathUnter1 + ".meter_parents"}},
		{"weights summing to 110", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = []MeterParent{{Id: "asset-meter", Weight: 60}, {Id: "a-pv", Weight: 50}}
		}, []string{pathUnter1 + ".meter_parents"}},
		{"a weight out of range", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = []MeterParent{{Id: "asset-meter", Weight: 101}, {Id: "a-pv", Weight: -1}}
		}, []string{pathUnter1 + ".meter_parents"}},
		{"more than 100 parents", func(env *Environment) {
			parents := []MeterParent{}
			for i := 0; i <= MaxMeterParents; i++ {
				parents = append(parents, MeterParent{Id: fmt.Sprintf("teil-%d", i)})
			}
			env.Zones[0].Assets[2].MeterParents = parents
		}, []string{pathUnter1 + ".meter_parents"}},
		{"a group without an id", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Name: "Abgänge", Parents: parentsOf("a-pv")}}
		}, []string{"meter_groups[0].id"}},
		{"a group without a name", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "abgaenge", Name: " ", Parents: parentsOf("a-pv")}}
		}, []string{"meter_groups[0].name"}},
		{"a group without parents", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "abgaenge", Name: "Abgänge"}}
		}, []string{"meter_groups[0].parents"}},
		{"two groups with one id", func(env *Environment) {
			env.MeterGroups = []MeterGroup{
				{Id: "abgaenge", Name: "Abgänge", Parents: parentsOf("a-pv")},
				{Id: "abgaenge", Name: "Abgänge 2", Parents: parentsOf("asset-meter")},
			}
		}, []string{"meter_groups[1].id"}},
		{"a group with the id of an asset", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "a-unter-2", Name: "Abgänge", Parents: parentsOf("a-pv")}}
		}, []string{"meter_groups[0].id"}},
		{"a group with the id of a zone", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "zone-b", Name: "Abgänge", Parents: parentsOf("a-pv")}}
		}, []string{"meter_groups[0].id"}},
		{"a group naming itself", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "abgaenge", Name: "Abgänge", Parents: parentsOf("a-pv", "abgaenge")}}
		}, []string{"meter_groups[0].parents[1]"}},
		{"a group parent named twice", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "abgaenge", Name: "Abgänge", Parents: parentsOf("a-pv", "a-pv")}}
		}, []string{"meter_groups[0].parents[1]"}},
		{"group weights not summing to 100", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "abgaenge", Name: "Abgänge",
				Parents: []MeterParent{{Id: "a-pv", Weight: 50}, {Id: "asset-meter", Weight: 49}}}}
		}, []string{"meter_groups[0].parents"}},
		{"a group parent in another top level zone than the others", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "abgaenge", Name: "Abgänge", Parents: parentsOf("a-pv", "b-haupt")}}
		}, []string{"meter_groups[0].parents[1]"}},
		{"a group member in another top level zone than its parents", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "abgaenge", Name: "Abgänge", Parents: parentsOf("a-pv")}}
			env.Zones[0].Assets[2].MeterParents = parentsOf("abgaenge")
			env.Zones[1].Assets[0].MeterParents = parentsOf("abgaenge")
		}, []string{pathBHaupt + ".meter_parents[0]"}},
		{"group members in two top level zones", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "summe", Name: "Summe", Parents: parentsOf("nur-gruppe")},
				{Id: "nur-gruppe", Name: "Nur Gruppe", Parents: parentsOf("a-pv")}}
			env.Zones[0].Assets[2].MeterParents = parentsOf("summe")
			env.Zones[1].Assets[0].MeterParents = parentsOf("summe")
		}, []string{pathBHaupt + ".meter_parents[0]"}},
		{"a cycle of meter parents", func(env *Environment) {
			env.Zones[0].Assets[2].MeterParents = parentsOf("a-unter-2")
			env.Zones[0].Assets[3].MeterParents = parentsOf("asset-meter", "a-unter-1")
		}, []string{pathUnter2 + ".meter_parents[1]"}},
		{"a cycle through a group", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "abgaenge", Name: "Abgänge", Parents: parentsOf("a-pv", "a-unter-1")}}
			env.Zones[0].Assets[2].MeterParents = parentsOf("abgaenge")
		}, []string{"meter_groups[0].parents[1]"}},
		// the main meter is placed under its own sub-meter, which submetered_by
		// still hangs under the main meter
		{"a cycle closed by submetered_by", func(env *Environment) {
			env.Zones[0].Assets[2].SubmeteredBy = "asset-meter"
			env.Zones[0].Assets[0].MeterParents = parentsOf("a-unter-1")
		}, []string{"zones[0].assets[0].meter_parents[0]"}},
		// submetered_by is overridden by meter_parents and so takes no part
		{"a submetered_by cycle broken by meter_parents is still one of submetered_by", func(env *Environment) {
			env.Zones[0].Assets[2].SubmeteredBy = "a-unter-2"
			env.Zones[0].Assets[3].SubmeteredBy = "a-unter-1"
			env.Zones[0].Assets[3].MeterParents = parentsOf("asset-meter")
		}, []string{pathUnter1 + ".submetered_by", pathUnter2 + ".submetered_by"}},
		// reported once, by the submetered_by check
		{"a cycle of submetered_by alone", func(env *Environment) {
			env.Zones[0].Assets[2].SubmeteredBy = "a-unter-2"
			env.Zones[0].Assets[3].SubmeteredBy = "a-unter-1"
		}, []string{pathUnter1 + ".submetered_by", pathUnter2 + ".submetered_by"}},
		// groups share the node budget of zones, assets and channels
		{"more groups than the node budget leaves", func(env *Environment) {
			for i := 0; i < MaxNodes; i++ {
				env.MeterGroups = append(env.MeterGroups, MeterGroup{Id: fmt.Sprintf("g-%d", i), Name: "G", Parents: parentsOf("a-pv")})
			}
		}, []string{""}},
		// "a->b" + "->" + "c" and "a" + "->" + "b->c" are one edge id
		{"group ids that would make edge ids collide", func(env *Environment) {
			env.MeterGroups = []MeterGroup{
				{Id: "c", Name: "C", Parents: parentsOf("a-pv")},
				{Id: "b->c", Name: "BC", Parents: parentsOf("a-pv")},
				{Id: "a->b", Name: "AB", Parents: parentsOf("c")},
				{Id: "a", Name: "A", Parents: parentsOf("b->c")},
			}
		}, []string{"meter_groups[1].id", "meter_groups[2].id"}},
		{"a group with the id of a device", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "urn:infai:ses:device:abc", Name: "Abgänge", Parents: parentsOf("a-pv")}}
		}, []string{"meter_groups[0].id"}},
		{"a group with the id of the root node", func(env *Environment) {
			env.MeterGroups = []MeterGroup{{Id: "root", Name: "Abgänge", Parents: parentsOf("a-pv")}}
		}, []string{"meter_groups[0].id"}},
		{"a group of one site supplied by a group of another", func(env *Environment) {
			env.MeterGroups = []MeterGroup{
				{Id: "g-a", Name: "Abgänge A", Parents: parentsOf("a-pv")},
				{Id: "g-b-sub", Name: "Unterverteilung B", Parents: parentsOf("g-a")},
			}
			env.Zones[1].Assets[0].MeterParents = parentsOf("g-b-sub")
		}, []string{"meter_groups[1].parents[0]"}},
		{"a group of one site that is a member of a group of another", func(env *Environment) {
			env.MeterGroups = []MeterGroup{
				{Id: "g-b", Name: "Summe B", Parents: parentsOf("b-haupt")},
				{Id: "g-a", Name: "Abgänge A", Parents: parentsOf("g-b")},
			}
			env.Zones[0].Assets[2].MeterParents = parentsOf("g-a")
		}, []string{"meter_groups[1].parents[0]"}},
		// g-mid has no asset of its own; it takes the site of g-a, and the
		// reference from site B's group is the one that crosses
		{"a chain of groups crossing sites", func(env *Environment) {
			env.MeterGroups = []MeterGroup{
				{Id: "g-a", Name: "Abgänge A", Parents: parentsOf("a-pv")},
				{Id: "g-mid", Name: "Mitte", Parents: parentsOf("g-a")},
				{Id: "g-b-sub", Name: "Unterverteilung B", Parents: parentsOf("g-mid")},
			}
			env.Zones[1].Assets[0].MeterParents = parentsOf("g-b-sub")
		}, []string{"meter_groups[2].parents[0]"}},
		{"a chain of groups within one site", func(env *Environment) {
			env.MeterGroups = []MeterGroup{
				{Id: "g-a", Name: "Abgänge A", Parents: parentsOf("a-pv")},
				{Id: "g-mid", Name: "Mitte", Parents: parentsOf("g-a")},
				{Id: "g-sub", Name: "Unterverteilung", Parents: parentsOf("g-mid")},
			}
			env.Zones[0].Assets[2].MeterParents = parentsOf("g-sub")
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := meterEnvironment()
			c.edit(&env)
			err := Validate(env)
			if c.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if got := problemPaths(t, err); !reflect.DeepEqual(got, c.want) {
				t.Errorf("expected problems at %v, got %v", c.want, err)
			}
		})
	}
}

// Some refusals would also be caught by a later rule at the same path; the
// message says which rule it was.
func TestValidateMeterParentsSaysWhy(t *testing.T) {
	for _, c := range []struct {
		name    string
		parents []MeterParent
		message string
	}{
		{"empty id", []MeterParent{{}}, "must name an asset or a meter group"},
		{"itself", parentsOf("a-unter-1"), "cannot name itself"},
		{"twice", parentsOf("a-pv", "a-pv"), "is already named at"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := meterEnvironment()
			env.Zones[0].Assets[2].MeterParents = c.parents
			err := Validate(env)
			if err == nil || !strings.Contains(err.Error(), c.message) {
				t.Errorf("expected %q, got %v", c.message, err)
			}
		})
	}
}

// A document of many groups each naming 99 others is a cycle on nearly every
// edge. The answer has to stay small and quick to compute: every problem is
// held in memory and sent back in one 400.
func TestMeterCyclesStayBoundedOnAnAdversarialDocument(t *testing.T) {
	env := meterEnvironment()
	const groups, parents = 1000, 99
	long := strings.Repeat("x", 4000)
	for i := 0; i < groups; i++ {
		list := []MeterParent{}
		for k := 1; k <= parents; k++ {
			list = append(list, MeterParent{Id: fmt.Sprintf("g-%d-%s", (i+k)%groups, long)})
		}
		env.MeterGroups = append(env.MeterGroups, MeterGroup{Id: fmt.Sprintf("g-%d-%s", i, long), Name: "G", Parents: list})
	}
	runtime.GC()
	before := runtime.MemStats{}
	runtime.ReadMemStats(&before)
	started := time.Now()

	err := Validate(env)

	elapsed := time.Since(started)
	after := runtime.MemStats{}
	runtime.ReadMemStats(&after)
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected the cycles to be refused, got %v", err)
	}
	size := 0
	for _, problem := range invalid.Problems {
		size += len(problem.Path) + len(problem.Message)
	}
	t.Logf("%d problems, %d bytes, %d MiB allocated, %v", len(invalid.Problems), size, (after.TotalAlloc-before.TotalAlloc)>>20, elapsed)
	if len(invalid.Problems) > maxCycleProblems+1 {
		t.Errorf("expected at most %d problems, got %d", maxCycleProblems+1, len(invalid.Problems))
	}
	if size > 64<<10 {
		t.Errorf("expected the problems to stay below 64 KiB, got %d bytes", size)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<20 {
		t.Errorf("expected the validation to allocate less than 256 MiB, got %d MiB", allocated>>20)
	}
	if elapsed > 5*time.Second {
		t.Errorf("expected the validation to finish quickly, took %v", elapsed)
	}
	summarized := false
	for _, problem := range invalid.Problems {
		summarized = summarized || strings.Contains(problem.Message, "not listed")
	}
	if !summarized {
		t.Error("expected a summary of the cycles not listed")
	}
}

// Every parent of every group can be its own problem; the answer has to stay
// bounded regardless of how many there are.
func TestMeterProblemsStayBoundedOnManyBadEntries(t *testing.T) {
	env := validEnvironment()
	for i := 0; i < MaxNodes-10; i++ {
		parents := make([]MeterParent, MaxMeterParents)
		for k := range parents {
			parents[k] = MeterParent{Id: "x"}
		}
		env.MeterGroups = append(env.MeterGroups, MeterGroup{Id: fmt.Sprintf("g-%d", i), Name: "G", Parents: parents})
	}
	runtime.GC()
	before := runtime.MemStats{}
	runtime.ReadMemStats(&before)

	err := Validate(env)

	after := runtime.MemStats{}
	runtime.ReadMemStats(&after)
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected the entries to be refused, got %v", err)
	}
	size := 0
	summarized := false
	for _, problem := range invalid.Problems {
		size += len(problem.Path) + len(problem.Message)
		summarized = summarized || strings.Contains(problem.Message, "not listed")
	}
	t.Logf("%d problems, %d bytes, %d MiB allocated", len(invalid.Problems), size, (after.TotalAlloc-before.TotalAlloc)>>20)
	if len(invalid.Problems) > maxMeterProblems+1 {
		t.Errorf("expected at most %d problems, got %d", maxMeterProblems+1, len(invalid.Problems))
	}
	if size > 256<<10 {
		t.Errorf("expected the problems to stay below 256 KiB, got %d bytes", size)
	}
	if !summarized {
		t.Error("expected a summary of the problems not listed")
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<20 {
		t.Errorf("expected the validation to allocate less than 256 MiB, got %d MiB", allocated>>20)
	}
}

// The base document of the table is valid, so every problem above is the one
// its case introduced.
func TestTheMeterBaseDocumentIsValid(t *testing.T) {
	if err := Validate(meterEnvironment()); err != nil {
		t.Fatal(err)
	}
}

func TestMeterWeights(t *testing.T) {
	cases := []struct {
		name    string
		parents []MeterParent
		want    []int
		ok      bool
	}{
		{"none", nil, nil, true},
		{"one omitted", parentsOf("a"), []int{100}, true},
		{"three omitted", parentsOf("a", "b", "c"), []int{34, 33, 33}, true},
		{"six omitted", parentsOf("a", "b", "c", "d", "e", "f"), []int{17, 17, 17, 17, 16, 16}, true},
		{"explicit", []MeterParent{{Id: "a", Weight: 99}, {Id: "b", Weight: 1}}, []int{99, 1}, true},
		{"mixed", []MeterParent{{Id: "a", Weight: 50}, {Id: "b"}}, nil, false},
		{"sum below", []MeterParent{{Id: "a", Weight: 50}, {Id: "b", Weight: 49}}, nil, false},
		{"negative making up the sum", []MeterParent{{Id: "a", Weight: 101}, {Id: "b", Weight: -1}}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := MeterWeights(c.parents)
			if ok != c.ok || !reflect.DeepEqual(got, c.want) {
				t.Errorf("expected %v %v, got %v %v", c.want, c.ok, got, ok)
			}
		})
	}
	hundred := make([]MeterParent, MaxMeterParents)
	if weights, ok := MeterWeights(hundred); !ok || weights[0] != 1 || weights[MaxMeterParents-1] != 1 {
		t.Errorf("expected 100 parents to split into ones, got %v %v", weights, ok)
	}
	if _, ok := MeterWeights(make([]MeterParent, MaxMeterParents+1)); ok {
		t.Error("101 parents cannot all carry a weight of at least 1")
	}
}

func TestEffectiveMeterParents(t *testing.T) {
	if got := (Asset{}).EffectiveMeterParents(); got != nil {
		t.Errorf("an asset stating nothing has no parents, got %v", got)
	}
	if got := (Asset{SubmeteredBy: "a"}).EffectiveMeterParents(); !reflect.DeepEqual(got, []MeterParent{{Id: "a", Weight: 100}}) {
		t.Errorf("expected submetered_by with the whole flow, got %v", got)
	}
	explicit := []MeterParent{{Id: "b"}}
	if got := (Asset{SubmeteredBy: "a", MeterParents: explicit}).EffectiveMeterParents(); !reflect.DeepEqual(got, explicit) {
		t.Errorf("expected meter_parents to win, got %v", got)
	}
}
