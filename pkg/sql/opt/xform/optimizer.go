// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package xform

import (
	"math/rand"

	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/norm"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/ordering"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props/physical"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// MatchedRuleFunc defines the callback function for the NotifyOnMatchedRule
// event supported by the optimizer. See the comment in factory.go for more
// details.
type MatchedRuleFunc = norm.MatchedRuleFunc

// AppliedRuleFunc defines the callback function for the NotifyOnAppliedRule
// event supported by the optimizer. See the comment in factory.go for more
// details.
type AppliedRuleFunc = norm.AppliedRuleFunc

// RuleSet efficiently stores an unordered set of RuleNames.
type RuleSet = util.FastIntSet

// Optimizer transforms an input expression tree into the logically equivalent
// output expression tree with the lowest possible execution cost.
//
// To use the optimizer, construct an input expression tree by invoking
// construction methods on the Optimizer.Factory instance. The factory
// transforms the input expression into its canonical form as part of
// construction. Pass the root of the tree constructed by the factory to the
// Optimize method, along with a set of required physical properties that the
// expression must provide. The optimizer will return an Expr over the output
// expression tree with the lowest cost.
type Optimizer struct {
	evalCtx *tree.EvalContext

	// f is the factory that creates the normalized expressions during the first
	// optimization phase.
	f norm.Factory

	// catalog is the Catalog object that's used to resolve SQL names.
	catalog cat.Catalog

	// mem is the Memo data structure containing the forest of query plans that
	// are generated by the optimizer.
	mem *memo.Memo

	// explorer generates alternate, equivalent expressions and stores them in
	// the memo.
	explorer explorer

	// defaultCoster implements the default cost model. If SetCoster is not
	// called, this coster will be used.
	defaultCoster coster

	// coster is set by default to reference defaultCoster, but can be overridden
	// by calling SetCoster.
	coster Coster

	// stateMap allocates temporary storage that's used to speed up optimization.
	// This state could be discarded once optimization is complete.
	stateMap   map[groupStateKey]*groupState
	stateAlloc groupStateAlloc

	// matchedRule is the callback function that is invoked each time an
	// optimization rule (Normalize or Explore) has been matched by the optimizer.
	// It can be set via a call to the NotifyOnMatchedRule method.
	matchedRule MatchedRuleFunc

	// appliedRule is the callback function which is invoked each time an
	// optimization rule (Normalize or Explore) has been applied by the optimizer.
	// It can be set via a call to the NotifyOnAppliedRule method.
	appliedRule AppliedRuleFunc

	// disabledRules is a set of rules that are not allowed to run, used for
	// testing.
	disabledRules RuleSet

	// JoinOrderBuilder adds new join orderings to the memo.
	jb JoinOrderBuilder
}

// Init initializes the Optimizer with a new, blank memo structure inside. This
// must be called before the optimizer can be used (or reused).
func (o *Optimizer) Init(evalCtx *tree.EvalContext, catalog cat.Catalog) {
	// This initialization pattern ensures that fields are not unwittingly
	// reused. Field reuse must be explicit.
	*o = Optimizer{
		evalCtx:  evalCtx,
		catalog:  catalog,
		f:        o.f,
		stateMap: make(map[groupStateKey]*groupState),
	}
	o.f.Init(evalCtx, catalog)
	o.mem = o.f.Memo()
	o.explorer.init(o)
	o.defaultCoster.Init(evalCtx, o.mem, evalCtx.TestingKnobs.OptimizerCostPerturbation)
	o.coster = &o.defaultCoster
	if evalCtx.TestingKnobs.DisableOptimizerRuleProbability > 0 {
		o.disableRules(evalCtx.TestingKnobs.DisableOptimizerRuleProbability)
	}
}

// DetachMemo extracts the memo from the optimizer, and then re-initializes the
// optimizer so that its reuse will not impact the detached memo. This method is
// used to extract a read-only memo during the PREPARE phase.
func (o *Optimizer) DetachMemo() *memo.Memo {
	detach := o.f.DetachMemo()
	o.Init(o.evalCtx, o.catalog)
	return detach
}

// Factory returns a factory interface that the caller uses to construct an
// input expression tree. The root of the resulting tree can be passed to the
// Optimize method in order to find the lowest cost plan.
func (o *Optimizer) Factory() *norm.Factory {
	return &o.f
}

// Coster returns the coster instance that the optimizer is currently using to
// estimate the cost of executing portions of the expression tree. When a new
// optimizer is constructed, it creates a default coster that will be used
// unless it is overridden with a call to SetCoster.
func (o *Optimizer) Coster() Coster {
	return o.coster
}

// SetCoster overrides the default coster. The optimizer will now use the given
// coster to estimate the cost of expression execution.
func (o *Optimizer) SetCoster(coster Coster) {
	o.coster = coster
}

// JoinOrderBuilder returns the JoinOrderBuilder instance that the optimizer is
// currently using to reorder join trees.
func (o *Optimizer) JoinOrderBuilder() *JoinOrderBuilder {
	return &o.jb
}

// DisableOptimizations disables all transformation rules, including normalize
// and explore rules. The unaltered input expression tree becomes the output
// expression tree (because no transforms are applied).
func (o *Optimizer) DisableOptimizations() {
	o.NotifyOnMatchedRule(func(opt.RuleName) bool { return false })
}

// NotifyOnMatchedRule sets a callback function which is invoked each time an
// optimization rule (Normalize or Explore) has been matched by the optimizer.
// If matchedRule is nil, then no notifications are sent, and all rules are
// applied by default. In addition, callers can invoke the DisableOptimizations
// convenience method to disable all rules.
func (o *Optimizer) NotifyOnMatchedRule(matchedRule MatchedRuleFunc) {
	o.matchedRule = matchedRule

	// Also pass through the call to the factory so that normalization rules
	// make same callback.
	o.f.NotifyOnMatchedRule(matchedRule)
}

// NotifyOnAppliedRule sets a callback function which is invoked each time an
// optimization rule (Normalize or Explore) has been applied by the optimizer.
// If appliedRule is nil, then no further notifications are sent.
func (o *Optimizer) NotifyOnAppliedRule(appliedRule AppliedRuleFunc) {
	o.appliedRule = appliedRule

	// Also pass through the call to the factory so that normalization rules
	// make same callback.
	o.f.NotifyOnAppliedRule(appliedRule)
}

// Memo returns the memo structure that the optimizer is using to optimize.
func (o *Optimizer) Memo() *memo.Memo {
	return o.mem
}

// Optimize returns the expression which satisfies the required physical
// properties at the lowest possible execution cost, but is still logically
// equivalent to the given expression. If there is a cost "tie", then any one
// of the qualifying lowest cost expressions may be selected by the optimizer.
func (o *Optimizer) Optimize() (_ opt.Expr, err error) {
	defer func() {
		if r := recover(); r != nil {
			// This code allows us to propagate internal errors without having to add
			// error checks everywhere throughout the code. This is only possible
			// because the code does not update shared state and does not manipulate
			// locks.
			if ok, e := errorutil.ShouldCatch(r); ok {
				err = e
			} else {
				// Other panic objects can't be considered "safe" and thus are
				// propagated as crashes that terminate the session.
				panic(r)
			}
		}
	}()

	if o.mem.IsOptimized() {
		return nil, errors.AssertionFailedf("cannot optimize a memo multiple times")
	}

	// Optimize the root expression according to the properties required of it.
	o.optimizeRootWithProps()

	// Now optimize the entire expression tree.
	root := o.mem.RootExpr().(memo.RelExpr)
	rootProps := o.mem.RootProps()
	o.optimizeGroup(root, rootProps)

	// Walk the tree from the root, updating child pointers so that the memo
	// root points to the lowest cost tree by default (rather than the normalized
	// tree by default.
	root = o.setLowestCostTree(root, rootProps).(memo.RelExpr)
	o.mem.SetRoot(root, rootProps)

	// Validate there are no dangling references.
	if !root.Relational().OuterCols.Empty() {
		return nil, errors.AssertionFailedf(
			"top-level relational expression cannot have outer columns: %s",
			errors.Safe(root.Relational().OuterCols),
		)
	}

	// Validate that the factory's stack depth is zero after all optimizations
	// have been applied.
	o.f.CheckConstructorStackDepth()

	return root, nil
}

// optimizeExpr calls either optimizeGroup or optimizeScalarExpr depending on
// the type of the expression (relational or scalar).
func (o *Optimizer) optimizeExpr(
	e opt.Expr, required *physical.Required,
) (cost memo.Cost, fullyOptimized bool) {
	switch t := e.(type) {
	case memo.RelExpr:
		state := o.optimizeGroup(t, required)
		return state.cost, state.fullyOptimized

	case memo.ScalarPropsExpr:
		// Short-circuit traversal of scalar expressions with no nested subquery,
		// since there's only one possible tree.
		if !t.ScalarProps().HasSubquery {
			return 0, true
		}
		return o.optimizeScalarExpr(t)

	case opt.ScalarExpr:
		return o.optimizeScalarExpr(t)

	default:
		panic(errors.AssertionFailedf("unhandled child: %+v", e))
	}
}

// optimizeGroup enumerates expression trees rooted in the given memo group and
// finds the expression tree with the lowest cost (i.e. the "best") that
// provides the given required physical properties. Enforcers are added as
// needed to provide the required properties.
//
// The following is a simplified walkthrough of how the optimizer might handle
// the following SQL query:
//
//   SELECT * FROM a WHERE x=1 ORDER BY y
//
// Before the optimizer is invoked, the memo group contains a single normalized
// expression:
//
//   memo
//    ├── G1: (select G2 G3)
//    ├── G2: (scan a)
//    ├── G3: (eq 3 2)
//    ├── G4: (variable x)
//    └── G5: (const 1)
//
// Optimization begins at the root of the memo (group #1), and calls
// optimizeGroup with the properties required of that group ("ordering:y").
// optimizeGroup then calls optimizeGroupMember for the Select expression, which
// checks whether the expression can provide the required properties. Since
// Select is a pass-through operator, it can provide the properties by passing
// through the requirement to its input. Accordingly, optimizeGroupMember
// recursively invokes optimizeGroup on select's input child (group #2), with
// the same set of required properties.
//
// Now the same set of steps are applied to group #2. However, the Scan
// expression cannot provide the required ordering (say because it's ordered on
// x rather than y). The optimizer must add a Sort enforcer. It does this by
// recursively invoking optimizeGroup on the same group #2, but this time
// without the ordering requirement. The Scan operator is capable of meeting
// these reduced requirements, so it is costed and added as the current lowest
// cost expression for that group for that set of properties (i.e. the empty
// set).
//
//   memo
//    ├── G1: (select G2 G3)
//    ├── G2: (scan a)
//    │    └── []
//    │         ├── best: (scan a)
//    │         └── cost: 100.00
//    ├── G3: (eq 3 2)
//    ├── G4: (variable x)
//    └── G5: (const 1)
//
// The recursion pops up a level, and now the Sort enforcer knows its input,
// and so it too can be costed (cost of input + extra cost of sort) and added
// as the best expression for the property set with the ordering requirement.
//
//   memo
//    ├── G1: (select G2 G3)
//    ├── G2: (scan a)
//    │    ├── [ordering: y]
//    │    │    ├── best: (sort G2)
//    │    │    └── cost: 150.00
//    │    └── []
//    │         ├── best: (scan a)
//    │         └── cost: 100.00
//    ├── G3: (eq 3 2)
//    ├── G4: (variable x)
//    └── G5: (const 1)
//
// Recursion pops up another level, and the Select operator now knows its input
// (the Sort of the Scan). It then moves on to its scalar filter child and
// optimizes it and its descendants, which is relatively uninteresting since
// there are no subqueries to consider (in fact, the optimizer recognizes this
// and skips traversal altogether). Once all children have been optimized, the
// Select operator can now be costed and added as the best expression for the
// ordering requirement. It requires the same ordering requirement from its
// input child (i.e. the scan).
//
//   memo
//    ├── G1: (select G2 G3)
//    │    └── [ordering: y]
//    │         ├── best: (select G2="ordering: y" G3)
//    │         └── cost: 160.00
//    ├── G2: (scan a)
//    │    ├── [ordering: y]
//    │    │    ├── best: (sort G2)
//    │    │    └── cost: 150.00
//    │    └── []
//    │         ├── best: (scan a)
//    │         └── cost: 100.00
//    ├── G3: (eq 3 2)
//    ├── G4: (variable x)
//    └── G5: (const 1)
//
// But the process is not yet complete. After traversing the Select child
// groups, optimizeExpr generates an alternate plan that satisfies the ordering
// property by using a top-level enforcer. It does this by recursively invoking
// optimizeGroup for group #1, but without the ordering requirement, analogous
// to what it did for group #2. This triggers optimization for each child group
// of the Select operator. But this time, the memo already has fully-costed
// best expressions available for both the Input and Filter children, and so
// returns them immediately with no extra work. The Select expression is now
// costed and added as the best expression without an ordering requirement.
//
//   memo
//    ├── G1: (select G2 G3)
//    │    ├── [ordering: y]
//    │    │    ├── best: (select G2="ordering: y" G3)
//    │    │    └── cost: 160.00
//    │    └── []
//    │         ├── best: (select G2 G3)
//    │         └── cost: 110.00
//    ├── G2: (scan a)
//    │    ├── [ordering: y]
//    │    │    ├── best: (sort G2)
//    │    │    └── cost: 150.00
//    │    └── []
//    │         ├── best: (scan a)
//    │         └── cost: 100.00
//    ├── G3: (eq 3 2)
//    ├── G4: (variable x)
//    └── G5: (const 1)
//
// Finally, the Sort enforcer for group #1 has its input and can be costed. But
// rather than costing 50.0 like the other Sort enforcer, this one only costs
// 1.0, because it's sorting a tiny set of filtered rows. That means its total
// cost is only 111.0, which makes it the new best expression for group #1 with
// an ordering requirement:
//
//   memo
//    ├── G1: (select G2 G3)
//    │    ├── [ordering: y]
//    │    │    ├── best: (sort G1)
//    │    │    └── cost: 111.00
//    │    └── []
//    │         ├── best: (select G2 G3)
//    │         └── cost: 110.00
//    ├── G2: (scan a)
//    │    ├── [ordering: y]
//    │    │    ├── best: (sort G2)
//    │    │    └── cost: 150.00
//    │    └── []
//    │         ├── best: (scan a)
//    │         └── cost: 100.00
//    ├── G3: (eq 3 2)
//    ├── G4: (variable x)
//    └── G5: (const 1)
//
// Now the memo has been fully optimized, and the best expression for group #1
// and "ordering:y" can be set as the root of the tree by setLowestCostTree:
//
//   sort
//    ├── columns: x:1(int) y:2(int)
//    ├── ordering: +2
//    └── select
//         ├── columns: x:1(int) y:2(int)
//         ├── scan
//         │    └── columns: x:1(int) y:2(int)
//         └── eq [type=bool]
//              ├── variable: a.x [type=int]
//              └── const: 1 [type=int]
//
func (o *Optimizer) optimizeGroup(grp memo.RelExpr, required *physical.Required) *groupState {
	// Always start with the first expression in the group.
	grp = grp.FirstExpr()

	// If this group is already fully optimized, then return the already prepared
	// best expression (won't ever get better than this).
	state := o.ensureOptState(grp, required)
	if state.fullyOptimized {
		return state
	}

	// Iterate until the group has been fully optimized.
	for {
		fullyOptimized := true

		for i, member := 0, grp; member != nil; i, member = i+1, member.NextExpr() {
			// If this group member has already been fully optimized for the given
			// required properties, then skip it, since it won't get better.
			if state.isMemberFullyOptimized(i) {
				continue
			}

			// Optimize the group member with respect to the required properties.
			memberOptimized := o.optimizeGroupMember(state, member, required)

			// If any of the group members have not yet been fully optimized, then
			// the group is not yet fully optimized.
			if memberOptimized {
				state.markMemberAsFullyOptimized(i)
			} else {
				fullyOptimized = false
			}
		}

		// Now try to generate new expressions that are logically equivalent to
		// other expressions in this group.
		if o.shouldExplore(required) && !o.explorer.exploreGroup(grp).fullyExplored {
			fullyOptimized = false
		}

		if fullyOptimized {
			state.fullyOptimized = true
			break
		}
	}

	return state
}

// optimizeGroupMember determines whether the group member expression can
// provide the required properties. If so, it recursively optimizes the
// expression's child groups and computes the cost of the expression. In
// addition, optimizeGroupMember calls enforceProps to check whether enforcers
// can provide the required properties at a lower cost. The lowest cost
// expression is saved to groupState.
func (o *Optimizer) optimizeGroupMember(
	state *groupState, member memo.RelExpr, required *physical.Required,
) (fullyOptimized bool) {
	// Compute the cost for enforcers to provide the required properties. This
	// may be lower than the expression providing the properties itself. For
	// example, it might be better to sort the results of a hash join than to
	// use the results of a merge join that are already sorted, but at the cost
	// of requiring one of the merge join children to be sorted.
	fullyOptimized = o.enforceProps(state, member, required)

	// If the expression cannot provide the required properties, then don't
	// continue. But what if the expression is able to provide a subset of the
	// properties? That case is taken care of by enforceProps, which will
	// recursively optimize the group with property subsets and then add
	// enforcers to provide the remainder.
	if CanProvidePhysicalProps(member, required) {
		var cost memo.Cost
		for i, n := 0, member.ChildCount(); i < n; i++ {
			// Given required parent properties, get the properties required from
			// the nth child.
			childRequired := BuildChildPhysicalProps(o.mem, member, i, required)

			// Optimize the child with respect to those properties.
			childCost, childOptimized := o.optimizeExpr(member.Child(i), childRequired)

			// Accumulate cost of children.
			cost += childCost

			// If any child expression is not fully optimized, then the parent
			// expression is also not fully optimized.
			if !childOptimized {
				fullyOptimized = false
			}
		}

		// Check whether this is the new lowest cost expression.
		cost += o.coster.ComputeCost(member, required)
		o.ratchetCost(state, member, cost)
	}

	return fullyOptimized
}

// optimizeScalarExpr recursively optimizes the children of a scalar expression.
// This is only necessary when the scalar expression contains a subquery, since
// scalar expressions otherwise always have zero cost and only one possible
// plan.
func (o *Optimizer) optimizeScalarExpr(
	scalar opt.ScalarExpr,
) (cost memo.Cost, fullyOptimized bool) {
	fullyOptimized = true
	for i, n := 0, scalar.ChildCount(); i < n; i++ {
		childProps := BuildChildPhysicalPropsScalar(o.mem, scalar, i)
		childCost, childOptimized := o.optimizeExpr(scalar.Child(i), childProps)

		// Accumulate cost of children.
		cost += childCost

		// If any child expression is not fully optimized, then the parent
		// expression is also not fully optimized.
		if !childOptimized {
			fullyOptimized = false
		}
	}
	return cost, fullyOptimized
}

// enforceProps costs an expression where one of the physical properties has
// been provided by an enforcer rather than by the expression itself. There are
// two reasons why this is necessary/desirable:
//
//   1. The expression may not be able to provide the property on its own. For
//      example, a hash join cannot provide ordered results.
//   2. The enforcer might be able to provide the property at lower overall
//      cost. For example, an enforced sort on top of a hash join might be
//      lower cost than a merge join that is already sorted, but at the cost of
//      requiring one of its children to be sorted.
//
// Note that enforceProps will recursively optimize this same group, but with
// one less required physical property. The recursive call will eventually make
// its way back here, at which point another physical property will be stripped
// off, and so on. Afterwards, the group will have computed a lowest cost
// expression for each sublist of physical properties, from all down to none.
//
// Right now, the only physical property that can be provided by an enforcer is
// physical.Required.Ordering. When adding another enforceable property, also
// update shouldExplore, which should return true if enforceProps will explore
// the group by recursively calling optimizeGroup (by way of optimizeEnforcer).
func (o *Optimizer) enforceProps(
	state *groupState, member memo.RelExpr, required *physical.Required,
) (fullyOptimized bool) {
	// Strip off one property that can be enforced. Other properties will be
	// stripped by recursively optimizing the group with successively fewer
	// properties. The properties are stripped off in a heuristic order, from
	// least likely to be expensive to enforce to most likely.
	if !required.Ordering.Any() {
		// Try Sort enforcer that requires no ordering from its input.
		enforcer := &memo.SortExpr{Input: member}
		memberProps := BuildChildPhysicalProps(o.mem, enforcer, 0, required)
		fullyOptimized = o.optimizeEnforcer(state, enforcer, required, member, memberProps)

		// Try Sort enforcer that requires a partial ordering from its input. Choose
		// the interesting ordering that forms the longest common prefix with the
		// required ordering. We do not need to add the enforcer if the required
		// ordering is implied by the input ordering (in which case the returned
		// prefix is nil).
		interestingOrderings := ordering.DeriveInterestingOrderings(member)
		longestCommonPrefix := interestingOrderings.LongestCommonPrefix(&required.Ordering)
		if longestCommonPrefix != nil {
			enforcer := &memo.SortExpr{Input: state.best}
			enforcer.InputOrdering = *longestCommonPrefix
			memberProps := BuildChildPhysicalProps(o.mem, enforcer, 0, required)
			if o.optimizeEnforcer(state, enforcer, required, member, memberProps) {
				fullyOptimized = true
			}
		}

		return fullyOptimized
	}

	return true
}

// optimizeEnforcer optimizes and costs the enforcer.
func (o *Optimizer) optimizeEnforcer(
	state *groupState,
	enforcer memo.RelExpr,
	enforcerProps *physical.Required,
	member memo.RelExpr,
	memberProps *physical.Required,
) (fullyOptimized bool) {
	// Recursively optimize the member group with respect to a subset of the
	// enforcer properties.
	innerState := o.optimizeGroup(member, memberProps)
	fullyOptimized = innerState.fullyOptimized

	// Check whether this is the new lowest cost expression with the enforcer
	// added.
	cost := innerState.cost + o.coster.ComputeCost(enforcer, enforcerProps)
	o.ratchetCost(state, enforcer, cost)

	// Enforcer expression is fully optimized if its input expression is fully
	// optimized.
	return fullyOptimized
}

// shouldExplore ensures that exploration is only triggered for optimizeGroup
// calls that will not recurse via a call from enforceProps.
func (o *Optimizer) shouldExplore(required *physical.Required) bool {
	return required.Ordering.Any()
}

// setLowestCostTree traverses the memo and recursively updates child pointers
// so that they point to the lowest cost expression tree rather than to the
// normalized expression tree. Each participating memo group is updated to store
// the physical properties required of it, as well as the estimated cost of
// executing the lowest cost expression in the group. As an example, say this is
// the memo, with a normalized tree containing the first expression in each of
// the groups:
//
//   memo
//    ├── G1: (inner-join G2 G3 G4) (inner-join G3 G2 G4)
//    ├── G2: (scan a)
//    ├── G3: (select G5 G6) (scan b,constrained)
//    ├── G4: (true)
//    ├── G5: (scan b)
//    ├── G6: (eq G7 G8)
//    ├── G7: (variable b.x)
//    └── G8: (const 1)
//
// setLowestCostTree is called after exploration is complete, and after each
// group member has been costed. If the second expression in groups G1 and G3
// have lower cost than the first expressions, then setLowestCostTree will
// update pointers so that the root expression tree includes those expressions
// instead.
//
// Note that a memo group can have multiple lowest cost expressions, each for a
// different set of physical properties. During optimization, these are retained
// in the groupState map. However, only one of those lowest cost expressions
// will be used in the final tree; the others are simply discarded. This is
// because there is never a case where a relational expression is referenced
// multiple times in the final tree, but with different physical properties
// required by each of those references.
func (o *Optimizer) setLowestCostTree(parent opt.Expr, parentProps *physical.Required) opt.Expr {
	var relParent memo.RelExpr
	var relCost memo.Cost
	switch t := parent.(type) {
	case memo.RelExpr:
		state := o.lookupOptState(t.FirstExpr(), parentProps)
		relParent, relCost = state.best, state.cost
		parent = relParent

	case memo.ScalarPropsExpr:
		// Short-circuit traversal of scalar expressions with no nested subquery,
		// since there's only one possible tree.
		if !t.ScalarProps().HasSubquery {
			return parent
		}
	}

	// Iterate over the expression's children, replacing any that have a lower
	// cost alternative.
	var mutable opt.MutableExpr
	var childProps *physical.Required
	for i, n := 0, parent.ChildCount(); i < n; i++ {
		before := parent.Child(i)

		if relParent != nil {
			childProps = BuildChildPhysicalProps(o.mem, relParent, i, parentProps)
		} else {
			childProps = BuildChildPhysicalPropsScalar(o.mem, parent, i)
		}

		after := o.setLowestCostTree(before, childProps)
		if after != before {
			if mutable == nil {
				mutable = parent.(opt.MutableExpr)
			}
			mutable.SetChild(i, after)
		}
	}

	if relParent != nil {
		var provided physical.Provided
		// BuildProvided relies on ProvidedPhysical() being set in the children, so
		// it must run after the recursive calls on the children.
		provided.Ordering = ordering.BuildProvided(relParent, &parentProps.Ordering)
		o.mem.SetBestProps(relParent, parentProps, &provided, relCost)
	}

	return parent
}

// ratchetCost computes the cost of the candidate expression, and then checks
// whether it's lower than the cost of the existing best expression in the
// group. If so, then the candidate becomes the new lowest cost expression.
func (o *Optimizer) ratchetCost(state *groupState, candidate memo.RelExpr, cost memo.Cost) {
	if state.best == nil || cost.Less(state.cost) {
		state.best = candidate
		state.cost = cost
	}
}

// lookupOptState looks up the state associated with the given group and
// properties. If no state exists yet, then lookupOptState returns nil.
func (o *Optimizer) lookupOptState(grp memo.RelExpr, required *physical.Required) *groupState {
	return o.stateMap[groupStateKey{group: grp, required: required}]
}

// ensureOptState looks up the state associated with the given group and
// properties. If none is associated yet, then ensureOptState allocates new
// state and returns it.
func (o *Optimizer) ensureOptState(grp memo.RelExpr, required *physical.Required) *groupState {
	key := groupStateKey{group: grp, required: required}
	state, ok := o.stateMap[key]
	if !ok {
		state = o.stateAlloc.allocate()
		state.required = required
		o.stateMap[key] = state
	}
	return state
}

// optimizeRootWithProps tries to simplify the root operator based on the
// properties required of it. This may trigger the creation of a new root and
// new properties.
func (o *Optimizer) optimizeRootWithProps() {
	root, ok := o.mem.RootExpr().(memo.RelExpr)
	if !ok {
		panic(errors.AssertionFailedf("Optimize can only be called on relational root expressions"))
	}
	rootProps := o.mem.RootProps()

	// [SimplifyRootOrdering]
	// SimplifyRootOrdering removes redundant columns from the root properties,
	// based on the operator's functional dependencies.
	if rootProps.Ordering.CanSimplify(&root.Relational().FuncDeps) {
		if o.matchedRule == nil || o.matchedRule(opt.SimplifyRootOrdering) {
			simplified := *rootProps
			simplified.Ordering = rootProps.Ordering.Copy()
			simplified.Ordering.Simplify(&root.Relational().FuncDeps)
			o.mem.SetRoot(root, &simplified)
			rootProps = o.mem.RootProps()
			if o.appliedRule != nil {
				o.appliedRule(opt.SimplifyRootOrdering, nil, root)
			}
		}
	}

	// [PruneRootCols]
	// PruneRootCols discards columns that are not needed by the root's ordering
	// or presentation properties.
	neededCols := rootProps.ColSet()
	if !neededCols.SubsetOf(root.Relational().OutputCols) {
		panic(errors.AssertionFailedf(
			"columns required of root %s must be subset of output columns %s",
			neededCols,
			root.Relational().OutputCols,
		))
	}
	if o.f.CustomFuncs().CanPruneCols(root, neededCols) {
		if o.matchedRule == nil || o.matchedRule(opt.PruneRootCols) {
			root = o.f.CustomFuncs().PruneCols(root, neededCols)
			// We may have pruned a column that appears in the required ordering.
			rootCols := root.Relational().OutputCols
			if !rootProps.Ordering.SubsetOfCols(rootCols) {
				newProps := *rootProps
				newProps.Ordering = rootProps.Ordering.Copy()
				newProps.Ordering.ProjectCols(rootCols)
				o.mem.SetRoot(root, &newProps)
				//lint:ignore SA4006 set rootProps in case another rule is added below.
				rootProps = o.mem.RootProps()
			} else {
				o.mem.SetRoot(root, rootProps)
			}
			if o.appliedRule != nil {
				o.appliedRule(opt.PruneRootCols, nil, root)
			}
		}
	}
}

// groupStateKey associates groupState with a group that is being optimized with
// respect to a set of physical properties.
type groupStateKey struct {
	group    memo.RelExpr
	required *physical.Required
}

// groupState is temporary storage that's associated with each group that's
// optimized (or same group with different sets of physical properties). The
// optimizer stores various flags and other state here that allows it to do
// quicker lookups and short-circuit already traversed parts of the expression
// tree.
type groupState struct {
	// best identifies the lowest cost expression in the memo group for a given
	// set of physical properties.
	best memo.RelExpr

	// required is the set of physical properties that must be provided by this
	// lowest cost expression. An expression that cannot provide these properties
	// cannot be the best expression, no matter how low its cost.
	required *physical.Required

	// cost is the estimated execution cost for this expression. The best
	// expression for a given group and set of physical properties is the
	// expression with the lowest cost.
	cost memo.Cost

	// fullyOptimized is set to true once the lowest cost expression has been
	// found for a memo group, with respect to the required properties. A lower
	// cost expression will never be found, no matter how many additional
	// optimization passes are made.
	fullyOptimized bool

	// fullyOptimizedExprs contains the set of ordinal positions of each member
	// expression in the group that has been fully optimized for the required
	// properties. These never need to be recosted, no matter how many additional
	// optimization passes are made.
	fullyOptimizedExprs util.FastIntSet

	// explore is used by the explorer to store intermediate state so that
	// redundant work is minimized.
	explore exploreState
}

// isMemberFullyOptimized returns true if the group member at the given ordinal
// position has been fully optimized for the required properties. The expression
// never needs to be recosted, no matter how many additional optimization passes
// are made.
func (os *groupState) isMemberFullyOptimized(ord int) bool {
	return os.fullyOptimizedExprs.Contains(ord)
}

// markMemberAsFullyOptimized marks the group member at the given ordinal
// position as fully optimized for the required properties. The expression never
// needs to be recosted, no matter how many additional optimization passes are
// made.
func (os *groupState) markMemberAsFullyOptimized(ord int) {
	if os.fullyOptimized {
		panic(errors.AssertionFailedf("best expression is already fully optimized"))
	}
	if os.isMemberFullyOptimized(ord) {
		panic(errors.AssertionFailedf("memo expression is already fully optimized for required physical properties"))
	}
	os.fullyOptimizedExprs.Add(ord)
}

// groupStateAlloc allocates pages of groupState structs. This is preferable to
// a slice of groupState structs because pointers are not invalidated when a
// resize occurs, and because there's no need to retain a stable index.
type groupStateAlloc struct {
	page []groupState
}

// allocate returns a pointer to a new, empty groupState struct. The pointer is
// stable, meaning that its location won't change as other groupState structs
// are allocated.
func (a *groupStateAlloc) allocate() *groupState {
	if len(a.page) == 0 {
		a.page = make([]groupState, 8)
	}
	state := &a.page[0]
	a.page = a.page[1:]
	return state
}

// disableRules disables rules with the given probability for testing.
func (o *Optimizer) disableRules(probability float64) {
	essentialRules := util.MakeFastIntSet(
		// Needed to prevent constraint building from failing.
		int(opt.NormalizeInConst),
		// Needed when an index is forced.
		int(opt.GenerateIndexScans),
		// Needed to prevent "same fingerprint cannot map to different groups."
		int(opt.PruneJoinLeftCols),
		int(opt.PruneJoinRightCols),
		// Needed to prevent stack overflow.
		int(opt.PushFilterIntoJoinLeftAndRight),
		int(opt.PruneSelectCols),
		// Needed to prevent execbuilder error.
		// TODO(radu): the DistinctOn execution path should be fixed up so it
		// supports distinct on an empty column set.
		int(opt.EliminateDistinctNoColumns),
		int(opt.EliminateEnsureDistinctNoColumns),
	)

	for i := opt.RuleName(1); i < opt.NumRuleNames; i++ {
		if rand.Float64() < probability && !essentialRules.Contains(int(i)) {
			o.disabledRules.Add(int(i))
		}
	}

	o.NotifyOnMatchedRule(func(ruleName opt.RuleName) bool {
		if o.disabledRules.Contains(int(ruleName)) {
			log.Infof(o.evalCtx.Context, "disabled rule matched: %s", ruleName.String())
			return false
		}
		return true
	})
}

func (o *Optimizer) String() string {
	return o.FormatMemo(FmtPretty)
}

// FormatMemo returns a string representation of the memo for testing
// and debugging. The given flags control which properties are shown.
func (o *Optimizer) FormatMemo(flags FmtFlags) string {
	mf := makeMemoFormatter(o, flags)
	return mf.format()
}

// RecomputeCost recomputes the cost of each expression in the lowest cost
// tree. It should be used in combination with the perturb-cost OptTester flag
// in order to update the query plan tree after optimization is complete with
// the real computed cost, not the perturbed cost.
func (o *Optimizer) RecomputeCost() {
	var c coster
	c.Init(o.evalCtx, o.mem, 0 /* perturbation */)

	root := o.mem.RootExpr()
	rootProps := o.mem.RootProps()
	o.recomputeCostImpl(root, rootProps, &c)
}

func (o *Optimizer) recomputeCostImpl(
	parent opt.Expr, parentProps *physical.Required, c Coster,
) memo.Cost {
	cost := memo.Cost(0)
	for i, n := 0, parent.ChildCount(); i < n; i++ {
		child := parent.Child(i)
		childProps := physical.MinRequired
		switch t := child.(type) {
		case memo.RelExpr:
			childProps = t.RequiredPhysical()
		}
		cost += o.recomputeCostImpl(child, childProps, c)
	}

	switch t := parent.(type) {
	case memo.RelExpr:
		cost += c.ComputeCost(t, parentProps)
		o.mem.ResetCost(t, cost)
	}

	return cost
}

// FormatExpr is a convenience wrapper for memo.FormatExpr.
func (o *Optimizer) FormatExpr(e opt.Expr, flags memo.ExprFmtFlags) string {
	return memo.FormatExpr(e, flags, o.mem, o.catalog)
}

// CustomFuncs exports the xform.CustomFuncs for testing purposes.
func (o *Optimizer) CustomFuncs() *CustomFuncs {
	return &o.explorer.funcs
}
