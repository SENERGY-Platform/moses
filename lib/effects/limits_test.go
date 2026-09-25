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
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

func withLimits(t *testing.T, change func()) {
	t.Helper()
	saved := limits
	t.Cleanup(func() { limits = saved })
	change()
}

// literalTable is a table of n one-column rows k0 .. k<n-1>.
func literalTable(n int) string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf("['k%d']", i)
	}
	return "var L = [" + strings.Join(rows, ",") + "];\n"
}

// measure runs f and answers how long it took and how many bytes it allocated.
func measure(f func()) (time.Duration, uint64) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	f()
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	return elapsed, after.TotalAlloc - before.TotalAlloc
}

// 500 reads of one 500 row table are one shape: the table is walked once and
// every edge counts all 500 reads, instead of 250,000 row lookups.
func TestAReadRepeatedOverALargeTableIsResolvedOnce(t *testing.T) {
	code := literalTable(500) + strings.Repeat("moses.environment.state.get(L[i][0]);\n", 500)
	var g Graph
	elapsed, allocated := measure(func() { g = Derive(scriptSite(code)) })
	expectUnresolved(t, g)
	edges := scriptEdgesOf(g)
	if len(edges) != 500 || edges[0].Count != 500 {
		t.Fatalf("expected 500 edges counting 500 reads each, got %d, first %+v", len(edges), edges[0])
	}
	if elapsed > time.Second || allocated > 64<<20 {
		t.Errorf("expected well under a second and 64 MiB, took %v and allocated %d MiB", elapsed, allocated>>20)
	}
	t.Logf("500 x 500: %v, %d MiB allocated", elapsed, allocated>>20)
}

// 500 different reads of a 500 row table are 250,000 row lookups; the row
// budget stops the script after 100 of them with one entry.
func TestTheRowBudgetTruncatesAScript(t *testing.T) {
	reads := make([]string, 500)
	for i := range reads {
		reads[i] = fmt.Sprintf("moses.environment.getRoom('z-a').getDevice(L[i][0]).state.get('p%d');", i)
	}
	code := strings.ReplaceAll(literalTable(500), "'k", "'b") + strings.Join(reads, "\n")
	var g Graph
	elapsed, allocated := measure(func() { g = Derive(scriptSite(code)) })
	truncations := 0
	for _, entry := range g.Unresolved {
		if strings.Contains(entry.Reason, "tables are larger than the analysis reads") {
			truncations++
		}
	}
	if truncations != 1 {
		t.Fatalf("expected one truncation entry, got %+v", g.Unresolved)
	}
	if edges := len(scriptEdgesOf(g)); edges != 0 {
		t.Errorf("expected a truncated script to add no edge, got %d", edges)
	}
	if elapsed > time.Second || allocated > 64<<20 {
		t.Errorf("expected well under a second and 64 MiB, took %v and allocated %d MiB", elapsed, allocated>>20)
	}
	t.Logf("500 x 500 distinct: %v, %d MiB allocated", elapsed, allocated>>20)
}

func TestTheScriptCapsTruncateWithOneEntry(t *testing.T) {
	withLimits(t, func() { limits.edgesPerScript, limits.unresolvedPerScript = 10, 5 })
	reads := make([]string, 20)
	for i := range reads {
		reads[i] = fmt.Sprintf("moses.environment.state.get('k%02d');", i)
	}
	g := Derive(scriptSite(strings.Join(reads, "\n")))
	if len(scriptEdgesOf(g)) != 10 || len(g.Unresolved) != 1 || !strings.Contains(g.Unresolved[0].Reason, "at most 10 edges") {
		t.Errorf("expected ten edges and the truncation, got %d edges and %+v", len(scriptEdgesOf(g)), g.Unresolved)
	}

	g = Derive(scriptSite("function f(k) {\n" + strings.Repeat("moses.environment.state.get(k + '');\n", 20) + "}"))
	if len(g.Unresolved) != 6 || !strings.Contains(g.Unresolved[5].Reason, "at most 5") {
		t.Errorf("expected five entries and the truncation, got %+v", g.Unresolved)
	}
}

func TestTheDocumentCapsTruncateWithOneEntry(t *testing.T) {
	withLimits(t, func() { limits.edgesPerDocument, limits.unresolvedPerDocument = 15, 3 })
	env := domain.Environment{Zones: []domain.Zone{{Id: "z"}}}
	for a := 0; a < 3; a++ {
		reads := make([]string, 10)
		for i := range reads {
			reads[i] = fmt.Sprintf("moses.environment.state.get('k%d');", i)
		}
		env.Zones[0].Assets = append(env.Zones[0].Assets, domain.Asset{Id: fmt.Sprintf("a%d", a), Channels: []domain.Channel{
			channelOf(fmt.Sprintf("ch-%d", a), domain.Source{Kind: domain.SourceScript, Script: &domain.ScriptSource{Code: strings.Join(reads, "")}}),
			channelOf(fmt.Sprintf("ch-f-%d", a), domain.Source{Kind: domain.SourceFormula, Formula: &domain.FormulaSource{
				Expression: "1", Inputs: map[string]string{"x": "channel.ghost", "y": "channel.ghost-2"}}}),
		}})
	}
	g := Derive(env)
	truncations := 0
	for _, entry := range g.Unresolved {
		if strings.Contains(entry.Reason, "the document has more effects") {
			truncations++
		}
	}
	if len(g.Edges) != 15 || len(g.Unresolved) != 4 || truncations != 1 {
		t.Errorf("expected 15 edges, three entries and one truncation, got %d edges and %+v", len(g.Edges), g.Unresolved)
	}
	first, _ := json.Marshal(g)
	again, _ := json.Marshal(Derive(env))
	if string(first) != string(again) {
		t.Error("expected a truncated derivation to be deterministic")
	}
}
