/*
Copyright 2022 The Vitess Authors.

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

package operators

import (
	"io"

	"golang.org/x/exp/slices"

	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/operators/ops"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/plancontext"
	"vitess.io/vitess/go/vt/vtgate/semantics"
)

type Derived struct {
	Source  ops.Operator
	TableId semantics.TableSet

	// QP contains the QueryProjection for this op
	QP *QueryProjection

	Query         sqlparser.SelectStatement
	Alias         string
	ColumnAliases sqlparser.Columns

	// Columns needed to feed other plans
	Columns       []*sqlparser.ColName
	ColumnsOffset []int
}

// Clone implements the Operator interface
func (d *Derived) Clone(inputs []ops.Operator) ops.Operator {
	return &Derived{
		Source:        inputs[0],
		Query:         d.Query,
		Alias:         d.Alias,
		ColumnAliases: sqlparser.CloneColumns(d.ColumnAliases),
		Columns:       slices.Clone(d.Columns),
		ColumnsOffset: slices.Clone(d.ColumnsOffset),
		TableId:       d.TableId,
	}
}

// findOutputColumn returns the index on which the given name is found in the slice of
// *sqlparser.SelectExprs of the derivedTree. The *sqlparser.SelectExpr must be of type
// *sqlparser.AliasedExpr and match the given name.
// If name is not present but the query's select expressions contain a *sqlparser.StarExpr
// the function will return no error and an index equal to -1.
// If name is not present and the query does not have a *sqlparser.StarExpr, the function
// will return an unknown column error.
func (d *Derived) findOutputColumn(name *sqlparser.ColName) (int, error) {
	hasStar := false
	for j, exp := range sqlparser.GetFirstSelect(d.Query).SelectExprs {
		switch exp := exp.(type) {
		case *sqlparser.AliasedExpr:
			if !exp.As.IsEmpty() && exp.As.Equal(name.Name) {
				return j, nil
			}
			if exp.As.IsEmpty() {
				col, ok := exp.Expr.(*sqlparser.ColName)
				if !ok {
					return 0, vterrors.VT12001("complex expression needs column alias: %s", sqlparser.String(exp))
				}
				if name.Name.Equal(col.Name) {
					return j, nil
				}
			}
		case *sqlparser.StarExpr:
			hasStar = true
		}
	}

	// we have found a star but no matching *sqlparser.AliasedExpr, thus we return -1 with no error.
	if hasStar {
		return -1, nil
	}
	return 0, vterrors.VT03014(name.Name.String(), "field list")
}

// IsMergeable is not a great name for this function. Suggestions for a better one are welcome!
// This function will return false if the derived table inside it has to run on the vtgate side, and so can't be merged with subqueries
// This logic can also be used to check if this is a derived table that can be had on the left hand side of a vtgate join.
// Since vtgate joins are always nested loop joins, we can't execute them on the RHS
// if they do some things, like LIMIT or GROUP BY on wrong columns
func (d *Derived) IsMergeable(ctx *plancontext.PlanningContext) bool {
	return isMergeable(ctx, d.Query, d)
}

// Inputs implements the Operator interface
func (d *Derived) Inputs() []ops.Operator {
	return []ops.Operator{d.Source}
}

// SetInputs implements the Operator interface
func (d *Derived) SetInputs(ops []ops.Operator) {
	d.Source = ops[0]
}

func (d *Derived) AddPredicate(ctx *plancontext.PlanningContext, expr sqlparser.Expr) (ops.Operator, error) {
	if _, isUNion := d.Source.(*Union); isUNion {
		// If we have a derived table on top of a UNION, we can let the UNION do the expression rewriting
		var err error
		d.Source, err = d.Source.AddPredicate(ctx, expr)
		return d, err
	}
	tableInfo, err := ctx.SemTable.TableInfoForExpr(expr)
	if err != nil {
		if err == semantics.ErrNotSingleTable {
			return &Filter{
				Source:     d,
				Predicates: []sqlparser.Expr{expr},
			}, nil
		}
		return nil, err
	}

	newExpr := semantics.RewriteDerivedTableExpression(expr, tableInfo)
	if !canBePushedDownIntoDerived(newExpr) {
		// if we have an aggregation, we don't want to push it inside
		return &Filter{Source: d, Predicates: []sqlparser.Expr{expr}}, nil
	}
	d.Source, err = d.Source.AddPredicate(ctx, newExpr)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func canBePushedDownIntoDerived(expr sqlparser.Expr) (canBePushed bool) {
	canBePushed = true
	_ = sqlparser.Walk(func(node sqlparser.SQLNode) (kontinue bool, err error) {
		switch node.(type) {
		case *sqlparser.Max, *sqlparser.Min:
			// empty by default
		case sqlparser.AggrFunc:
			canBePushed = false
			return false, io.EOF
		}
		return true, nil
	}, expr)
	return
}

func (d *Derived) AddColumn(ctx *plancontext.PlanningContext, expr *sqlparser.AliasedExpr, _, addToGroupBy bool) (ops.Operator, int, error) {
	col, ok := expr.Expr.(*sqlparser.ColName)
	if !ok {
		return nil, 0, vterrors.VT13001("cannot push non-colname expression to a derived table")
	}

	identity := func(c *sqlparser.ColName) sqlparser.Expr { return c }
	if offset, found := canReuseColumn(ctx, d.Columns, col, identity); found {
		return d, offset, nil
	}

	i, err := d.findOutputColumn(col)
	if err != nil {
		return nil, 0, err
	}
	var pos int
	d.ColumnsOffset, pos = addToIntSlice(d.ColumnsOffset, i)

	d.Columns = append(d.Columns, col)
	// add it to the source if we were not already passing it through
	if i <= -1 {
		newSrc, _, err := d.Source.AddColumn(ctx, aeWrap(sqlparser.NewColName(col.Name.String())), true, addToGroupBy)
		if err != nil {
			return nil, 0, err
		}
		d.Source = newSrc
	}
	return d, pos, nil
}

// canReuseColumn is generic, so it can be used with slices of different types.
// We don't care about the actual type, as long as we know it's a sqlparser.Expr
func canReuseColumn[T any](
	ctx *plancontext.PlanningContext,
	columns []T,
	col sqlparser.Expr,
	f func(T) sqlparser.Expr,
) (offset int, found bool) {
	for offset, column := range columns {
		if ctx.SemTable.EqualsExprWithDeps(col, f(column)) {
			return offset, true
		}
	}

	return
}

func (d *Derived) GetColumns() (exprs []*sqlparser.AliasedExpr, err error) {
	for _, expr := range sqlparser.GetFirstSelect(d.Query).SelectExprs {
		ae, ok := expr.(*sqlparser.AliasedExpr)
		if !ok {
			return nil, errHorizonNotPlanned()
		}
		exprs = append(exprs, ae)
	}
	return
}

func (d *Derived) GetOrdering() ([]ops.OrderBy, error) {
	if d.QP == nil {
		return nil, vterrors.VT13001("QP should already be here")
	}
	return d.QP.OrderExprs, nil
}

func addToIntSlice(columnOffset []int, valToAdd int) ([]int, int) {
	for idx, val := range columnOffset {
		if val == valToAdd {
			return columnOffset, idx
		}
	}
	columnOffset = append(columnOffset, valToAdd)
	return columnOffset, len(columnOffset) - 1
}

// TODO: REMOVE
func (d *Derived) selectStatement() sqlparser.SelectStatement {
	return d.Query
}

func (d *Derived) src() ops.Operator {
	return d.Source
}

func (d *Derived) getQP(ctx *plancontext.PlanningContext) (*QueryProjection, error) {
	if d.QP != nil {
		return d.QP, nil
	}
	qp, err := CreateQPFromSelect(ctx, d.Query.(*sqlparser.Select))
	if err != nil {
		return nil, err
	}
	d.QP = qp
	return d.QP, nil
}

func (d *Derived) setQP(qp *QueryProjection) {
	d.QP = qp
}

func (d *Derived) Description() ops.OpDescription {
	return ops.OpDescription{
		OperatorType: "Derived",
	}
}

func (d *Derived) ShortDescription() string {
	return d.Alias
}
