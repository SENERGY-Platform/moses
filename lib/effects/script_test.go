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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// scriptSite is two zones: z-a with the scripted asset "self" and a neighbour
// "b", z-other with an asset "c" that getDevice cannot reach from z-a.
func scriptSite(code string) domain.Environment {
	return domain.Environment{
		Context: map[string]interface{}{"shift": 0.0},
		Zones: []domain.Zone{
			{Id: "z-a", Name: "Halle A", Assets: []domain.Asset{
				{Id: "self", Name: "Skript", Kind: domain.AssetMachine, Channels: []domain.Channel{{
					Id: "ch-self", Source: domain.Source{Kind: domain.SourceScript, Script: &domain.ScriptSource{Code: code}},
				}}},
				{Id: "b", Name: "Nachbar", Kind: domain.AssetMeter},
			}},
			{Id: "z-other", Name: "Halle B", Assets: []domain.Asset{
				{Id: "c", Name: "Fern", Kind: domain.AssetMeter},
			}},
		},
	}
}

func scriptEdge(from, to string, kind EdgeKind, key string, count int) Edge {
	return Edge{From: from, To: to, Kind: kind, Via: ViaScript, Channel: "ch-self", Key: key, Count: count}
}

func scriptEdgesOf(g Graph) []Edge {
	result := []Edge{}
	for _, edge := range g.Edges {
		if edge.Via == ViaScript {
			result = append(result, edge)
		}
	}
	return result
}

func expectScriptEdges(t *testing.T, g Graph, expected ...Edge) {
	t.Helper()
	actual := scriptEdgesOf(g)
	if len(actual) != len(expected) {
		t.Fatalf("expected %d script edges %v, got %d: %v", len(expected), expected, len(actual), actual)
	}
	for i := range expected {
		if actual[i] != expected[i] {
			t.Errorf("edge %d: expected %+v, got %+v", i, expected[i], actual[i])
		}
	}
}

func expectUnresolved(t *testing.T, g Graph, reasons ...string) {
	t.Helper()
	if len(g.Unresolved) != len(reasons) {
		t.Fatalf("expected %d unresolved entries, got %d: %+v", len(reasons), len(g.Unresolved), g.Unresolved)
	}
	for i, reason := range reasons {
		if !strings.Contains(g.Unresolved[i].Reason, reason) {
			t.Errorf("entry %d: expected a reason containing %q, got %+v", i, reason, g.Unresolved[i])
		}
		if g.Unresolved[i].Asset != "self" || g.Unresolved[i].Channel != "ch-self" {
			t.Errorf("entry %d: expected it to name the scripted channel, got %+v", i, g.Unresolved[i])
		}
	}
}

func TestAContextReadIsAnEdgeFromTheKeyToTheAsset(t *testing.T) {
	for _, code := range []string{
		"moses.environment.state.get('shift')",
		"moses.world.state.get('shift')",
		"moses.environment.state.get(`shift`)",
		"moses.environment['state'].get('shift')",
	} {
		g := Derive(scriptSite(code))
		expectScriptEdges(t, g, scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1))
		expectUnresolved(t, g)
	}
}

func TestAContextWriteIsAnEdgeFromTheAssetToTheKey(t *testing.T) {
	g := Derive(scriptSite("moses.environment.state.set('shift', 1)"))
	expectScriptEdges(t, g, scriptEdge("asset:self", "context:shift", EdgeWrites, "shift", 1))
}

func TestAnAliasOfTheEnvironmentStateIsFollowed(t *testing.T) {
	g := Derive(scriptSite("var E = moses.environment.state;\nvar x = E.get('shift') || 0;\nE.get('shift');"))
	expectScriptEdges(t, g, scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 2))
	expectUnresolved(t, g)
}

func TestSeveralDeclaratorsInOneVarAreEachAnAlias(t *testing.T) {
	g := Derive(scriptSite("var E = moses.environment.state, s = moses.asset.state, K = moses.zone.getDevice('b').state;\ns.set('x', E.get('shift') + K.get('p_w'));"))
	expectScriptEdges(t, g,
		scriptEdge("asset:b", "asset:self", EdgeReads, "p_w", 1),
		scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1),
	)
	expectUnresolved(t, g)
}

func TestAnAliasOfAZoneReachesItsDevices(t *testing.T) {
	g := Derive(scriptSite("var S = moses.environment.getRoom('z-a');\nvar pv = S.getDevice('b').state.get('pv_w') || 0;"))
	expectScriptEdges(t, g, scriptEdge("asset:b", "asset:self", EdgeReads, "pv_w", 1))
}

func TestAnAliasOfADeviceStateIsFollowed(t *testing.T) {
	g := Derive(scriptSite("var K = moses.environment.getRoom('z-a').getDevice('b').state;\nK.get('p_reserve_w'); K.set('ack', 1);"))
	expectScriptEdges(t, g,
		scriptEdge("asset:b", "asset:self", EdgeReads, "p_reserve_w", 1),
		scriptEdge("asset:self", "asset:b", EdgeWrites, "ack", 1),
	)
}

func TestAnAliasBoundByAPlainAssignmentAndThroughAnotherAliasIsFollowed(t *testing.T) {
	g := Derive(scriptSite("var W = moses.world;\nvar E;\nE = W.state;\nlet k = 'shift';\nconst f = function() { return E.get(k); };\nf();"))
	expectScriptEdges(t, g, scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1))
	expectUnresolved(t, g)
}

func TestAnAliasRebindingToTheSameHandleStaysAnAlias(t *testing.T) {
	g := Derive(scriptSite("var E = moses.environment.state;\nE = moses.world.state;\nE.get('shift');"))
	expectScriptEdges(t, g, scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1))
}

func TestTheZoneStateIsAZoneNodeAndReadsOnlyWhenReferenced(t *testing.T) {
	g := Derive(scriptSite("moses.zone.state.get('temp'); moses.room.state.set('temp', 1); moses.environment.getRoom('z-other').state.get('co2');"))
	expectScriptEdges(t, g,
		scriptEdge("asset:self", "zone:z-a", EdgeWrites, "temp", 1),
		scriptEdge("zone:z-a", "asset:self", EdgeReads, "temp", 1),
		scriptEdge("zone:z-other", "asset:self", EdgeReads, "co2", 1),
	)
	zones := map[string]Node{}
	for _, node := range g.Nodes {
		if node.Kind == NodeZone {
			zones[node.Id] = node
		}
	}
	if len(zones) != 2 || *zones["zone:z-a"].Site != "z-a" || zones["zone:z-other"].Label != "Halle B" {
		t.Errorf("expected exactly the two referenced zones as nodes, got %+v", zones)
	}
}

func TestTheAssetsOwnStateIsNoEdgeHoweverItIsReached(t *testing.T) {
	g := Derive(scriptSite("var s = moses.asset.state; s.set(s.get('x') + '_v', 1); moses.device.state.get('y');\nmoses.zone.getDevice('self').state.get('z'); moses.environment.getRoom('z-a').getDevice('self').state.set('w', 2);"))
	expectScriptEdges(t, g)
	//a computed key on the asset's own state is not a gap in the graph either
	expectUnresolved(t, g)
}

func TestEveryRowOfALiteralTableIsOneEdge(t *testing.T) {
	code := `var L = [["z-a", "b", "energy_kwh", 1], ["z-a", "c", "energy_kwh", -1], ["z-other", "c", "p_w", 1]];
for (var i = 0; i < L.length; i++) {
  var v = moses.environment.getRoom(L[i][0]).getDevice(L[i][1]).state.get(L[i][2]) || 0;
  if (L[i][3] > 0) { v = 0; }
}`
	g := Derive(scriptSite(code))
	expectUnresolved(t, g, `row 1 of the table: no asset "c" directly in zone "z-a"`)
	if g.Unresolved[0].Expression != "moses.environment.getRoom(L[i][0]).getDevice(L[i][1]).state.get(L[i][2])" {
		t.Errorf("expected the expression to be the access, got %q", g.Unresolved[0].Expression)
	}
	expectScriptEdges(t, g,
		scriptEdge("asset:b", "asset:self", EdgeReads, "energy_kwh", 1),
		scriptEdge("asset:c", "asset:self", EdgeReads, "p_w", 1),
	)
}

func TestAFlatTableAndAFixedRowAreRead(t *testing.T) {
	code := `var Z = moses.environment.getRoom('z-a'), ids = ['b', 'self'], d = 0;
for (var i = 0; i < ids.length; i++) { d += Z.getDevice(ids[i]).state.get('air_demand') || 0; }
var R = [['shift'], ['holiday']];
moses.environment.state.get(R[1][0]);`
	g := Derive(scriptSite(code))
	expectScriptEdges(t, g,
		scriptEdge("asset:b", "asset:self", EdgeReads, "air_demand", 1),
		scriptEdge("context:holiday", "asset:self", EdgeReads, "holiday", 1),
	)
	expectUnresolved(t, g)
}

func TestATableColumnWithoutAStringInEveryRowIsUnresolvedForThatRow(t *testing.T) {
	g := Derive(scriptSite("var L = [['shift'], [7], ['holiday']];\nfor (var i = 0; i < 3; i++) { moses.environment.state.get(L[i][0]); }"))
	expectUnresolved(t, g, "row 1 of the table: there is no string literal at position 0")
	expectScriptEdges(t, g,
		scriptEdge("context:holiday", "asset:self", EdgeReads, "holiday", 1),
		scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1),
	)
}

func TestArgumentsFromTwoTablesOrTwoRowsAreNotCombined(t *testing.T) {
	g := Derive(scriptSite("var A = [['z-a']], B = [['b']];\nmoses.environment.getRoom(A[i][0]).getDevice(B[i][0]).state.get('k');\nmoses.environment.getRoom(A[i][0]).getDevice(A[j][0]).state.get('k');"))
	expectUnresolved(t, g, "different tables or rows", "different tables or rows")
	expectScriptEdges(t, g)
}

func TestATableTheScriptModifiesOrHandsOnIsNotRead(t *testing.T) {
	for _, modification := range []string{"L.push(['holiday']);", "L[0] = ['holiday'];", "L[0][0] = 'holiday';", "helper(L);"} {
		g := Derive(scriptSite("var L = [['shift']];\n" + modification + "\nfor (var i = 0; i < 1; i++) { moses.environment.state.get(L[i][0]); }"))
		expectUnresolved(t, g, "the table is handed on or modified")
		expectScriptEdges(t, g)
	}
}

func TestAComputedKeyIsUnresolvedAndNeverGuessed(t *testing.T) {
	g := Derive(scriptSite("var E = moses.environment.state;\nfunction f(k) { E.get(k + '_v'); E.get(`${k}_n`); }\nE.get(pick());\nE.get(3);"))
	expectUnresolved(t, g,
		"the key is computed at run time",
		"the key is computed at run time",
		"the key is computed at run time",
		"the key is a number",
	)
	if g.Unresolved[0].Expression != "E.get(k + '_v')" {
		t.Errorf("expected the source of the call as the expression, got %q", g.Unresolved[0].Expression)
	}
	expectScriptEdges(t, g)
}

func TestAFunctionParameterAsKeyIsUnresolved(t *testing.T) {
	g := Derive(scriptSite("function read(key) { return moses.environment.state.get(key); }\nread('shift');"))
	expectUnresolved(t, g, "the key is held in a variable that is not bound to one string literal")
	expectScriptEdges(t, g)
}

func TestAShadowingDeclarationEndsTheAlias(t *testing.T) {
	for _, code := range []string{
		"var E = moses.environment.state;\nfunction f() { var E = moses.asset.state; return E.get('own'); }\nE.get('shift');",
		"var E = moses.environment.state;\nfunction f(E) { return E.get('own'); }\nE.get('shift');",
		"var E = moses.environment.state;\nE = moses.zone.state;\nE.get('shift');",
		"var E = moses.environment.state;\nfor (E in {}) {}\nE.get('shift');",
	} {
		g := Derive(scriptSite(code))
		expectScriptEdges(t, g)
		if len(g.Unresolved) == 0 || !strings.Contains(g.Unresolved[0].Reason, "is handed on where its reads and writes are not followed") {
			t.Errorf("%q: expected the lost alias to be reported, got %+v", code, g.Unresolved)
		}
	}
}

func TestAHandleThatEscapesIsReported(t *testing.T) {
	g := Derive(scriptSite("helper(moses.environment.state);\nvar o = {s: moses.zone};\nvar E = cond ? moses.environment.state : moses.zone.state;"))
	expectUnresolved(t, g,
		"a handle on the environment state is handed on",
		"a handle on a zone is handed on",
		"a handle on the environment state is handed on",
		"a handle on a zone state is handed on",
	)
}

func TestAHandleOfTheAssetItselfMayEscape(t *testing.T) {
	g := Derive(scriptSite("helper(moses.asset.state); var o = {a: moses.device}; moses.service.send(1);"))
	expectUnresolved(t, g)
}

func TestLogicalOperatorsForwardAnObjectHandle(t *testing.T) {
	g := Derive(scriptSite("var S = moses.environment.getRoom('z-a') || {};\nvar T = null ?? S;\nvar K = S && S.getDevice('b').state;\nK.get('p_w');"))
	expectScriptEdges(t, g, scriptEdge("asset:b", "asset:self", EdgeReads, "p_w", 1))
}

func TestAMethodCalledThroughItsOwnPropertiesIsReported(t *testing.T) {
	g := Derive(scriptSite("var g = moses.environment.state.get;\ng('shift');\nmoses.environment.state.get.call(null, 'holiday');"))
	//a bound get is a closure in the runtime, so calling it off its state works
	expectScriptEdges(t, g, scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1))
	expectUnresolved(t, g, "the get function of the environment state is taken off it")
}

func TestAComputedMemberOfAHandleIsUnresolved(t *testing.T) {
	g := Derive(scriptSite("function f(n) { return moses.environment[n].get('shift'); }"))
	expectUnresolved(t, g, "the member of the environment is computed at run time")
	expectScriptEdges(t, g)
}

// A name or key concatenated from string literals is a literal as well.
func TestConcatenatedLiteralsAreRead(t *testing.T) {
	g := Derive(scriptSite("var k = 'r';\nmoses.environment['st' + 'ate'].get(k + '_v');"))
	expectScriptEdges(t, g, scriptEdge("context:r_v", "asset:self", EdgeReads, "r_v", 1))
	expectUnresolved(t, g)
}

func TestAWithStatementAndARebindingOfMosesAreReported(t *testing.T) {
	g := Derive(scriptSite("with (moses.environment) { state.get('shift'); }"))
	expectUnresolved(t, g, "a with statement", "a handle on the environment is handed on")
	g = Derive(scriptSite("var moses = {environment: {state: {get: function() {}}}};\nmoses.environment.state.get('shift');"))
	expectUnresolved(t, g, "the script binds the name moses itself")
	expectScriptEdges(t, g)
}

func TestAnUnknownZoneOrAnAssetOutsideTheZoneIsUnresolved(t *testing.T) {
	g := Derive(scriptSite("moses.environment.getRoom('nope').state.get('t');\nmoses.environment.getRoom('z-a').getDevice('c').state.get('p');\nmoses.zone.getDevice('ghost').state.get('p');"))
	expectUnresolved(t, g,
		`no zone "nope" in the document`,
		`no asset "c" directly in zone "z-a"`,
		`no asset "ghost" directly in zone "z-a"`,
	)
}

func TestAScriptThatDoesNotParseIsOneEntryAndTheRestIsDerived(t *testing.T) {
	env := scriptSite("var = ;")
	env.Zones[0].Assets[1].Channels = []domain.Channel{{
		Id: "ch-b", Source: domain.Source{Kind: domain.SourceScript, Script: &domain.ScriptSource{Code: "moses.environment.state.get('shift')"}},
	}}
	g := Derive(env)
	expectUnresolved(t, g, "the script does not parse")
	if g.Unresolved[0].Expression != "var = ;" {
		t.Errorf("expected the start of the script as the expression, got %q", g.Unresolved[0].Expression)
	}
	if len(g.Edges) != 1 || g.Edges[0].From != "context:shift" || g.Edges[0].To != "asset:b" {
		t.Errorf("expected the other script to be derived anyway, got %+v", g.Edges)
	}
}

// Nesting without brackets passes the pre-scan and is bounded by the walk.
func TestAPathologicallyNestedScriptIsCutOffAndReported(t *testing.T) {
	code := "var x = " + strings.Repeat("!", maxDepth+10) + "1;\nmoses.environment.state.get('shift');"
	g := Derive(scriptSite(code))
	expectUnresolved(t, g, "the script nests deeper than the analysis follows")
	expectScriptEdges(t, g, scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1))
}

func TestALongExpressionIsCutToTheSnippetLength(t *testing.T) {
	g := Derive(scriptSite("function f(a) { moses.environment.state.get(a + '" + strings.Repeat("ä", 200) + "'); }"))
	expectUnresolved(t, g, "computed at run time")
	expression := []rune(g.Unresolved[0].Expression)
	if len(expression) != maxSnippet || !strings.HasSuffix(string(expression), "...") {
		t.Errorf("expected %d runes ending in an ellipsis, got %d: %q", maxSnippet, len(expression), string(expression))
	}
}

func TestCodeEvaluatedFromAStringIsReported(t *testing.T) {
	g := Derive(scriptSite("eval(\"moses.environment.state.get('shift')\");\nnew Function('return 1')();"))
	expectUnresolved(t, g, "the script reaches eval", "the script reaches Function")
	g = Derive(scriptSite("function eval2() {}\nvar Function = function() {};\nFunction('x');"))
	expectUnresolved(t, g)
}

func TestAnEmptyKeyIsNoEdge(t *testing.T) {
	g := Derive(scriptSite("moses.environment.state.get(''); moses.zone.state.set('', 1);"))
	expectScriptEdges(t, g)
	expectUnresolved(t, g)
	if _, ok := nodeById(g, "context:"); ok {
		t.Error("expected no node for an empty key")
	}
}

// Every construct goja parses is walked: a read inside each of them is found,
// and none of them is reported as syntax the analysis does not know.
func TestEveryConstructIsWalked(t *testing.T) {
	code := `var E = moses.environment.state;
class C extends Object {
  #p = E.get('k1');
  static s = E.get('k2');
  static { E.get('k3'); }
  get g() { return E.get('k4'); }
  has(o) { return #p in o; }
  constructor() { super(); new.target; this.x = E.get('k5'); }
}
var arrow = (a = E.get('k6'), ...rest) => E.get('k7');
var {q = E.get('k8'), ...others} = {q: 1};
var [r = E.get('k9'), , ...tail] = [1];
function* gen() { yield E.get('k10'); }
async function run() { await E.get('k11'); }
var o = {e: 1, [E.get('k12')]: 1, ...{s: E.get('k13')}, m() { return E.get('k14'); }};
var t = String.raw` + "`x${E.get('k15')}`" + `;
var u = o?.m?.() ?? E.get('k16');
label: for (var key in o) { for (var v of [1]) { if (v) { continue label; } } }
do { E.get('k17'); } while (false);
switch (E.get('k18')) { case E.get('k19'): break; default: E.get('k20'); }
try { throw E.get('k21'); } catch ({message = E.get('k22')}) { E.get('k23'); } finally { E.get('k24'); }
var re = /x/g, seq = (E.get('k25'), 1), typ = typeof E.get('k26'), neg = -E.get('k27');
[r, q] = [E.get('k28'), 2];
while (false) { E.get('k29'); }
for (let i = 0; i < 1; i++) { E.get('k30'); }
debugger;
;`
	g := Derive(scriptSite(code))
	for _, entry := range g.Unresolved {
		if strings.Contains(entry.Reason, "does not know") || strings.Contains(entry.Reason, "does not parse") {
			t.Fatalf("expected every construct to be known, got %+v", entry)
		}
	}
	found := map[string]bool{}
	for _, edge := range scriptEdgesOf(g) {
		found[edge.Key] = true
	}
	for i := 1; i <= 30; i++ {
		key := "k" + strconv.Itoa(i)
		if !found[key] {
			t.Errorf("expected the read of %s to be found, got %v and %+v", key, found, g.Unresolved)
		}
	}
}

// A property of the global object is the variable of that name, so a write
// through it rebinds the name.
func TestAWriteThroughTheGlobalObjectEndsTheAlias(t *testing.T) {
	for _, write := range []string{"globalThis.k = 'shift2';", "this.k = 'shift2';", "globalThis['k'] = 'shift2';", "delete this.k;"} {
		g := Derive(scriptSite("var k = 'a';\n" + write + "\nmoses.environment.state.get(k);"))
		expectScriptEdges(t, g)
		expectUnresolved(t, g, "the key is held in a variable that is not bound to one string literal")
	}
	for _, write := range []string{"globalThis[name] = 'x';", "Object.assign(globalThis, {k: 'x'});", "var g = this;", "(function() { return this; })().k = 1;"} {
		g := Derive(scriptSite("var k = 'shift';\n" + write + "\nmoses.environment.state.get(k);"))
		expectScriptEdges(t, g)
		expectUnresolved(t, g, "the script writes the global object through a computed name or hands it on")
	}
	g := Derive(scriptSite("globalThis.moses = {};\nmoses.environment.state.get('shift');"))
	expectScriptEdges(t, g)
	expectUnresolved(t, g, "the script binds the name moses itself")
	g = Derive(scriptSite("globalThis.moses.environment.state.get('shift');"))
	expectUnresolved(t, g, "the script reaches moses through the global object")
}

func TestADeclarationWithoutInitializerIsABindingOfUndefined(t *testing.T) {
	for _, code := range []string{
		"var k = 'shift';\nfunction f() { var k; return moses.environment.state.get(k); }",
		"var k = 'shift';\n{ let k; moses.environment.state.get(k); }",
		"var k = 'shift';\nfor (let k; false;) {}\nmoses.environment.state.get(k);",
	} {
		g := Derive(scriptSite(code))
		expectScriptEdges(t, g)
		expectUnresolved(t, g, "the key is held in a variable that is not bound to one string literal")
	}
	//a redeclaration in the global scope changes nothing
	g := Derive(scriptSite("var k = 'shift';\nvar k;\nlet E;\nE = moses.environment.state;\nE.get(k);"))
	expectScriptEdges(t, g, scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1))
	expectUnresolved(t, g)
}

func TestAWriteIntoTheApiLeavesEveryAccessUnresolved(t *testing.T) {
	for _, write := range []string{
		"moses.environment.state.get = function() { return 0; };",
		"moses.environment.state = {get: function() { return 0; }};",
		"moses.environment = {};",
		"delete moses.zone;",
		"moses.asset.state.get = moses.world.state.get;",
		"[moses.environment.state] = [{}];",
	} {
		g := Derive(scriptSite(write + "\nmoses.environment.state.get('shift');"))
		expectScriptEdges(t, g)
		reasons := []string{}
		for _, entry := range g.Unresolved {
			reasons = append(reasons, entry.Reason)
		}
		joined := strings.Join(reasons, "|")
		if !strings.Contains(joined, "of the moses api") || !strings.Contains(joined, "the script changes the moses api or hands a handle on") {
			t.Errorf("%q: expected the write and the unresolved access, got %+v", write, g.Unresolved)
		}
	}
	//the channel api is not read through, writing into it changes no access
	g := Derive(scriptSite("moses.channel.note = 1;\nmoses.environment.state.get('shift');"))
	expectScriptEdges(t, g, scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1))
}

func TestAHandleHandedOnLeavesEveryAccessUnresolved(t *testing.T) {
	g := Derive(scriptSite("helper(moses.environment.state);\nmoses.zone.getDevice('b').state.get('p_w');"))
	expectScriptEdges(t, g)
	expectUnresolved(t, g, "a handle on the environment state is handed on", "the script changes the moses api or hands a handle on")
}

func TestAWriteToATimelineGovernedKeyIsDroppedAndReported(t *testing.T) {
	env := scriptSite("moses.environment.state.set('shift', 1);\nmoses.environment.state.get('shift');\nmoses.environment.state.set('free', 1);")
	env.Timeline = []domain.DatedChange{{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Target: "context.shift", Value: 1}}
	g := Derive(env)
	expectScriptEdges(t, g,
		scriptEdge("asset:self", "context:free", EdgeWrites, "free", 1),
		scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1),
	)
	expectUnresolved(t, g, "write to a timeline-governed key is dropped at runtime")
}

func TestIndirectEvaluationIsReported(t *testing.T) {
	for _, code := range []string{
		"(0, eval)('moses');",
		"globalThis.eval('moses');",
		"(function(){}).constructor('return moses')();",
		"var f = function(){}; f['constr' + 'uctor']('x')();",
		"Function('x')();",
		"[].map['constructor']('x');",
	} {
		g := Derive(scriptSite(code))
		found := false
		for _, entry := range g.Unresolved {
			found = found || strings.Contains(entry.Reason, "code it runs from a string is not analysed")
		}
		if !found {
			t.Errorf("%q: expected the evaluation to be reported, got %+v", code, g.Unresolved)
		}
	}
}

func TestASetWithoutValueOrAGetWithoutKeyDoesNothing(t *testing.T) {
	g := Derive(scriptSite("var E = moses.environment.state;\nE.set('a'); E.set('b', null); E.set('c', undefined); E.set('d', void 0);\nE.get(); E.get(null); E.get(undefined); E.set();"))
	expectScriptEdges(t, g)
	expectUnresolved(t, g)
}
