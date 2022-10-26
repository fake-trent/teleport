/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package predicate

import (
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/trace"
	"github.com/vulcand/predicate"
)

// NamedParameter is an object with a name that can be added to the environment in which a predicate expression runs.
type NamedParameter interface {
	// GetName returns the name of the object.
	GetName() string
}

func buildEnv(objects ...NamedParameter) map[string]any {
	env := make(map[string]any)
	for _, obj := range objects {
		env[obj.GetName()] = obj
	}

	return env
}

// PredicateAccessChecker checks access/permissions to access certain resources by evaluating AccessPolicy resources.
type PredicateAccessChecker struct {
	polices []types.Policy
}

// NewPredicateAccessChecker creates a new PredicateAccessChecker with a set of policies describing the permissions.
func NewPredicateAccessChecker(policies []types.Policy) *PredicateAccessChecker {
	return &PredicateAccessChecker{policies}
}

// CheckAccessToNode checks if a given user has access to a Server Access node.
func (c *PredicateAccessChecker) CheckAccessToNode(node *Node, user *User) (bool, error) {
	env := buildEnv(node, user)
	return c.checkPolicyExprs("node", env)
}

// checkPolicyExprs is the internal routine that evaluates expressions in a given scope from all policies
// with a provided execution environment containing input values.
func (c *PredicateAccessChecker) checkPolicyExprs(scope string, env map[string]any) (bool, error) {
	getIdentifierInEnv := func(selector []string) (any, error) {
		return getIdentifier(env, selector)
	}

	parser, err := predicate.NewParser(predicate.Def{
		Operators: predicate.Operators{
			AND: predicate.And,
			OR:  predicate.Or,
			NOT: predicate.Not,
			EQ:  builtinOpEquals,
			LT:  builtinOpLT,
			GT:  builtinOpGT,
			LE:  builtinOpLE,
			GE:  builtinOpGE,
		},
		Functions: map[string]any{
			"add":      builtinAdd,
			"sub":      builtinSub,
			"mul":      builtinMul,
			"div":      builtinDiv,
			"xor":      builtinXor,
			"split":    builtinSplit,
			"upper":    builtinUpper,
			"lower":    builtinLower,
			"contains": builtinContains,
			"first":    builtinFirst,
			"append":   builtinAppend,
			"array":    builtinArray,
		},
		GetIdentifier: getIdentifierInEnv,
		GetProperty:   getProperty,
	})
	if err != nil {
		return false, trace.Wrap(err)
	}

	evaluate := func(expr string) (bool, error) {
		ifn, err := parser.Parse(expr)
		if err != nil {
			return false, trace.Wrap(err)
		}

		fn, ok := ifn.(predicate.BoolPredicate)
		if !ok {
			return false, trace.BadParameter("unsupported type: %T", ifn)
		}

		return fn(), nil
	}

	for _, policy := range c.polices {
		if expr, ok := policy.GetDeny()[scope]; ok {
			denied, err := evaluate(expr)
			if err != nil {
				return false, trace.Wrap(err)
			}

			if denied {
				return false, nil
			}
		}
	}

	for _, policy := range c.polices {
		if expr, ok := policy.GetAllow()[scope]; ok {
			allowed, err := evaluate(expr)
			if err != nil {
				return false, trace.Wrap(err)
			}

			if allowed {
				return true, nil
			}
		}
	}

	return false, nil
}

// Node describes a Server Access node.
type Node struct {
	Login  string            `json:"login"`
	Labels map[string]string `json:"labels"`
}

// GetName returns the name of the object.
func (n *Node) GetName() string {
	return "node"
}

// User describes a Teleport user.
type User struct {
	Name   string              `json:"name"`
	Traits map[string][]string `json:"traits"`
}

// GetName returns the name of the object.
func (u *User) GetName() string {
	return "user"
}