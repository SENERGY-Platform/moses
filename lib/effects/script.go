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
	"fmt"
	"math"
	"reflect"
	"sort"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/token"
)

// The script analysis reduces every expression to what it denotes in the api of
// lib/runtime/jsapi.go, or to unknown. A name is an alias only while every
// binding it has anywhere in the script denotes the same thing.

type valueKind int

const (
	vUnknown valueKind = iota
	vMoses
	vEnvironment
	vZone
	vAsset
	vChannel
	vState
	vMethod
	vString
	vTable
	vRow
	vCell
)

type scopeKind int

const (
	scopeContext scopeKind = iota
	scopeZone
	scopeAsset
)

type termKind int

const (
	termBad termKind = iota
	termLiteral
	termCell
)

// tableIndex is how a table is indexed: by a variable, which stands for every
// row, or by an integer literal, which stands for one.
type tableIndex struct {
	name    string
	fixed   int
	isFixed bool
}

func (this tableIndex) id() string {
	if this.isFixed {
		return fmt.Sprintf("#%d", this.fixed)
	}
	return this.name
}

// term is an argument naming a zone, an asset or a key.
type term struct {
	kind    termKind
	literal string
	table   *table
	index   tableIndex
	column  int
	reason  string
}

func (this term) id() string {
	switch this.kind {
	case termLiteral:
		return "l:" + this.literal
	case termCell:
		return fmt.Sprintf("c:%p:%s:%d", this.table, this.index.id(), this.column)
	}
	return "b:" + this.reason
}

// ref is a zone or asset reference: the script's own, or one named by a term.
type ref struct {
	own  bool
	term term
}

func (this ref) id() string {
	if this.own {
		return "own"
	}
	return this.term.id()
}

type value struct {
	kind     valueKind
	scope    scopeKind
	zone     ref
	asset    ref
	method   string
	receiver *value
	literal  string
	table    *table
	index    tableIndex
	column   int
}

var unknown = value{kind: vUnknown}

// identity is equal for two values that denote the same thing; unknown is
// never equal to anything, including itself.
func (this value) identity() string {
	if this.kind == vUnknown {
		return ""
	}
	receiver := ""
	if this.receiver != nil {
		receiver = this.receiver.identity()
	}
	return fmt.Sprintf("%d|%d|%s|%s|%s|%s|%q|%p|%s|%d", this.kind, this.scope, this.zone.id(), this.asset.id(),
		this.method, receiver, this.literal, this.table, this.index.id(), this.column)
}

// relevant is true for a value whose reads and writes can be an edge. The
// asset's own state never is.
func (this value) relevant() bool {
	switch this.kind {
	case vMoses, vEnvironment, vZone:
		return true
	case vAsset:
		return !this.asset.own
	case vState:
		return this.scope != scopeAsset || !this.asset.own
	case vMethod:
		return this.receiver.relevant()
	}
	return false
}

// object is true for a value that is a javascript object, hence truthy and
// not nullish, which is what decides a || b, a && b and a ?? b.
func (this value) object() bool {
	switch this.kind {
	case vMoses, vEnvironment, vZone, vAsset, vChannel, vState, vMethod, vTable, vRow:
		return true
	}
	return false
}

// api is true for a part of the moses api a write into can redirect what an
// access reads, the asset's own handles included.
func (this value) api() bool {
	switch this.kind {
	case vMoses, vEnvironment, vZone, vAsset, vState, vMethod:
		return true
	}
	return false
}

func (this value) describe() string {
	switch this.kind {
	case vMoses:
		return "moses"
	case vEnvironment:
		return "the environment"
	case vZone:
		return "a zone"
	case vAsset:
		return "an asset"
	case vState:
		switch this.scope {
		case scopeContext:
			return "the environment state"
		case scopeZone:
			return "a zone state"
		}
		return "an asset state"
	case vMethod:
		return this.receiver.describe()
	}
	return "a value"
}

// table is an array literal. flat holds each element that is a string
// literal, rows the same per element when every element is an array literal.
type table struct {
	flat    []*string
	rows    [][]*string
	tainted string
}

func literalString(expression ast.Expression) *string {
	switch literal := expression.(type) {
	case *ast.StringLiteral:
		text := literal.Value.String()
		return &text
	case *ast.TemplateLiteral:
		if literal.Tag == nil && len(literal.Expressions) == 0 && len(literal.Elements) == 1 {
			text := literal.Elements[0].Parsed.String()
			return &text
		}
	}
	return nil
}

func newTable(literal *ast.ArrayLiteral) *table {
	result := &table{flat: make([]*string, len(literal.Value))}
	nested := len(literal.Value) > 0
	rows := make([][]*string, len(literal.Value))
	for i, element := range literal.Value {
		result.flat[i] = literalString(element)
		row, ok := element.(*ast.ArrayLiteral)
		if !ok {
			nested = false
			continue
		}
		rows[i] = make([]*string, len(row.Value))
		for j, cell := range row.Value {
			rows[i][j] = literalString(cell)
		}
	}
	if nested {
		result.rows = rows
	}
	return result
}

func (this *table) length(column int) int {
	if column < 0 {
		return len(this.flat)
	}
	return len(this.rows)
}

// cell is the string at one row and column, nil where there is none.
func (this *table) cell(row int, column int) *string {
	if column < 0 {
		if row < len(this.flat) {
			return this.flat[row]
		}
		return nil
	}
	if row < len(this.rows) && column < len(this.rows[row]) {
		return this.rows[row][column]
	}
	return nil
}

type use int

const (
	// useEscape is every place a value flows on to something the analysis does
	// not follow: an argument, a return, a property, an untracked variable.
	useEscape use = iota
	useObject
	useCallee
	// useDiscard is a value that is dropped or only observed: a statement, a
	// test, an operand of arithmetic.
	useDiscard
	useAlias
)

type access struct {
	write bool
	state value
	key   term
	node  ast.Node
}

type finding struct {
	node   ast.Node
	reason string
}

// maxDepth bounds the recursion over a script, so a pathologically nested
// document cannot exhaust the stack of a request.
const maxDepth = 2000

type scriptAnalysis struct {
	code string

	bindings   map[string][]ast.Expression
	poisoned   map[string]bool
	mosesBound ast.Node

	// globalChanged is where the global object is written through a computed
	// name or handed on; every name then may refer to anything. functions
	// counts the function scopes the first pass is in, topLevel holds the
	// declarations directly in the program.
	globalChanged ast.Node
	globalMembers map[ast.Node]bool
	functions     int
	topLevel      map[ast.Node]bool

	// apiChanged is set when the script writes into the moses api or hands a
	// handle on; none of its accesses is then trusted.
	apiChanged bool

	aliases   map[string]value
	resolving map[string]bool
	tables    map[*ast.ArrayLiteral]*table

	// pure is non-zero while an alias is evaluated, which must neither record
	// nor report: the same expression is visited for real in its own place.
	pure  int
	depth int

	accesses    []access
	findings    []finding
	unsupported map[string]bool
	tooDeep     bool
}

func newScriptAnalysis(code string) *scriptAnalysis {
	return &scriptAnalysis{
		code:          code,
		bindings:      map[string][]ast.Expression{},
		poisoned:      map[string]bool{},
		globalMembers: map[ast.Node]bool{},
		topLevel:      map[ast.Node]bool{},
		aliases:       map[string]value{},
		resolving:     map[string]bool{},
		tables:        map[*ast.ArrayLiteral]*table{},
		unsupported:   map[string]bool{},
	}
}

// parseScript parses without source maps: the parser would otherwise read the
// file a sourceMappingURL comment names, and a script is untrusted input.
func parseScript(code string) (*ast.Program, error) {
	return parser.ParseFile(nil, "", code, 0, parser.WithDisableSourceMaps)
}

func (this *scriptAnalysis) run(program *ast.Program) {
	for _, statement := range program.Body {
		this.topLevel[statement] = true
	}
	for _, statement := range program.Body {
		this.collect(statement, 0)
	}
	if this.globalChanged != nil {
		this.report(this.globalChanged, "the script writes the global object through a computed name or hands it on, what its names refer to is not followed")
	} else if this.mosesBound != nil {
		this.report(this.mosesBound, "the script binds the name moses itself, what it reads and writes through it is not followed")
	}
	for _, statement := range program.Body {
		this.statement(statement)
	}
}

func (this *scriptAnalysis) report(node ast.Node, reason string) {
	if this.pure > 0 {
		return
	}
	this.findings = append(this.findings, finding{node: node, reason: reason})
}

func (this *scriptAnalysis) reportUnsupported(node ast.Node) {
	name := fmt.Sprintf("%T", node)
	if this.unsupported[name] {
		return
	}
	this.unsupported[name] = true
	this.findings = append(this.findings, finding{node: node, reason: "the script uses syntax the analysis does not know (" + name + ")"})
}

func (this *scriptAnalysis) source(node ast.Node) string {
	if isNil(node) {
		return ""
	}
	from, to := int(node.Idx0())-1, int(node.Idx1())-1
	if from < 0 || from > len(this.code) {
		return ""
	}
	if to > len(this.code) || to < from {
		to = len(this.code)
	}
	return this.code[from:to]
}

func (this *scriptAnalysis) position(node ast.Node) int {
	if isNil(node) {
		return 0
	}
	return int(node.Idx0())
}

func isNil(node ast.Node) bool {
	if node == nil {
		return true
	}
	target := reflect.ValueOf(node)
	return target.Kind() == reflect.Pointer && target.IsNil()
}

// --- first pass: every binding of every name ---------------------------------

func (this *scriptAnalysis) bind(identifier *ast.Identifier, initializer ast.Expression) {
	name := identifier.Name.String()
	if name == "moses" && this.mosesBound == nil {
		this.mosesBound = identifier
	}
	this.bindings[name] = append(this.bindings[name], initializer)
}

func (this *scriptAnalysis) poison(identifier *ast.Identifier) {
	this.poisonName(identifier.Name.String(), identifier)
}

func (this *scriptAnalysis) poisonName(name string, node ast.Node) {
	if name == "moses" && this.mosesBound == nil {
		this.mosesBound = node
	}
	this.poisoned[name] = true
}

// poisonTarget marks every name a destructuring, a parameter or a loop
// variable binds: its value is not an expression the analysis can evaluate.
func (this *scriptAnalysis) poisonTarget(target ast.Expression) {
	switch t := target.(type) {
	case *ast.Identifier:
		this.poison(t)
	case *ast.ArrayPattern:
		for _, element := range t.Elements {
			if !isNil(element) {
				this.poisonTarget(element)
			}
		}
		if !isNil(t.Rest) {
			this.poisonTarget(t.Rest)
		}
	case *ast.ObjectPattern:
		for _, property := range t.Properties {
			switch p := property.(type) {
			case *ast.PropertyShort:
				this.poison(&p.Name)
			case *ast.PropertyKeyed:
				this.poisonTarget(p.Value)
			}
		}
		if !isNil(t.Rest) {
			this.poisonTarget(t.Rest)
		}
	case *ast.AssignExpression:
		this.poisonTarget(t.Left)
	case *ast.Binding:
		this.poisonTarget(t.Target)
	default:
		this.globalWrite(target)
	}
}

// declare records a declaration. One without an initializer is a new binding
// holding undefined unless it redeclares a name of the global scope, where it
// changes nothing.
func (this *scriptAnalysis) declare(list []*ast.Binding, global bool) {
	for _, binding := range list {
		identifier, ok := binding.Target.(*ast.Identifier)
		if !ok {
			this.poisonTarget(binding.Target)
			continue
		}
		if identifier.Name.String() == "moses" && this.mosesBound == nil {
			this.mosesBound = identifier
		}
		switch {
		case binding.Initializer != nil:
			this.bind(identifier, binding.Initializer)
		case !global:
			this.poison(identifier)
		}
	}
}

func (this *scriptAnalysis) isGlobalObject(expression ast.Expression) bool {
	switch e := expression.(type) {
	case *ast.ThisExpression:
		return true
	case *ast.Identifier:
		return e.Name.String() == "globalThis"
	}
	return false
}

// globalWrite handles a write whose target may be a property of the global
// object, which is the variable of that name.
func (this *scriptAnalysis) globalWrite(target ast.Expression) {
	switch t := target.(type) {
	case *ast.DotExpression:
		if this.isGlobalObject(t.Left) {
			this.poisonName(t.Identifier.Name.String(), t)
		}
	case *ast.BracketExpression:
		if !this.isGlobalObject(t.Left) {
			return
		}
		if text := literalString(t.Member); text != nil {
			this.poisonName(*text, t)
		} else if this.globalChanged == nil {
			this.globalChanged = t
		}
	}
}

func (this *scriptAnalysis) collect(node ast.Node, depth int) {
	if isNil(node) {
		return
	}
	if depth > maxDepth {
		this.tooDeep = true
		return
	}
	switch n := node.(type) {
	case *ast.VariableStatement:
		this.declare(n.List, this.functions == 0)
	case *ast.LexicalDeclaration:
		this.declare(n.List, this.topLevel[n])
	case *ast.ForLoopInitializerVarDeclList:
		this.declare(n.List, this.functions == 0)
	case *ast.DotExpression:
		if this.isGlobalObject(n.Left) {
			this.globalMembers[n.Left] = true
		}
	case *ast.BracketExpression:
		if this.isGlobalObject(n.Left) {
			this.globalMembers[n.Left] = true
		}
	case *ast.ThisExpression, *ast.Identifier:
		if this.isGlobalObject(n.(ast.Expression)) && !this.globalMembers[n] && this.globalChanged == nil {
			this.globalChanged = n
		}
	case *ast.ParameterList:
		for _, binding := range n.List {
			this.poisonTarget(binding.Target)
		}
		if !isNil(n.Rest) {
			this.poisonTarget(n.Rest)
		}
	case *ast.ForIntoVar:
		this.poisonTarget(n.Binding.Target)
	case *ast.ForDeclaration:
		this.poisonTarget(n.Target)
	case *ast.ForIntoExpression:
		this.poisonTarget(n.Expression)
	case *ast.AssignExpression:
		switch left := n.Left.(type) {
		case *ast.Identifier:
			if n.Operator == token.ASSIGN {
				this.bind(left, n.Right)
			} else {
				this.poison(left)
			}
		case *ast.ArrayPattern, *ast.ObjectPattern:
			this.poisonTarget(left)
		default:
			this.globalWrite(left)
		}
	case *ast.UnaryExpression:
		if n.Operator == token.INCREMENT || n.Operator == token.DECREMENT || n.Operator == token.DELETE {
			if identifier, ok := n.Operand.(*ast.Identifier); ok {
				this.poison(identifier)
			}
			this.globalWrite(n.Operand)
		}
	case *ast.FunctionLiteral, *ast.ArrowFunctionLiteral, *ast.ClassStaticBlock:
		if literal, ok := n.(*ast.FunctionLiteral); ok && literal.Name != nil {
			this.poison(literal.Name)
		}
		this.functions++
		defer func() { this.functions-- }()
	case *ast.ClassLiteral:
		if n.Name != nil {
			this.poison(n.Name)
		}
	case *ast.CatchStatement:
		if !isNil(n.Parameter) {
			this.poisonTarget(n.Parameter)
		}
	}
	if !forEachChild(node, func(child ast.Node) { this.collect(child, depth+1) }) {
		this.reportUnsupported(node)
	}
}

// --- aliases -------------------------------------------------------------------

// alias is what a name denotes: the one value every binding of it agrees on,
// or unknown.
func (this *scriptAnalysis) alias(name string) value {
	if this.globalChanged != nil {
		return unknown
	}
	if name == "moses" {
		if this.mosesBound != nil {
			return unknown
		}
		return value{kind: vMoses}
	}
	if known, ok := this.aliases[name]; ok {
		return known
	}
	if this.poisoned[name] || this.resolving[name] || len(this.bindings[name]) == 0 {
		return unknown
	}
	this.resolving[name] = true
	this.pure++
	result := this.expression(this.bindings[name][0], useDiscard)
	for _, initializer := range this.bindings[name][1:] {
		if result.kind == vUnknown {
			break
		}
		if this.expression(initializer, useDiscard).identity() != result.identity() {
			result = unknown
		}
	}
	this.pure--
	delete(this.resolving, name)
	this.aliases[name] = result
	return result
}

func (this *scriptAnalysis) tracked(name string) bool {
	return this.alias(name).kind != vUnknown
}

// --- second pass: what every expression denotes ----------------------------------

// check reports a relevant value that reaches a place the analysis does not
// follow, and taints a table that does.
func (this *scriptAnalysis) check(node ast.Node, v value, u use) {
	if this.pure > 0 {
		return
	}
	if v.kind == vTable || v.kind == vRow {
		if u == useEscape || u == useCallee {
			v.table.tainted = "the table is handed on or modified in the script, its rows are not read as written"
		}
		return
	}
	if !v.relevant() {
		return
	}
	switch u {
	case useEscape:
		//whatever receives the handle may write into it
		this.apiChanged = true
		this.report(node, "a handle on "+v.describe()+" is handed on where its reads and writes are not followed")
	case useObject:
		if v.kind == vMethod {
			this.report(node, "the "+v.method+" function of "+v.describe()+" is taken off it, its calls are not followed")
		}
	}
}

func (this *scriptAnalysis) statements(list []ast.Statement) {
	for _, statement := range list {
		this.statement(statement)
	}
}

func (this *scriptAnalysis) declarations(list []*ast.Binding) {
	for _, binding := range list {
		identifier, isIdentifier := binding.Target.(*ast.Identifier)
		if !isIdentifier {
			this.expression(binding.Target, useDiscard)
		}
		if binding.Initializer == nil {
			continue
		}
		u := useEscape
		if isIdentifier && this.tracked(identifier.Name.String()) {
			u = useAlias
		}
		this.expression(binding.Initializer, u)
	}
}

func (this *scriptAnalysis) optionalExpression(expression ast.Expression, u use) {
	if !isNil(expression) {
		this.expression(expression, u)
	}
}

func (this *scriptAnalysis) statement(statement ast.Statement) {
	if isNil(statement) {
		return
	}
	this.depth++
	defer func() { this.depth-- }()
	if this.depth > maxDepth {
		this.tooDeep = true
		return
	}
	switch s := statement.(type) {
	case *ast.ExpressionStatement:
		this.expression(s.Expression, useDiscard)
	case *ast.VariableStatement:
		this.declarations(s.List)
	case *ast.LexicalDeclaration:
		this.declarations(s.List)
	case *ast.BlockStatement:
		this.statements(s.List)
	case *ast.IfStatement:
		this.expression(s.Test, useDiscard)
		this.statement(s.Consequent)
		this.statement(s.Alternate)
	case *ast.WhileStatement:
		this.expression(s.Test, useDiscard)
		this.statement(s.Body)
	case *ast.DoWhileStatement:
		this.statement(s.Body)
		this.expression(s.Test, useDiscard)
	case *ast.ForStatement:
		switch initializer := s.Initializer.(type) {
		case *ast.ForLoopInitializerExpression:
			this.expression(initializer.Expression, useDiscard)
		case *ast.ForLoopInitializerVarDeclList:
			this.declarations(initializer.List)
		case *ast.ForLoopInitializerLexicalDecl:
			this.declarations(initializer.LexicalDeclaration.List)
		}
		this.optionalExpression(s.Test, useDiscard)
		this.optionalExpression(s.Update, useDiscard)
		this.statement(s.Body)
	case *ast.ForInStatement:
		this.forInto(s.Into)
		this.expression(s.Source, useEscape)
		this.statement(s.Body)
	case *ast.ForOfStatement:
		this.forInto(s.Into)
		this.expression(s.Source, useEscape)
		this.statement(s.Body)
	case *ast.ReturnStatement:
		this.optionalExpression(s.Argument, useEscape)
	case *ast.ThrowStatement:
		this.optionalExpression(s.Argument, useEscape)
	case *ast.SwitchStatement:
		this.expression(s.Discriminant, useDiscard)
		for _, clause := range s.Body {
			this.optionalExpression(clause.Test, useDiscard)
			this.statements(clause.Consequent)
		}
	case *ast.TryStatement:
		this.statement(s.Body)
		if s.Catch != nil {
			if !isNil(s.Catch.Parameter) {
				this.expression(s.Catch.Parameter, useDiscard)
			}
			this.statement(s.Catch.Body)
		}
		this.statement(s.Finally)
	case *ast.LabelledStatement:
		this.statement(s.Statement)
	case *ast.WithStatement:
		this.report(s, "a with statement changes what the names in its body refer to, it is not followed")
		this.expression(s.Object, useEscape)
		this.statement(s.Body)
	case *ast.FunctionDeclaration:
		this.function(s.Function)
	case *ast.ClassDeclaration:
		this.expression(s.Class, useDiscard)
	case *ast.BranchStatement, *ast.EmptyStatement, *ast.DebuggerStatement, *ast.BadStatement:
	default:
		this.reportUnsupported(statement)
	}
}

func (this *scriptAnalysis) forInto(into ast.ForInto) {
	switch i := into.(type) {
	case *ast.ForIntoVar:
		if i.Binding.Initializer != nil {
			this.expression(i.Binding.Initializer, useEscape)
		}
	case *ast.ForIntoExpression:
		if !this.memberWrite(i.Expression) {
			this.expression(i.Expression, useDiscard)
		}
	}
}

func (this *scriptAnalysis) parameters(list *ast.ParameterList) {
	if list == nil {
		return
	}
	for _, binding := range list.List {
		if _, isIdentifier := binding.Target.(*ast.Identifier); !isIdentifier {
			this.expression(binding.Target, useDiscard)
		}
		this.optionalExpression(binding.Initializer, useEscape)
	}
}

func (this *scriptAnalysis) function(literal *ast.FunctionLiteral) {
	if literal == nil {
		return
	}
	this.parameters(literal.ParameterList)
	if literal.Body != nil {
		this.statements(literal.Body.List)
	}
}

// expression visits one expression and answers what it denotes. The value is
// checked against u here and only here, so a pass-through node - a sequence, a
// conditional, a logical operator - visits the part whose value it forwards
// with useDiscard and leaves the check to its own call.
func (this *scriptAnalysis) expression(expression ast.Expression, u use) value {
	if isNil(expression) {
		return unknown
	}
	this.depth++
	defer func() { this.depth-- }()
	if this.depth > maxDepth {
		this.tooDeep = true
		return unknown
	}
	result := this.denote(expression)
	this.check(expression, result, u)
	return result
}

func (this *scriptAnalysis) denote(expression ast.Expression) value {
	switch e := expression.(type) {
	case *ast.Identifier:
		name := e.Name.String()
		if (name == "eval" || name == "Function") && !this.poisoned[name] && len(this.bindings[name]) == 0 {
			this.report(e, "the script reaches "+name+", code it runs from a string is not analysed")
		}
		return this.alias(name)
	case *ast.StringLiteral:
		return value{kind: vString, literal: e.Value.String()}
	case *ast.TemplateLiteral:
		if text := literalString(e); text != nil {
			return value{kind: vString, literal: *text}
		}
		expressionUse := useDiscard
		if !isNil(e.Tag) {
			this.expression(e.Tag, useCallee)
			expressionUse = useEscape
		}
		for _, part := range e.Expressions {
			this.expression(part, expressionUse)
		}
		return unknown
	case *ast.ArrayLiteral:
		for _, element := range e.Value {
			this.optionalExpression(element, useEscape)
		}
		return value{kind: vTable, table: this.tableOf(e)}
	case *ast.ObjectLiteral:
		for _, property := range e.Value {
			this.expression(property, useDiscard)
		}
		return unknown
	case *ast.PropertyKeyed:
		if e.Computed {
			this.expression(e.Key, useDiscard)
		}
		this.expression(e.Value, useEscape)
		return unknown
	case *ast.PropertyShort:
		this.expression(&e.Name, useEscape)
		this.optionalExpression(e.Initializer, useEscape)
		return unknown
	case *ast.SpreadElement:
		this.expression(e.Expression, useEscape)
		return unknown
	case *ast.DotExpression:
		object := this.expression(e.Left, useObject)
		return this.named(e, e.Left, object, e.Identifier.Name.String())
	case *ast.PrivateDotExpression:
		this.expression(e.Left, useDiscard)
		return unknown
	case *ast.BracketExpression:
		return this.bracket(e)
	case *ast.CallExpression:
		return this.call(e)
	case *ast.NewExpression:
		this.expression(e.Callee, useCallee)
		for _, argument := range e.ArgumentList {
			this.expression(argument, useEscape)
		}
		return unknown
	case *ast.AssignExpression:
		return this.assign(e)
	case *ast.ConditionalExpression:
		this.expression(e.Test, useDiscard)
		consequent := this.expression(e.Consequent, useDiscard)
		alternate := this.expression(e.Alternate, useDiscard)
		if consequent.kind != vUnknown && consequent.identity() == alternate.identity() {
			return consequent
		}
		this.check(e.Consequent, consequent, useEscape)
		this.check(e.Alternate, alternate, useEscape)
		return unknown
	case *ast.BinaryExpression:
		return this.binary(e)
	case *ast.UnaryExpression:
		if e.Operator == token.INCREMENT || e.Operator == token.DECREMENT || e.Operator == token.DELETE {
			if this.memberWrite(e.Operand) {
				return unknown
			}
		}
		this.expression(e.Operand, useDiscard)
		return unknown
	case *ast.SequenceExpression:
		last := unknown
		for _, part := range e.Sequence {
			last = this.expression(part, useDiscard)
		}
		return last
	case *ast.FunctionLiteral:
		this.function(e)
		return unknown
	case *ast.ArrowFunctionLiteral:
		this.parameters(e.ParameterList)
		switch body := e.Body.(type) {
		case *ast.BlockStatement:
			this.statements(body.List)
		case *ast.ExpressionBody:
			this.expression(body.Expression, useEscape)
		}
		return unknown
	case *ast.ClassLiteral:
		this.optionalExpression(e.SuperClass, useEscape)
		for _, element := range e.Body {
			switch member := element.(type) {
			case *ast.FieldDefinition:
				if member.Computed {
					this.expression(member.Key, useDiscard)
				}
				this.optionalExpression(member.Initializer, useEscape)
			case *ast.MethodDefinition:
				if member.Computed {
					this.expression(member.Key, useDiscard)
				}
				this.function(member.Body)
			case *ast.ClassStaticBlock:
				if member.Block != nil {
					this.statements(member.Block.List)
				}
			}
		}
		return unknown
	case *ast.YieldExpression:
		this.optionalExpression(e.Argument, useEscape)
		return unknown
	case *ast.AwaitExpression:
		this.optionalExpression(e.Argument, useEscape)
		return unknown
	case *ast.OptionalChain:
		return this.expression(e.Expression, useDiscard)
	case *ast.Optional:
		return this.expression(e.Expression, useDiscard)
	case *ast.ArrayPattern:
		for _, element := range e.Elements {
			if !this.memberWrite(element) {
				this.optionalExpression(element, useDiscard)
			}
		}
		this.optionalExpression(e.Rest, useDiscard)
		return unknown
	case *ast.ObjectPattern:
		for _, property := range e.Properties {
			switch p := property.(type) {
			case *ast.PropertyKeyed:
				if p.Computed {
					this.expression(p.Key, useDiscard)
				}
				if !this.memberWrite(p.Value) {
					this.expression(p.Value, useDiscard)
				}
			case *ast.PropertyShort:
				this.optionalExpression(p.Initializer, useEscape)
			default:
				this.expression(property, useDiscard)
			}
		}
		this.optionalExpression(e.Rest, useDiscard)
		return unknown
	case *ast.Binding:
		this.optionalExpression(e.Initializer, useEscape)
		return unknown
	case *ast.NumberLiteral, *ast.BooleanLiteral, *ast.NullLiteral, *ast.RegExpLiteral,
		*ast.ThisExpression, *ast.SuperExpression, *ast.MetaProperty, *ast.BadExpression, *ast.PrivateIdentifier:
		return unknown
	}
	this.reportUnsupported(expression)
	return unknown
}

func (this *scriptAnalysis) tableOf(literal *ast.ArrayLiteral) *table {
	if known, ok := this.tables[literal]; ok {
		return known
	}
	result := newTable(literal)
	this.tables[literal] = result
	return result
}

// memberWrite visits the target of a write into a property: a table written
// to is tainted, and a write into the moses api changes what every access of
// the script means.
func (this *scriptAnalysis) memberWrite(target ast.Expression) bool {
	var object, member ast.Expression
	switch t := target.(type) {
	case *ast.DotExpression:
		object = t.Left
	case *ast.BracketExpression:
		object, member = t.Left, t.Member
	default:
		return false
	}
	owner := this.expression(object, useObject)
	this.optionalExpression(member, useDiscard)
	if this.pure > 0 {
		return true
	}
	switch {
	case owner.kind == vTable || owner.kind == vRow:
		owner.table.tainted = "the table is handed on or modified in the script, its rows are not read as written"
	case owner.api():
		this.apiChanged = true
		this.report(target, "the script writes into "+owner.describe()+" of the moses api, what it reads and writes is not followed")
	}
	return true
}

// codeName is true for a name through which a string can be run as code.
func codeName(name string) bool {
	return name == "eval" || name == "Function" || name == "constructor"
}

func (this *scriptAnalysis) member(object value, name string) value {
	switch object.kind {
	case vMoses:
		switch name {
		case "environment", "world":
			return value{kind: vEnvironment}
		case "zone", "room":
			return value{kind: vZone, zone: ref{own: true}}
		case "asset", "device":
			return value{kind: vAsset, zone: ref{own: true}, asset: ref{own: true}}
		case "channel", "service":
			return value{kind: vChannel}
		}
	case vEnvironment:
		switch name {
		case "state":
			return value{kind: vState, scope: scopeContext}
		case "getRoom":
			receiver := object
			return value{kind: vMethod, method: name, receiver: &receiver}
		}
	case vZone:
		switch name {
		case "state":
			return value{kind: vState, scope: scopeZone, zone: object.zone}
		case "getDevice":
			receiver := object
			return value{kind: vMethod, method: name, receiver: &receiver}
		}
	case vAsset:
		if name == "state" {
			return value{kind: vState, scope: scopeAsset, zone: object.zone, asset: object.asset}
		}
	case vState:
		if name == "get" || name == "set" {
			receiver := object
			return value{kind: vMethod, method: name, receiver: &receiver}
		}
	case vTable, vRow:
		if name != "length" && this.pure == 0 {
			object.table.tainted = "the table is handed on or modified in the script, its rows are not read as written"
		}
	}
	return unknown
}

func integerIndex(expression ast.Expression) (int, bool) {
	literal, ok := expression.(*ast.NumberLiteral)
	if !ok {
		return 0, false
	}
	switch number := literal.Value.(type) {
	case int64:
		if number >= 0 && number <= math.MaxInt32 {
			return int(number), true
		}
	case float64:
		if number >= 0 && number <= math.MaxInt32 && number == math.Trunc(number) {
			return int(number), true
		}
	}
	return 0, false
}

// named is a member read by a name the script spells out.
func (this *scriptAnalysis) named(node ast.Node, left ast.Expression, object value, name string) value {
	if codeName(name) {
		this.report(node, "the script reaches "+name+", code it runs from a string is not analysed")
	}
	if name == "moses" && this.isGlobalObject(left) {
		this.report(node, "the script reaches moses through the global object, what it reads and writes through it is not followed")
	}
	return this.member(object, name)
}

func (this *scriptAnalysis) bracket(e *ast.BracketExpression) value {
	object := this.expression(e.Left, useObject)
	if name := this.expression(e.Member, useDiscard); name.kind == vString {
		return this.named(e, e.Left, object, name.literal)
	}
	switch object.kind {
	case vTable:
		index, ok := tableIndexOf(e.Member)
		if !ok {
			return unknown
		}
		if object.table.rows != nil {
			return value{kind: vRow, table: object.table, index: index}
		}
		return value{kind: vCell, table: object.table, index: index, column: -1}
	case vRow:
		if column, ok := integerIndex(e.Member); ok {
			return value{kind: vCell, table: object.table, index: object.index, column: column}
		}
		return unknown
	}
	if object.relevant() {
		this.report(e, "the member of "+object.describe()+" is computed at run time")
	}
	return unknown
}

func tableIndexOf(member ast.Expression) (tableIndex, bool) {
	if identifier, ok := member.(*ast.Identifier); ok {
		return tableIndex{name: identifier.Name.String()}, true
	}
	if fixed, ok := integerIndex(member); ok {
		return tableIndex{fixed: fixed, isFixed: true}, true
	}
	return tableIndex{}, false
}

// noValue is true for an argument that arrives in the runtime as nil, which
// jsStateApi takes for no field name or no value and does nothing with.
func (this *scriptAnalysis) noValue(argument ast.Expression) bool {
	switch a := argument.(type) {
	case *ast.NullLiteral:
		return true
	case *ast.Identifier:
		return a.Name.String() == "undefined" && !this.poisoned["undefined"] && len(this.bindings["undefined"]) == 0
	case *ast.UnaryExpression:
		return a.Operator == token.VOID
	}
	return false
}

func (this *scriptAnalysis) call(e *ast.CallExpression) value {
	callee := this.expression(e.Callee, useCallee)
	if callee.kind != vMethod {
		for _, argument := range e.ArgumentList {
			this.expression(argument, useEscape)
		}
		return unknown
	}
	var result value
	switch callee.method {
	case "getRoom":
		result = value{kind: vZone, zone: ref{term: this.argument(e, 0, "zone id")}}
	case "getDevice":
		result = value{kind: vAsset, zone: callee.receiver.zone, asset: ref{term: this.argument(e, 0, "asset id")}}
	case "get", "set":
		arguments := e.ArgumentList
		if callee.method == "set" && len(arguments) > 1 {
			this.expression(arguments[1], useEscape)
		}
		noop := len(arguments) == 0 || this.noValue(arguments[0]) ||
			callee.method == "set" && (len(arguments) < 2 || this.noValue(arguments[1]))
		if noop {
			if len(arguments) > 0 {
				this.expression(arguments[0], useDiscard)
			}
			result = unknown
			break
		}
		key := this.argument(e, 0, "key")
		if this.pure == 0 && callee.receiver.relevant() {
			this.accesses = append(this.accesses, access{
				write: callee.method == "set", state: *callee.receiver, key: key, node: e,
			})
		}
		result = unknown
	}
	for i := 1; i < len(e.ArgumentList); i++ {
		if callee.method == "set" && i == 1 {
			continue
		}
		this.expression(e.ArgumentList[i], useDiscard)
	}
	return result
}

// argument turns the argument at position i into a term. Only a string
// literal, an alias of one and a cell of a literal table are read.
func (this *scriptAnalysis) argument(e *ast.CallExpression, i int, noun string) term {
	if i >= len(e.ArgumentList) {
		return term{kind: termBad, reason: "the call passes no " + noun}
	}
	argument := e.ArgumentList[i]
	v := this.expression(argument, useDiscard)
	switch v.kind {
	case vString:
		return term{kind: termLiteral, literal: v.literal}
	case vCell:
		return term{kind: termCell, table: v.table, index: v.index, column: v.column}
	}
	switch argument.(type) {
	case *ast.BinaryExpression, *ast.TemplateLiteral, *ast.CallExpression, *ast.ConditionalExpression:
		return term{kind: termBad, reason: "the " + noun + " is computed at run time"}
	case *ast.Identifier:
		return term{kind: termBad, reason: "the " + noun + " is held in a variable that is not bound to one string literal"}
	case *ast.NumberLiteral:
		return term{kind: termBad, reason: "the " + noun + " is a number, only string literals are read"}
	case *ast.BracketExpression:
		return term{kind: termBad, reason: "the " + noun + " is read from something that is not a literal table of strings"}
	}
	return term{kind: termBad, reason: "the " + noun + " is not a string literal"}
}

func (this *scriptAnalysis) assign(e *ast.AssignExpression) value {
	switch left := e.Left.(type) {
	case *ast.Identifier:
		if e.Operator != token.ASSIGN {
			rightUse := useDiscard
			if e.Operator == token.LOGICAL_OR || e.Operator == token.LOGICAL_AND || e.Operator == token.COALESCE {
				rightUse = useEscape
			}
			this.expression(e.Right, rightUse)
			return unknown
		}
		rightUse := useEscape
		if this.tracked(left.Name.String()) {
			rightUse = useAlias
		}
		return this.expression(e.Right, rightUse)
	case *ast.DotExpression, *ast.BracketExpression:
		this.memberWrite(left)
		this.expression(e.Right, useEscape)
		return unknown
	}
	this.expression(e.Left, useDiscard)
	this.expression(e.Right, useEscape)
	return unknown
}

func (this *scriptAnalysis) binary(e *ast.BinaryExpression) value {
	switch e.Operator {
	case token.LOGICAL_OR, token.COALESCE:
		if this.noValue(e.Left) {
			//null and undefined are falsy and nullish, so the right operand is the result
			this.expression(e.Left, useDiscard)
			return this.expression(e.Right, useDiscard)
		}
		left := this.expression(e.Left, useDiscard)
		if left.object() {
			//an object is truthy and not nullish, so the left operand is the result
			this.expression(e.Right, useDiscard)
			return left
		}
		this.check(e.Left, left, useEscape)
		this.expression(e.Right, useEscape)
		return unknown
	case token.LOGICAL_AND:
		left := this.expression(e.Left, useDiscard)
		if left.object() {
			return this.expression(e.Right, useDiscard)
		}
		this.check(e.Left, left, useEscape)
		this.expression(e.Right, useEscape)
		return unknown
	}
	left, right := this.expression(e.Left, useDiscard), this.expression(e.Right, useDiscard)
	if e.Operator == token.PLUS && left.kind == vString && right.kind == vString {
		//folded so that a name built from pieces, 'constr' + 'uctor', is still seen
		return value{kind: vString, literal: left.literal + right.literal}
	}
	return unknown
}

// --- resolution against the document ----------------------------------------------

// scriptSink applies the per script caps on top of the document caps of the
// builder; once a cap is hit the script is truncated with one entry.
type scriptSink struct {
	b          *builder
	asset      string
	channel    string
	edges      int
	unresolved int
	rows       int
	truncated  bool
}

func (this *scriptSink) truncate(reason string) {
	if this.truncated {
		return
	}
	this.truncated = true
	this.b.unresolved = append(this.b.unresolved, Unresolved{Asset: this.asset, Channel: this.channel, Reason: reason})
}

func (this *scriptSink) edge(key edgeKey, count int) {
	if this.truncated {
		return
	}
	if _, known := this.b.edges[key]; !known {
		if this.edges >= limits.edgesPerScript {
			this.truncate(fmt.Sprintf("the script has more effects than the analysis reports (at most %d edges), the rest of it is left out", limits.edgesPerScript))
			return
		}
		this.edges++
	}
	this.b.addEdge(key.from, key.to, key.kind, key.via, key.channel, key.key, count)
}

func (this *scriptSink) problem(expression string, reason string) {
	if this.truncated {
		return
	}
	if this.unresolved >= limits.unresolvedPerScript {
		this.truncate(fmt.Sprintf("the script has more unresolved references than the analysis reports (at most %d), the rest of it is left out", limits.unresolvedPerScript))
		return
	}
	this.unresolved++
	this.b.addUnresolved(Unresolved{Asset: this.asset, Channel: this.channel, Expression: snippet(expression), Reason: reason})
}

// scriptEdges analyses one script channel and adds what it reads and writes.
func (this *builder) scriptEdges(entry *assetEntry, channel domain.Channel) {
	sink := &scriptSink{b: this, asset: entry.asset.Id, channel: channel.Id}
	code := channel.Source.Script.Code
	if reason := prescan(code); reason != "" {
		sink.problem(code, reason)
		return
	}
	err := analyseScript(code, func(a *scriptAnalysis) { this.resolveScript(entry, channel, a, sink) })
	if err != nil {
		sink.problem(code, err.Error())
	}
}

// analyseScript parses and analyses code and hands the analysis to resolve. A
// panic inside the parser or the analysis is turned into an error, so one
// script cannot fail the derivation of a whole document.
func analyseScript(code string, resolve func(a *scriptAnalysis)) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("the script could not be analysed: %v", recovered)
		}
	}()
	program, parseErr := parseScript(code)
	if parseErr != nil {
		return fmt.Errorf("the script does not parse: %v", parseErr)
	}
	analysis := newScriptAnalysis(code)
	analysis.run(program)
	resolve(analysis)
	return nil
}

// shape is what an access resolves to. Accesses of the same shape resolve the
// same, so a table is walked once per shape however often it is read.
type shape struct {
	edges    map[edgeKey]int
	problems []string
	sites    int
}

func accessShape(acc access) string {
	return fmt.Sprintf("%t|%s|%s", acc.write, acc.state.identity(), acc.key.id())
}

func (this *builder) resolveScript(entry *assetEntry, channel domain.Channel, a *scriptAnalysis, sink *scriptSink) {
	shapes := map[string]*shape{}
	order := []string{}
	for _, acc := range a.accesses {
		id := accessShape(acc)
		if known, ok := shapes[id]; ok {
			known.sites++
			continue
		}
		shapes[id] = &shape{sites: 1}
		order = append(order, id)
		shapes[id].edges, shapes[id].problems = this.resolveAccess(entry, channel, acc, a, sink)
		if sink.truncated {
			return
		}
	}
	for _, id := range order {
		for _, edge := range sortedEdges(shapes[id].edges) {
			sink.edge(edge.key, edge.count*shapes[id].sites)
		}
	}

	type site struct {
		position int
		node     ast.Node
		reasons  []string
	}
	sites := []site{}
	for _, f := range a.findings {
		sites = append(sites, site{position: a.position(f.node), node: f.node, reasons: []string{f.reason}})
	}
	for _, acc := range a.accesses {
		if problems := shapes[accessShape(acc)].problems; len(problems) > 0 {
			sites = append(sites, site{position: a.position(acc.node), node: acc.node, reasons: problems})
		}
	}
	sort.SliceStable(sites, func(i, j int) bool { return sites[i].position < sites[j].position })
	for _, s := range sites {
		for _, reason := range s.reasons {
			sink.problem(a.source(s.node), reason)
		}
	}
	if a.tooDeep {
		sink.problem("", "the script nests deeper than the analysis follows, the rest of it is not read")
	}
}

type edgeCount struct {
	key   edgeKey
	count int
}

// sortedEdges hands the edges of a shape out in a fixed order, so which edge a
// cap cuts off does not depend on map iteration.
func sortedEdges(edges map[edgeKey]int) []edgeCount {
	result := make([]edgeCount, 0, len(edges))
	for key, count := range edges {
		result = append(result, edgeCount{key: key, count: count})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].key.less(result[j].key) })
	return result
}

func (this *builder) resolveAccess(entry *assetEntry, channel domain.Channel, acc access, a *scriptAnalysis, sink *scriptSink) (map[edgeKey]int, []string) {
	edges := map[edgeKey]int{}
	problems := []string{}
	problem := func(reason string) { problems = append(problems, reason) }
	if a.apiChanged {
		problem("the script changes the moses api or hands a handle on (see its other entries), what it reads and writes is not followed")
		return edges, problems
	}

	terms := []*term{}
	var zoneTerm, assetTerm *term
	switch acc.state.scope {
	case scopeZone:
		if !acc.state.zone.own {
			zoneTerm = &acc.state.zone.term
		}
	case scopeAsset:
		if !acc.state.zone.own {
			zoneTerm = &acc.state.zone.term
		}
		assetTerm = &acc.state.asset.term
	}
	for _, t := range []*term{zoneTerm, assetTerm, &acc.key} {
		if t == nil {
			continue
		}
		if t.kind == termBad {
			problem(t.reason)
			return edges, problems
		}
		terms = append(terms, t)
	}

	rows := []int{-1}
	var cell *term
	for _, t := range terms {
		if t.kind != termCell {
			continue
		}
		if cell == nil {
			cell = t
			continue
		}
		if t.table != cell.table || t.index.id() != cell.index.id() {
			problem("the arguments are read from different tables or rows, which rows belong together is not followed")
			return edges, problems
		}
	}
	if cell != nil {
		if cell.table.tainted != "" {
			problem(cell.table.tainted)
			return edges, problems
		}
		count := cell.table.length(cell.column)
		rows = rows[:0]
		if cell.index.isFixed {
			if cell.index.fixed >= count {
				problem(fmt.Sprintf("the table has no row %d", cell.index.fixed))
				return edges, problems
			}
			rows = append(rows, cell.index.fixed)
		} else {
			if sink.rows+count > limits.rowsPerScript || this.rows+count > limits.rowsPerDocument {
				sink.truncate(fmt.Sprintf("the script's tables are larger than the analysis reads (at most %d rows per script, %d per document), the rest of it is left out",
					limits.rowsPerScript, limits.rowsPerDocument))
				return edges, problems
			}
			sink.rows += count
			this.rows += count
			for row := 0; row < count; row++ {
				rows = append(rows, row)
			}
		}
	}

	self := assetNodeId(entry.asset.Id)
	for _, row := range rows {
		prefix := ""
		if row >= 0 {
			prefix = describeRow(row) + ": "
		}
		concrete := func(t *term) (string, bool) {
			if t.kind == termLiteral {
				return t.literal, true
			}
			text := t.table.cell(row, t.column)
			if text == nil {
				position := "element"
				if t.column >= 0 {
					position = fmt.Sprintf("position %d", t.column)
				}
				problem(fmt.Sprintf("%sthere is no string literal at %s", prefix, position))
				return "", false
			}
			return *text, true
		}
		key, ok := concrete(&acc.key)
		if !ok {
			continue
		}
		//jsStateApi refuses an empty field name, nothing is read or written
		if key == "" {
			continue
		}
		zoneId := entry.zoneId
		if zoneTerm != nil {
			if zoneId, ok = concrete(zoneTerm); !ok {
				continue
			}
		}
		var other string
		switch acc.state.scope {
		case scopeContext:
			if acc.write && this.governed[key] {
				problem(prefix + "write to a timeline-governed key is dropped at runtime")
				continue
			}
			other = contextNodeId(key)
		case scopeZone:
			if _, known := this.zones[zoneId]; !known {
				problem(fmt.Sprintf("%sno zone %q in the document", prefix, zoneId))
				continue
			}
			other = zoneNodeId(zoneId)
		case scopeAsset:
			assetId, ok := concrete(assetTerm)
			if !ok {
				continue
			}
			if _, known := this.zones[zoneId]; !known {
				problem(fmt.Sprintf("%sno zone %q in the document", prefix, zoneId))
				continue
			}
			target, known := this.assets[assetId]
			if !known || target.zoneId != zoneId {
				problem(fmt.Sprintf("%sno asset %q directly in zone %q, getDevice finds nothing there", prefix, assetId, zoneId))
				continue
			}
			if assetId == entry.asset.Id {
				continue
			}
			other = assetNodeId(assetId)
		}
		if acc.write {
			edges[edgeKey{from: self, to: other, kind: EdgeWrites, via: ViaScript, channel: channel.Id, key: key}]++
		} else {
			edges[edgeKey{from: other, to: self, kind: EdgeReads, via: ViaScript, channel: channel.Id, key: key}]++
		}
	}
	return edges, problems
}
