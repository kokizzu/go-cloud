// Copyright 2019 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package awsdynamodb provides a docstore implementation backed by Amazon
// DynamoDB.
// Use OpenCollection to construct a *docstore.Collection.
//
// # URLs
//
// For docstore.OpenCollection, awsdynamodb registers for the scheme
// "dynamodb". The default URL opener will use an AWS session with the default
// credentials and configuration; see
// https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html for more details.
// To customize the URL opener, or for more details on the URL format, see
// URLOpener.
// See https://gocloud.dev/concepts/urls/ for background information.
//
// # As
//
// awsdynamodb exposes the following types for As:
//   - Collection.As: *dynamodb.DynamoDB
//   - ActionList.BeforeDo: *dynamodb.BatchGetItemInput or *dynamodb.PutItemInput or *dynamodb.DeleteItemInput
//     or *dynamodb.UpdateItemInput
//   - Query.BeforeQuery: *dynamodb.QueryInput or *dynamodb.ScanInput
//   - DocumentIterator: *dynamodb.QueryOutput or *dynamodb.ScanOutput
//   - ErrorAs: smithy.OperationError
package awsdynamodb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	dyn "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dyn2Types "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
	"github.com/google/wire"
	"gocloud.dev/docstore"
	"gocloud.dev/docstore/driver"
	"gocloud.dev/gcerrors"
	"gocloud.dev/internal/gcerr"
)

// Set holds Wire providers for this package.
var Set = wire.NewSet(
	wire.Struct(new(URLOpener)),
)

type collection struct {
	db           *dyn.Client
	table        string // DynamoDB table name
	partitionKey string
	sortKey      string
	description  *dyn2Types.TableDescription
	opts         *Options
}

// FallbackFunc is a function for executing queries that cannot be run by the built-in
// awsdynamodb logic. See Options.RunQueryFunc for details.
type FallbackFunc func(context.Context, *driver.Query, RunQueryFunc) (driver.DocumentIterator, error)

// Options holds various options.
type Options struct {
	// If false, queries that can only be executed by scanning the entire table
	// return an error instead (with the exception of a query with no filters).
	AllowScans bool

	// The name of the field holding the document revision.
	// Defaults to docstore.DefaultRevisionField.
	RevisionField string

	// If set, call this function on queries that we cannot execute at all (for
	// example, a query with an OrderBy clause that lacks an equality filter on a
	// partition key). The function should execute the query however it wishes, and
	// return an iterator over the results. It can use the RunQueryFunc passed as its
	// third argument to have the DynamoDB driver run a query, for instance a
	// modified version of the original query.
	//
	// If RunQueryFallback is nil, queries that cannot be executed will fail with a
	// error that has code Unimplemented.
	RunQueryFallback FallbackFunc

	// The maximum number of concurrent goroutines started for a single call to
	// ActionList.Do. If less than 1, there is no limit.
	MaxOutstandingActionRPCs int

	// If true, a strongly consistent read is used whenever possible, including
	// get, query, scan, etc.; default to false, where an eventually consistent
	// read is used.
	//
	// Not all read operations support this mode however, such as querying against
	// a global secondary index, the operation will return an InvalidArgument error
	// in such case, please check the official DynamoDB documentation for more
	// details.
	//
	// The native client for DynamoDB uses this option in a per-action basis, if
	// you need the flexibility to run both modes on the same collection, create
	// two collections with different mode.
	ConsistentRead bool
}

// RunQueryFunc is the type of the function passed to RunQueryFallback.
type RunQueryFunc func(context.Context, *driver.Query) (driver.DocumentIterator, error)

// OpenCollection creates a *docstore.Collection representing a DynamoDB collection.
func OpenCollection(db *dyn.Client, tableName, partitionKey, sortKey string, opts *Options) (*docstore.Collection, error) {
	c, err := newCollection(db, tableName, partitionKey, sortKey, opts)
	if err != nil {
		return nil, err
	}
	return docstore.NewCollection(c), nil
}

func newCollection(db *dyn.Client, tableName, partitionKey, sortKey string, opts *Options) (*collection, error) {
	out, err := db.DescribeTable(context.Background(), &dyn.DescribeTableInput{TableName: &tableName})
	if err != nil {
		return nil, err
	}
	if opts == nil {
		opts = &Options{}
	}
	if opts.RevisionField == "" {
		opts.RevisionField = docstore.DefaultRevisionField
	}
	return &collection{
		db:           db,
		table:        tableName,
		partitionKey: partitionKey,
		sortKey:      sortKey,
		description:  out.Table,
		opts:         opts,
	}, nil
}

// Key returns a two-element array with the partition key and sort key, if any.
func (c *collection) Key(doc driver.Document) (interface{}, error) {
	pkey, err := doc.GetField(c.partitionKey)
	if err != nil || pkey == nil || driver.IsEmptyValue(reflect.ValueOf(pkey)) {
		return nil, nil // missing key is not an error
	}
	keys := [2]interface{}{pkey}
	if c.sortKey != "" {
		keys[1], _ = doc.GetField(c.sortKey) // ignore error since keys[1] is nil in that case
	}
	return keys, nil
}

func (c *collection) RevisionField() string { return c.opts.RevisionField }

func (c *collection) RunActions(ctx context.Context, actions []*driver.Action, opts *driver.RunActionsOptions) driver.ActionListError {
	errs := make([]error, len(actions))
	beforeGets, gets, writes, writesTx, afterGets := driver.GroupActions(actions)
	c.runGets(ctx, beforeGets, errs, opts)
	ch := make(chan struct{})
	ch2 := make(chan struct{})
	go func() { defer close(ch); c.runWrites(ctx, writes, errs, opts) }()
	go func() { defer close(ch2); c.transactWrite(ctx, writesTx, errs, opts) }()
	c.runGets(ctx, gets, errs, opts)
	<-ch
	<-ch2
	c.runGets(ctx, afterGets, errs, opts)
	return driver.NewActionListError(errs)
}

func (c *collection) runGets(ctx context.Context, actions []*driver.Action, errs []error, opts *driver.RunActionsOptions) {
	const batchSize = 100
	t := driver.NewThrottle(c.opts.MaxOutstandingActionRPCs)
	for _, group := range driver.GroupByFieldPath(actions) {
		n := len(group) / batchSize
		for i := 0; i < n; i++ {
			i := i
			t.Acquire()
			go func(group []*driver.Action) {
				defer t.Release()
				c.batchGet(ctx, group, errs, opts, batchSize*i, batchSize*(i+1)-1)
			}(group)
		}
		if n*batchSize < len(group) {
			t.Acquire()
			go func(group []*driver.Action) {
				defer t.Release()
				c.batchGet(ctx, group, errs, opts, batchSize*n, len(group)-1)
			}(group)
		}
	}
	t.Wait()
}

func (c *collection) batchGet(ctx context.Context, gets []*driver.Action, errs []error, opts *driver.RunActionsOptions, start, end int) {
	// errors need to be mapped to the actions' indices.
	setErr := func(err error) {
		for i := start; i <= end; i++ {
			errs[gets[i].Index] = err
		}
	}

	keys := make([]map[string]dyn2Types.AttributeValue, 0, end-start+1)
	for i := start; i <= end; i++ {
		av, err := encodeDocKeyFields(gets[i].Doc, c.partitionKey, c.sortKey)
		if err != nil {
			errs[gets[i].Index] = err
		}

		keys = append(keys, av.Value)
	}
	ka := dyn2Types.KeysAndAttributes{
		Keys:           keys,
		ConsistentRead: aws.Bool(c.opts.ConsistentRead),
	}
	if len(gets[start].FieldPaths) != 0 {
		// We need to add the key fields if the user doesn't include them. The
		// BatchGet API doesn't return them otherwise.
		var hasP, hasS bool
		var nbs []expression.NameBuilder
		for _, fp := range gets[start].FieldPaths {
			p := strings.Join(fp, ".")
			nbs = append(nbs, expression.Name(p))
			if p == c.partitionKey {
				hasP = true
			} else if p == c.sortKey {
				hasS = true
			}
		}
		if !hasP {
			nbs = append(nbs, expression.Name(c.partitionKey))
		}
		if c.sortKey != "" && !hasS {
			nbs = append(nbs, expression.Name(c.sortKey))
		}
		expr, err := expression.NewBuilder().
			WithProjection(expression.AddNames(expression.ProjectionBuilder{}, nbs...)).
			Build()
		if err != nil {
			setErr(err)
			return
		}
		ka.ProjectionExpression = expr.Projection()
		ka.ExpressionAttributeNames = expr.Names()
	}
	in := &dyn.BatchGetItemInput{RequestItems: map[string]dyn2Types.KeysAndAttributes{c.table: ka}}
	if opts.BeforeDo != nil {
		if err := opts.BeforeDo(driver.AsFunc(in)); err != nil {
			setErr(err)
			return
		}
	}
	out, err := c.db.BatchGetItem(ctx, in)
	if err != nil {
		setErr(err)
		return
	}
	found := make([]bool, end-start+1)
	am := mapActionIndices(gets, start, end)
	for _, item := range out.Responses[c.table] {
		if item != nil {
			key := map[string]interface{}{c.partitionKey: nil}
			if c.sortKey != "" {
				key[c.sortKey] = nil
			}
			keysOnly, err := driver.NewDocument(key)
			if err != nil {
				panic(err)
			}
			err = decodeDoc(&dyn2Types.AttributeValueMemberM{Value: item}, keysOnly)
			if err != nil {
				continue
			}
			decKey, err := c.Key(keysOnly)
			if err != nil {
				continue
			}
			i := am[decKey]
			errs[gets[i].Index] = decodeDoc(&dyn2Types.AttributeValueMemberM{Value: item}, gets[i].Doc)
			found[i-start] = true
		}
	}
	for delta, f := range found {
		if !f {
			errs[gets[start+delta].Index] = gcerr.Newf(gcerr.NotFound, nil, "item %v not found", gets[start+delta].Doc)
		}
	}
}

func mapActionIndices(actions []*driver.Action, start, end int) map[interface{}]int {
	m := make(map[interface{}]int)
	for i := start; i <= end; i++ {
		m[actions[i].Key] = i
	}
	return m
}

// runWrites executes all the writes as separate RPCs, concurrently.
func (c *collection) runWrites(ctx context.Context, writes []*driver.Action, errs []error, opts *driver.RunActionsOptions) {
	var ops []*writeOp
	for _, w := range writes {
		op, err := c.newWriteOp(w, opts)
		if err != nil {
			errs[w.Index] = err
		} else {
			ops = append(ops, op)
		}
	}

	t := driver.NewThrottle(c.opts.MaxOutstandingActionRPCs)
	for _, op := range ops {
		op := op
		t.Acquire()
		go func() {
			defer t.Release()
			err := op.run(ctx)
			a := op.action
			if err != nil {
				errs[a.Index] = err
			} else {
				errs[a.Index] = c.onSuccess(op)
			}
		}()
	}
	t.Wait()
}

// A writeOp describes a single write to DynamoDB. The write can be executed
// on its own, or included as part of a transaction.
type writeOp struct {
	action          *driver.Action
	writeItem       dyn2Types.TransactWriteItem // for inclusion in a transaction
	newPartitionKey string                      // for a Create on a document without a partition key
	newRevision     string
	run             func(context.Context) error // run as a single RPC
}

func (c *collection) newWriteOp(a *driver.Action, opts *driver.RunActionsOptions) (*writeOp, error) {
	switch a.Kind {
	case driver.Create, driver.Replace, driver.Put:
		return c.newPut(a, opts)
	case driver.Update:
		return c.newUpdate(a, opts)
	case driver.Delete:
		return c.newDelete(a, opts)
	default:
		panic("bad write kind")
	}
}

func (c *collection) newPut(a *driver.Action, opts *driver.RunActionsOptions) (*writeOp, error) {
	av, err := encodeDoc(a.Doc)
	if err != nil {
		return nil, err
	}
	avm, ok := av.(*dyn2Types.AttributeValueMemberM)
	if !ok {
		// should never happen, but handle
		return nil, fmt.Errorf("unable to put document, unable to encode to map")
	}
	mf := c.missingKeyField(avm.Value)
	if a.Kind != driver.Create && mf != "" {
		return nil, fmt.Errorf("missing key field %q", mf)
	}
	var newPartitionKey string
	if mf == c.partitionKey {
		newPartitionKey = driver.UniqueString()
		avm.Value[c.partitionKey] = &dyn2Types.AttributeValueMemberS{Value: newPartitionKey}
	}
	if c.sortKey != "" && mf == c.sortKey {
		// It doesn't make sense to generate a random sort key.
		return nil, fmt.Errorf("missing sort key %q", c.sortKey)
	}
	var rev string
	if a.Doc.HasField(c.opts.RevisionField) {
		rev = driver.UniqueString()
		if avm.Value[c.opts.RevisionField], err = encodeValue(rev); err != nil {
			return nil, err
		}
	}
	dput := &dyn2Types.Put{
		TableName: &c.table,
		Item:      avm.Value,
	}
	cb, err := c.precondition(a)
	if err != nil {
		return nil, err
	}
	if cb != nil {
		ce, err := expression.NewBuilder().WithCondition(*cb).Build()
		if err != nil {
			return nil, err
		}
		dput.ExpressionAttributeNames = ce.Names()
		dput.ExpressionAttributeValues = ce.Values()
		dput.ConditionExpression = ce.Condition()
	}
	return &writeOp{
		action:          a,
		writeItem:       dyn2Types.TransactWriteItem{Put: dput},
		newPartitionKey: newPartitionKey,
		newRevision:     rev,
		run: func(ctx context.Context) error {
			return c.runPut(ctx, dput, a, opts)
		},
	}, nil
}

func (c *collection) runPut(ctx context.Context, dput *dyn2Types.Put, a *driver.Action, opts *driver.RunActionsOptions) error {
	in := &dyn.PutItemInput{
		TableName:                 dput.TableName,
		Item:                      dput.Item,
		ConditionExpression:       dput.ConditionExpression,
		ExpressionAttributeNames:  dput.ExpressionAttributeNames,
		ExpressionAttributeValues: dput.ExpressionAttributeValues,
	}
	if opts.BeforeDo != nil {
		if err := opts.BeforeDo(driver.AsFunc(in)); err != nil {
			return err
		}
	}
	_, err := c.db.PutItem(ctx, in)
	var ccf *dyn2Types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		if a.Kind == driver.Create {
			err = gcerr.Newf(gcerr.AlreadyExists, err, "document already exists")
		}
		if rev, _ := a.Doc.GetField(c.opts.RevisionField); rev == nil && a.Kind == driver.Replace {
			err = gcerr.Newf(gcerr.NotFound, nil, "document not found")
		}
	}
	return err
}

func (c *collection) newDelete(a *driver.Action, opts *driver.RunActionsOptions) (*writeOp, error) {
	av, err := encodeDocKeyFields(a.Doc, c.partitionKey, c.sortKey)
	if err != nil {
		return nil, err
	}

	del := &dyn2Types.Delete{
		TableName: &c.table,
		Key:       av.Value,
	}
	cb, err := c.precondition(a)
	if err != nil {
		return nil, err
	}
	if cb != nil {
		ce, err := expression.NewBuilder().WithCondition(*cb).Build()
		if err != nil {
			return nil, err
		}
		del.ExpressionAttributeNames = ce.Names()
		del.ExpressionAttributeValues = ce.Values()
		del.ConditionExpression = ce.Condition()
	}
	return &writeOp{
		action:    a,
		writeItem: dyn2Types.TransactWriteItem{Delete: del},
		run: func(ctx context.Context) error {
			in := &dyn.DeleteItemInput{
				TableName:                 del.TableName,
				Key:                       del.Key,
				ConditionExpression:       del.ConditionExpression,
				ExpressionAttributeNames:  del.ExpressionAttributeNames,
				ExpressionAttributeValues: del.ExpressionAttributeValues,
			}
			if opts.BeforeDo != nil {
				if err := opts.BeforeDo(driver.AsFunc(in)); err != nil {
					return err
				}
			}
			_, err := c.db.DeleteItem(ctx, in)
			return err
		},
	}, nil
}

func (c *collection) newUpdate(a *driver.Action, opts *driver.RunActionsOptions) (*writeOp, error) {
	av, err := encodeDocKeyFields(a.Doc, c.partitionKey, c.sortKey)
	if err != nil {
		return nil, err
	}

	var ub expression.UpdateBuilder
	for _, m := range a.Mods {
		// TODO(shantuo): check for invalid field paths
		fp := expression.Name(strings.Join(m.FieldPath, "."))
		if inc, ok := m.Value.(driver.IncOp); ok {
			ub = ub.Add(fp, expression.Value(inc.Amount))
		} else if m.Value == nil {
			ub = ub.Remove(fp)
		} else {
			ub = ub.Set(fp, expression.Value(m.Value))
		}
	}
	var rev string
	if a.Doc.HasField(c.opts.RevisionField) {
		rev = driver.UniqueString()
		ub = ub.Set(expression.Name(c.opts.RevisionField), expression.Value(rev))
	}
	cb, err := c.precondition(a)
	if err != nil {
		return nil, err
	}
	ce, err := expression.NewBuilder().WithCondition(*cb).WithUpdate(ub).Build()
	if err != nil {
		return nil, err
	}
	up := &dyn2Types.Update{
		TableName:                 &c.table,
		Key:                       av.Value,
		ConditionExpression:       ce.Condition(),
		UpdateExpression:          ce.Update(),
		ExpressionAttributeNames:  ce.Names(),
		ExpressionAttributeValues: ce.Values(),
	}
	return &writeOp{
		action:      a,
		writeItem:   dyn2Types.TransactWriteItem{Update: up},
		newRevision: rev,
		run: func(ctx context.Context) error {
			in := &dyn.UpdateItemInput{
				TableName:                 up.TableName,
				Key:                       up.Key,
				ConditionExpression:       up.ConditionExpression,
				UpdateExpression:          up.UpdateExpression,
				ExpressionAttributeNames:  up.ExpressionAttributeNames,
				ExpressionAttributeValues: up.ExpressionAttributeValues,
			}
			if opts.BeforeDo != nil {
				if err := opts.BeforeDo(driver.AsFunc(in)); err != nil {
					return err
				}
			}
			_, err := c.db.UpdateItem(ctx, in)
			return err
		},
	}, nil
}

// Handle the effects of successful execution.
func (c *collection) onSuccess(op *writeOp) error {
	// Set the new partition key (if any) and the new revision into the user's document.
	if op.newPartitionKey != "" {
		_ = op.action.Doc.SetField(c.partitionKey, op.newPartitionKey) // cannot fail
	}
	if op.newRevision != "" {
		return op.action.Doc.SetField(c.opts.RevisionField, op.newRevision)
	}
	return nil
}

func (c *collection) missingKeyField(m map[string]dyn2Types.AttributeValue) string {
	v, ok := m[c.partitionKey]
	if !ok {
		return c.partitionKey
	}
	if n, ok := v.(*dyn2Types.AttributeValueMemberNULL); ok && n.Value {
		return c.partitionKey
	}

	if c.sortKey != "" {
		v, ok := m[c.sortKey]
		if !ok {
			return c.sortKey
		}
		if n, ok := v.(*dyn2Types.AttributeValueMemberNULL); ok && n.Value {
			return c.sortKey
		}
	}

	return ""
}

// Construct the precondition for the action.
func (c *collection) precondition(a *driver.Action) (*expression.ConditionBuilder, error) {
	switch a.Kind {
	case driver.Create:
		// Precondition: the document doesn't already exist. (Precisely: the partitionKey
		// field is not on the document.)
		c := expression.AttributeNotExists(expression.Name(c.partitionKey))
		return &c, nil
	case driver.Replace, driver.Update:
		// Precondition: the revision matches, or if there is no revision, then
		// the document exists.
		cb, err := revisionPrecondition(a.Doc, c.opts.RevisionField)
		if err != nil {
			return nil, err
		}
		if cb == nil {
			c := expression.AttributeExists(expression.Name(c.partitionKey))
			cb = &c
		}
		return cb, nil
	case driver.Put, driver.Delete:
		// Precondition: the revision matches, if any.
		return revisionPrecondition(a.Doc, c.opts.RevisionField)
	case driver.Get:
		// No preconditions on a Get.
		return nil, nil
	default:
		panic("bad action kind")
	}
}

// revisionPrecondition returns a DynamoDB expression that asserts that the
// stored document's revision matches the revision of doc.
func revisionPrecondition(doc driver.Document, revField string) (*expression.ConditionBuilder, error) {
	v, err := doc.GetField(revField)
	if err != nil { // field not present
		return nil, nil
	}
	if v == nil { // field is present, but nil
		return nil, nil
	}
	rev, ok := v.(string)
	if !ok {
		return nil, gcerr.Newf(gcerr.InvalidArgument, nil,
			"%s field contains wrong type: got %T, want string",
			revField, v)
	}
	if rev == "" {
		return nil, nil
	}
	// Value encodes rev to an attribute value.
	cb := expression.Name(revField).Equal(expression.Value(rev))
	return &cb, nil
}

// transactWrite executes the write actions atomically: either they all succeed or they all fail together.
func (c *collection) transactWrite(ctx context.Context, actions []*driver.Action, errs []error, opts *driver.RunActionsOptions) {
	if len(actions) == 0 {
		return
	}
	// all actions will fail atomically even if a single action fails
	setErr := func(err error) {
		for _, a := range actions {
			errs[a.Index] = err
		}
	}

	tws := make([]dyn2Types.TransactWriteItem, 0, len(actions))
	var ops []*writeOp
	for _, w := range actions {
		op, err := c.newWriteOp(w, opts)
		if err != nil {
			setErr(err)
			return
		} else {
			ops = append(ops, op)
			tws = append(tws, op.writeItem)
		}
	}

	in := &dyn.TransactWriteItemsInput{
		ClientRequestToken: aws.String(driver.UniqueString()),
		TransactItems:      tws,
	}

	if opts.BeforeDo != nil {
		asFunc := func(i interface{}) bool {
			p, ok := i.(**dyn.TransactWriteItemsInput)
			if !ok {
				return false
			}
			*p = in
			return true
		}
		if err := opts.BeforeDo(asFunc); err != nil {
			setErr(err)
			return
		}
	}
	if _, err := c.db.TransactWriteItems(ctx, in); err != nil {
		setErr(err)
		return
	}
	for _, op := range ops {
		errs[op.action.Index] = c.onSuccess(op)
	}
}

// RevisionToBytes implements driver.RevisionToBytes.
func (c *collection) RevisionToBytes(rev interface{}) ([]byte, error) {
	s, ok := rev.(string)
	if !ok {
		return nil, gcerr.Newf(gcerr.InvalidArgument, nil, "revision %v of type %[1]T is not a string", rev)
	}
	return []byte(s), nil
}

// BytesToRevision implements driver.BytesToRevision.
func (c *collection) BytesToRevision(b []byte) (interface{}, error) {
	return string(b), nil
}

func (c *collection) As(i interface{}) bool {
	p, ok := i.(**dyn.Client)
	if !ok {
		return false
	}
	*p = c.db
	return true
}

// ErrorAs implements driver.Collection.ErrorAs.
func (c *collection) ErrorAs(err error, i interface{}) bool {
	e, ok := err.(*smithy.OperationError)
	if !ok {
		return false
	}
	p, ok := i.(*smithy.OperationError)
	if !ok {
		return false
	}
	*p = *e
	return true
}

func (c *collection) ErrorCode(err error) gcerrors.ErrorCode {

	var (
		conditionalCheckFailedException          *dyn2Types.ConditionalCheckFailedException
		idempotentParameterMismatchException     *dyn2Types.IdempotentParameterMismatchException
		internalServerError                      *dyn2Types.InternalServerError
		itemCollectionSizeLimitExceededException *dyn2Types.ItemCollectionSizeLimitExceededException
		provisionedThroughputExceededException   *dyn2Types.ProvisionedThroughputExceededException
		requestLimitExceeded                     *dyn2Types.RequestLimitExceeded
		resourceNotFoundException                *dyn2Types.ResourceNotFoundException
		transactionCanceledException             *dyn2Types.TransactionCanceledException
		transactionConflictException             *dyn2Types.TransactionConflictException
		transactionInProgressException           *dyn2Types.TransactionInProgressException
	)

	switch {
	case errors.As(err, &conditionalCheckFailedException):
		return gcerrors.FailedPrecondition
	case errors.As(err, &idempotentParameterMismatchException):
		return gcerrors.InvalidArgument
	case errors.As(err, &internalServerError):
		return gcerrors.Internal
	case errors.As(err, &itemCollectionSizeLimitExceededException):
		return gcerrors.ResourceExhausted
	case errors.As(err, &provisionedThroughputExceededException):
		return gcerrors.ResourceExhausted
	case errors.As(err, &requestLimitExceeded):
		return gcerrors.ResourceExhausted
	case errors.As(err, &resourceNotFoundException):
		return gcerrors.NotFound
	case errors.As(err, &transactionCanceledException):
		return gcerrors.FailedPrecondition
	case errors.As(err, &transactionConflictException):
		return gcerrors.Internal
	case errors.As(err, &transactionInProgressException):
		return gcerrors.InvalidArgument
	}

	for k, errCode := range errorCodeMap {
		if strings.Contains(err.Error(), k) {
			return errCode
		}
	}

	return gcerrors.Unknown
}

var errorCodeMap = map[string]gcerrors.ErrorCode{
	// handled by type above
	// "ConditionalCheckFailedException":          gcerrors.FailedPrecondition,
	// "IdempotentParameterMismatchException":     gcerrors.InvalidArgument,
	// "InternalServerErrorException":             gcerrors.Internal,
	// "ItemCollectionSizeLimitExceededException": gcerrors.ResourceExhausted,
	// "ProvisionedThroughputExceededException":   gcerrors.ResourceExhausted,
	// "RequestLimitExceededException":            gcerrors.ResourceExhausted,
	// "ResourceNotFoundException":                gcerrors.NotFound,
	// "TransactionCanceledException":             gcerrors.FailedPrecondition,
	// "TransactionConflictException":             gcerrors.Internal,
	// "TransactionInProgressException":           gcerrors.InvalidArgument,

	// V2 doesn't have a type for this, inspect message instead
	"ValidationException": gcerrors.InvalidArgument,
}

// Close implements driver.Collection.Close.
func (c *collection) Close() error { return nil }
