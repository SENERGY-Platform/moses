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

import "github.com/dop251/goja/ast"

// forEachChild calls visit for every direct child of node. It answers false
// for a node type it does not know, so a new goja node type is reported rather
// than silently skipped with every binding below it.
func forEachChild(node ast.Node, visit func(ast.Node)) bool {
	each := func(children ...ast.Node) {
		for _, child := range children {
			if !isNil(child) {
				visit(child)
			}
		}
	}
	expressions := func(list []ast.Expression) {
		for _, child := range list {
			each(child)
		}
	}
	statements := func(list []ast.Statement) {
		for _, child := range list {
			each(child)
		}
	}
	bindings := func(list []*ast.Binding) {
		for _, child := range list {
			each(child)
		}
	}
	switch n := node.(type) {
	case *ast.ArrayLiteral:
		expressions(n.Value)
	case *ast.ArrayPattern:
		expressions(n.Elements)
		each(n.Rest)
	case *ast.AssignExpression:
		each(n.Left, n.Right)
	case *ast.AwaitExpression:
		each(n.Argument)
	case *ast.YieldExpression:
		each(n.Argument)
	case *ast.BinaryExpression:
		each(n.Left, n.Right)
	case *ast.BracketExpression:
		each(n.Left, n.Member)
	case *ast.CallExpression:
		each(n.Callee)
		expressions(n.ArgumentList)
	case *ast.NewExpression:
		each(n.Callee)
		expressions(n.ArgumentList)
	case *ast.ConditionalExpression:
		each(n.Test, n.Consequent, n.Alternate)
	case *ast.DotExpression:
		each(n.Left)
	case *ast.PrivateDotExpression:
		each(n.Left)
	case *ast.OptionalChain:
		each(n.Expression)
	case *ast.Optional:
		each(n.Expression)
	case *ast.FunctionLiteral:
		if n.ParameterList != nil {
			each(n.ParameterList)
		}
		if n.Body != nil {
			each(n.Body)
		}
	case *ast.ArrowFunctionLiteral:
		if n.ParameterList != nil {
			each(n.ParameterList)
		}
		each(n.Body)
	case *ast.ExpressionBody:
		each(n.Expression)
	case *ast.ClassLiteral:
		each(n.SuperClass)
		for _, element := range n.Body {
			each(element)
		}
	case *ast.FieldDefinition:
		each(n.Key, n.Initializer)
	case *ast.MethodDefinition:
		each(n.Key)
		if n.Body != nil {
			each(n.Body)
		}
	case *ast.ClassStaticBlock:
		if n.Block != nil {
			each(n.Block)
		}
	case *ast.ObjectLiteral:
		for _, property := range n.Value {
			each(property)
		}
	case *ast.ObjectPattern:
		for _, property := range n.Properties {
			each(property)
		}
		each(n.Rest)
	case *ast.ParameterList:
		bindings(n.List)
		each(n.Rest)
	case *ast.PropertyShort:
		each(&n.Name, n.Initializer)
	case *ast.PropertyKeyed:
		each(n.Key, n.Value)
	case *ast.SpreadElement:
		each(n.Expression)
	case *ast.SequenceExpression:
		expressions(n.Sequence)
	case *ast.TemplateLiteral:
		each(n.Tag)
		expressions(n.Expressions)
	case *ast.UnaryExpression:
		each(n.Operand)
	case *ast.Binding:
		each(n.Target, n.Initializer)
	case *ast.Identifier, *ast.PrivateIdentifier, *ast.BooleanLiteral, *ast.NullLiteral, *ast.NumberLiteral,
		*ast.StringLiteral, *ast.RegExpLiteral, *ast.ThisExpression, *ast.SuperExpression, *ast.MetaProperty,
		*ast.BadExpression, *ast.TemplateElement:

	case *ast.BlockStatement:
		statements(n.List)
	case *ast.CaseStatement:
		each(n.Test)
		statements(n.Consequent)
	case *ast.CatchStatement:
		each(n.Parameter)
		if n.Body != nil {
			each(n.Body)
		}
	case *ast.DoWhileStatement:
		each(n.Body, n.Test)
	case *ast.ExpressionStatement:
		each(n.Expression)
	case *ast.ForInStatement:
		each(n.Into, n.Source, n.Body)
	case *ast.ForOfStatement:
		each(n.Into, n.Source, n.Body)
	case *ast.ForStatement:
		each(n.Initializer, n.Test, n.Update, n.Body)
	case *ast.IfStatement:
		each(n.Test, n.Consequent, n.Alternate)
	case *ast.LabelledStatement:
		each(n.Statement)
	case *ast.ReturnStatement:
		each(n.Argument)
	case *ast.SwitchStatement:
		each(n.Discriminant)
		for _, clause := range n.Body {
			if clause != nil {
				each(clause)
			}
		}
	case *ast.ThrowStatement:
		each(n.Argument)
	case *ast.TryStatement:
		if n.Body != nil {
			each(n.Body)
		}
		if n.Catch != nil {
			each(n.Catch)
		}
		if n.Finally != nil {
			each(n.Finally)
		}
	case *ast.VariableStatement:
		bindings(n.List)
	case *ast.LexicalDeclaration:
		bindings(n.List)
	case *ast.WhileStatement:
		each(n.Test, n.Body)
	case *ast.WithStatement:
		each(n.Object, n.Body)
	case *ast.FunctionDeclaration:
		if n.Function != nil {
			each(n.Function)
		}
	case *ast.ClassDeclaration:
		if n.Class != nil {
			each(n.Class)
		}
	case *ast.BadStatement, *ast.BranchStatement, *ast.DebuggerStatement, *ast.EmptyStatement:

	case *ast.ForLoopInitializerExpression:
		each(n.Expression)
	case *ast.ForLoopInitializerVarDeclList:
		bindings(n.List)
	case *ast.ForLoopInitializerLexicalDecl:
		each(&n.LexicalDeclaration)
	case *ast.ForIntoVar:
		if n.Binding != nil {
			each(n.Binding)
		}
	case *ast.ForDeclaration:
		each(n.Target)
	case *ast.ForIntoExpression:
		each(n.Expression)
	default:
		return false
	}
	return true
}
