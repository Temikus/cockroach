// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package scbuildstmt

import (
	"reflect"

	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondatapb"
	"github.com/cockroachdb/errors"
)

// supportedStatement tracks metadata for statements that are
// implemented by the new schema changer.
type supportedStatement struct {
	// fn is a function to perform a schema change.
	fn interface{}
	// on indicates that this statement is on by default.
	on bool
}

// isFullySupported returns if this statement type is supported, where the
// mode of the new schema changer can force unsupported statements to be
// supported.
func isFullySupported(onByDefault bool, mode sessiondatapb.NewSchemaChangerMode) bool {
	// If the unsafe modes of the new schema changer are used then any implemented
	// operation will be exposed.
	if mode == sessiondatapb.UseNewSchemaChangerUnsafeAlways ||
		mode == sessiondatapb.UseNewSchemaChangerUnsafe {
		return true
	}
	return onByDefault
}

// Tracks operations which are fully supported when the declarative schema
// changer is enabled. Operations marked as non-fully supported can only be
// with the use_declarative_schema_changer session variable.
var supportedStatements = map[reflect.Type]supportedStatement{
	// Alter table will have commands individually whitelisted via the
	// supportedAlterTableStatements list, so wwe will consider it fully supported
	// here.
	reflect.TypeOf((*tree.AlterTable)(nil)):          {fn: AlterTable, on: true},
	reflect.TypeOf((*tree.CreateIndex)(nil)):         {fn: CreateIndex, on: false},
	reflect.TypeOf((*tree.DropDatabase)(nil)):        {fn: DropDatabase, on: true},
	reflect.TypeOf((*tree.DropOwnedBy)(nil)):         {fn: DropOwnedBy, on: true},
	reflect.TypeOf((*tree.DropSchema)(nil)):          {fn: DropSchema, on: true},
	reflect.TypeOf((*tree.DropSequence)(nil)):        {fn: DropSequence, on: true},
	reflect.TypeOf((*tree.DropTable)(nil)):           {fn: DropTable, on: true},
	reflect.TypeOf((*tree.DropType)(nil)):            {fn: DropType, on: true},
	reflect.TypeOf((*tree.DropView)(nil)):            {fn: DropView, on: true},
	reflect.TypeOf((*tree.CommentOnDatabase)(nil)):   {fn: CommentOnDatabase, on: true},
	reflect.TypeOf((*tree.CommentOnSchema)(nil)):     {fn: CommentOnSchema, on: true},
	reflect.TypeOf((*tree.CommentOnTable)(nil)):      {fn: CommentOnTable, on: true},
	reflect.TypeOf((*tree.CommentOnColumn)(nil)):     {fn: CommentOnColumn, on: true},
	reflect.TypeOf((*tree.CommentOnIndex)(nil)):      {fn: CommentOnIndex, on: true},
	reflect.TypeOf((*tree.CommentOnConstraint)(nil)): {fn: CommentOnConstraint, on: true},
	// TODO (Xiang): turn on `DROP INDEX` as fully supported.
	reflect.TypeOf((*tree.DropIndex)(nil)): {fn: DropIndex, on: false},
}

func init() {
	// Check function signatures inside the supportedStatements map.
	for statementType, statementEntry := range supportedStatements {
		callBackType := reflect.TypeOf(statementEntry.fn)
		if callBackType.Kind() != reflect.Func {
			panic(errors.AssertionFailedf("%v entry for statement is "+
				"not a function", statementType))
		}
		if callBackType.NumIn() != 2 ||
			!callBackType.In(0).Implements(reflect.TypeOf((*BuildCtx)(nil)).Elem()) ||
			callBackType.In(1) != statementType {
			panic(errors.AssertionFailedf("%v entry for statement is "+
				"does not have a valid signature got %v", statementType, callBackType))
		}
	}
}

// Process dispatches on the statement type to populate the BuilderState
// embedded in the BuildCtx. Any error will be panicked.
func Process(b BuildCtx, n tree.Statement) {
	// Check if an entry exists for the statement type, in which
	// case it is either fully or partially supported.
	info, ok := supportedStatements[reflect.TypeOf(n)]
	if !ok {
		panic(scerrors.NotImplementedError(n))
	}
	// Check if partially supported operations are allowed next. If an
	// operation is not fully supported will not allow it to be run in
	// the declarative schema changer until its fully supported.
	if !isFullySupported(
		info.on, b.EvalCtx().SessionData().NewSchemaChangerMode,
	) {
		panic(scerrors.NotImplementedError(n))
	}
	// Next invoke the callback function, with the concrete types.
	fn := reflect.ValueOf(info.fn)
	in := []reflect.Value{reflect.ValueOf(b), reflect.ValueOf(n)}
	// Check if the feature flag for it is enabled.
	err := b.CheckFeature(b, tree.GetSchemaFeatureNameFromStmt(n))
	if err != nil {
		panic(err)
	}
	fn.Call(in)
}
