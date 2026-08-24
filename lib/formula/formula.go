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

// Package formula compiles and evaluates the expressions of formula sources.
// expr is not turing complete - no loops, guaranteed termination - which is
// what allows a formula to run under the environment mutex without any of the
// interrupt machinery a script needs.
package formula

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// Reference prefixes: every input names where its value comes from, so a
// reader never has to guess which scope a bare key would have matched.
const (
	RefContext = "context."
	RefZone    = "zone."
	RefAsset   = "asset."
	RefChannel = "channel."
)

var identifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Program is one compiled formula, evaluated with the resolved input values.
type Program struct {
	program *vm.Program
}

// Compile checks the expression the way the csv parser checks an upload: at
// store time, with a message, instead of a channel that fails on every tick.
// The trial run with zero-valued inputs is what catches a variable the inputs
// do not declare - expr resolves map variables at run time, not at compile
// time, so compiling alone would let it through.
func Compile(expression string, inputs map[string]string) (*Program, error) {
	if strings.TrimSpace(expression) == "" {
		return nil, fmt.Errorf("the expression must not be empty")
	}
	trial := make(map[string]interface{}, len(inputs))
	for name, ref := range inputs {
		if !identifier.MatchString(name) {
			return nil, fmt.Errorf("input %q is not usable as a variable name", name)
		}
		if err := checkReference(ref); err != nil {
			return nil, fmt.Errorf("input %q: %w", name, err)
		}
		trial[name] = 0.0
	}
	//the trial map doubles as the compile environment: without it an input
	//named like one of expr's builtins (last, min, count, ...) would resolve
	//to the builtin instead of the variable
	program, err := expr.Compile(expression, expr.Env(trial))
	if err != nil {
		return nil, fmt.Errorf("unable to compile the expression: %w", err)
	}
	result, err := expr.Run(program, trial)
	if err != nil {
		return nil, fmt.Errorf("the expression does not evaluate (tried with all inputs at 0): %w", err)
	}
	if _, ok := toFloat(result); !ok {
		return nil, fmt.Errorf("the expression yields %T, a formula channel publishes numbers", result)
	}
	return &Program{program: program}, nil
}

func checkReference(ref string) error {
	for _, prefix := range []string{RefContext, RefZone, RefAsset, RefChannel} {
		if strings.HasPrefix(ref, prefix) && len(ref) > len(prefix) {
			return nil
		}
	}
	return fmt.Errorf("the reference %q must start with %q, %q, %q or %q and name a key", ref, RefContext, RefZone, RefAsset, RefChannel)
}

// Evaluate runs the compiled expression with the given input values.
func (this *Program) Evaluate(values map[string]interface{}) (float64, error) {
	result, err := expr.Run(this.program, values)
	if err != nil {
		return 0, err
	}
	value, ok := toFloat(result)
	if !ok {
		return 0, fmt.Errorf("the expression yielded %T, expected a number", result)
	}
	return value, nil
}

func toFloat(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case bool:
		//comparisons are legitimate formulas: a threshold flag publishes 0 or 1
		if typed {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}
