// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package merkledb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"golang.org/x/sync/errgroup"
	"sync"

	"go.opentelemetry.io/otel/attribute"

	oteltrace "go.opentelemetry.io/otel/trace"

	"golang.org/x/exp/slices"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/maybe"
)

const (
	initKeyValuesSize        = 256
	defaultPreallocationSize = 100
)

var (
	_ TrieView = (*trieView)(nil)

	ErrCommitted                  = errors.New("view has been committed")
	ErrInvalid                    = errors.New("the trie this view was based on has changed, rendering this view invalid")
	ErrPartialByteLengthWithValue = errors.New(
		"the underlying db only supports whole number of byte keys, so cannot record changes with partial byte lengths",
	)
	ErrGetPathToFailure       = errors.New("GetPathTo failed to return the closest node")
	ErrStartAfterEnd          = errors.New("start key > end key")
	ErrNoValidRoot            = errors.New("a valid root was not provided to the trieView constructor")
	ErrParentNotDatabase      = errors.New("parent trie is not database")
	ErrNodesAlreadyCalculated = errors.New("cannot modify the trie after the node changes have been calculated")
)

type trieView struct {
	// If true, this view has been committed.
	// [commitLock] must be held while accessing this field.
	committed  bool
	commitLock sync.RWMutex

	// tracking bool to enforce that no changes are made to the trie after the nodes have been calculated
	nodesAlreadyCalculated utils.Atomic[bool]

	// calculateNodesOnce is a once to ensure that node calculation only occurs a single time
	calculateNodesOnce sync.Once

	// Controls the trie's validity related fields.
	// Must be held while reading/writing [childViews], [invalidated], and [parentTrie].
	// Only use to lock current trieView or descendants of the current trieView
	// DO NOT grab the [validityTrackingLock] of any ancestor trie while this is held.
	validityTrackingLock sync.RWMutex

	// If true, this view has been invalidated and can't be used.
	//
	// Invariant: This view is marked as invalid before any of its ancestors change.
	// Since we ensure that all subviews are marked invalid before making an invalidating change
	// then if we are still valid at the end of the function, then no corrupting changes could have
	// occurred during execution.
	// Namely, if we have a method with:
	//
	// *Code Accessing Ancestor State*
	//
	// if t.isInvalid() {
	//     return ErrInvalid
	//  }
	// return [result]
	//
	// If the invalidated check passes, then we're guaranteed that no ancestor changes occurred
	// during the code that accessed ancestor state and the result of that work is still valid
	//
	// [validityTrackingLock] must be held when reading/writing this field.
	invalidated bool

	// the uncommitted parent trie of this view
	// [validityTrackingLock] must be held when reading/writing this field.
	parentTrie TrieView

	// The valid children of this trie.
	// [validityTrackingLock] must be held when reading/writing this field.
	childViews []*trieView

	// Changes made to this view.
	// May include nodes that haven't been updated
	// but will when their ID is recalculated.
	changes *changeSummary

	db *merkleDB

	// The root of the trie represented by this view.
	root *node

	tokenSize int
}

// NewView returns a new view on top of this Trie where the passed changes
// have been applied.
// Adds the new view to [t.childViews].
// Assumes [t.commitLock] isn't held.
func (t *trieView) NewView(
	ctx context.Context,
	changes ViewChanges,
) (TrieView, error) {
	if t.isInvalid() {
		return nil, ErrInvalid
	}
	t.commitLock.RLock()
	defer t.commitLock.RUnlock()

	if t.committed {
		return t.getParentTrie().NewView(ctx, changes)
	}

	if err := t.calculateNodeIDs(ctx); err != nil {
		return nil, err
	}

	newView, err := newTrieView(t.db, t, changes)
	if err != nil {
		return nil, err
	}

	t.validityTrackingLock.Lock()
	defer t.validityTrackingLock.Unlock()

	if t.invalidated {
		return nil, ErrInvalid
	}
	t.childViews = append(t.childViews, newView)

	return newView, nil
}

// Creates a new view with the given [parentTrie].
// Assumes [parentTrie] isn't locked.
func newTrieView(
	db *merkleDB,
	parentTrie TrieView,
	changes ViewChanges,
) (*trieView, error) {
	root, err := parentTrie.getEditableNode(Key{}, false /* hasValue */)
	if err != nil {
		if err == database.ErrNotFound {
			return nil, ErrNoValidRoot
		}
		return nil, err
	}

	newView := &trieView{
		root:       root,
		db:         db,
		parentTrie: parentTrie,
		changes:    newChangeSummary(len(changes.BatchOps) + len(changes.MapOps)),
		tokenSize:  db.tokenSize,
	}

	for _, op := range changes.BatchOps {
		key := op.Key
		if !changes.ConsumeBytes {
			key = slices.Clone(op.Key)
		}

		newVal := maybe.Nothing[[]byte]()
		if !op.Delete {
			newVal = maybe.Some(op.Value)
			if !changes.ConsumeBytes {
				newVal = maybe.Some(slices.Clone(op.Value))
			}
		}
		if err := newView.recordValueChange(toKey(key), newVal); err != nil {
			return nil, err
		}
	}
	for key, val := range changes.MapOps {
		if !changes.ConsumeBytes {
			val = maybe.Bind(val, slices.Clone[[]byte])
		}
		if err := newView.recordValueChange(toKey(stringToByteSlice(key)), val); err != nil {
			return nil, err
		}
	}
	return newView, nil
}

func newHistoricalTrieView(
	db *merkleDB,
	changes *changeSummary,
) (*trieView, error) {

	changeOps := make([]database.BatchOp, 0, len(changes.values))
	for key, valueChange := range changes.values {
		changeOps = append(changeOps, database.BatchOp{Key: key.Bytes(), Value: valueChange.after.Value(), Delete: valueChange.after.IsNothing()})
	}
	return newTrieView(db, db, ViewChanges{BatchOps: changeOps})
}

// Recalculates the node IDs for all changed nodes in the trie.
// Cancelling [ctx] doesn't cancel calculation. It's used only for tracing.
func (t *trieView) calculateNodeIDs(ctx context.Context) error {
	var err error
	t.calculateNodesOnce.Do(func() {
		if t.isInvalid() {
			err = ErrInvalid
			return
		}
		defer t.nodesAlreadyCalculated.Set(true)

		// We wait to create the span until after checking that we need to actually
		// calculateNodeIDs to make traces more useful (otherwise there may be a span
		// per key modified even though IDs are not re-calculated).
		_, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.calculateNodeIDs")
		defer span.End()

		// add all the changed key/values to the nodes of the trie
		for key, change := range t.changes.values {
			if change.after.IsNothing() {
				// Note we're setting [err] defined outside this function.
				if err = t.remove(key); err != nil {
					return
				}
				// Note we're setting [err] defined outside this function.
			} else if _, err = t.insert(key, change.after); err != nil {
				return
			}
		}

		_ = t.db.calculateNodeIDsSema.Acquire(context.Background(), 1)
		var _ maybe.Maybe[[]byte]
		_, err = t.getValue(emptyKey)
		if err != nil {
			return
		}
		t.changes.rootID, err = t.calculateNodeIDsHelper(Key{}, t.root, t.root.value)
		if err != nil {
			return
		}
		t.db.calculateNodeIDsSema.Release(1)

		// ensure no ancestor changes occurred during execution
		if t.isInvalid() {
			err = ErrInvalid
			return
		}
	})
	return err
}

type nodeInfo struct {
	key      Key
	id       ids.ID
	hasValue bool
}

// Calculates the ID of all descendants of [n] which need to be recalculated,
// and then calculates the ID of [n] itself.
func (t *trieView) calculateNodeIDsHelper(key Key, n *node, val maybe.Maybe[[]byte]) (ids.ID, error) {
	var (
		// We use [wg] to wait until all descendants of [n] have been updated.
		wg              sync.WaitGroup
		eg              errgroup.Group
		updatedChildren = make(chan nodeInfo, len(n.children))
	)

	for childIndex, child := range n.children {
		childKey := key.Extend(ToToken(childIndex, t.tokenSize), child.compressedKey)
		childNodeChange, ok := t.changes.nodes[childKey]
		if !ok {
			// This child wasn't changed.
			continue
		}

		wg.Add(1)
		calculateChildID := func() error {
			defer wg.Done()
			_, err := t.getValue(childKey)
			if err != nil {
				return err
			}
			id, err := t.calculateNodeIDsHelper(childKey, childNodeChange.after, childNodeChange.after.value)
			if err != nil {
				return err
			}
			// Note that this will never block
			updatedChildren <- nodeInfo{
				key:      childKey,
				id:       id,
				hasValue: childNodeChange.after.value.HasValue(),
			}
			return nil
		}

		// Try updating the child and its descendants in a goroutine.
		if ok := t.db.calculateNodeIDsSema.TryAcquire(1); ok {
			eg.Go(func() error {
				defer t.db.calculateNodeIDsSema.Release(1)
				return calculateChildID()
			})
		} else {
			// We're at the goroutine limit; do the work in this goroutine.
			if err := calculateChildID(); err != nil {
				return ids.Empty, err
			}
		}
	}

	// Wait until all descendants of [n] have been updated.
	wg.Wait()
	if err := eg.Wait(); err != nil {
		return ids.Empty, err
	}
	close(updatedChildren)

	for updatedChild := range updatedChildren {
		index := updatedChild.key.Token(key.length, t.tokenSize)
		n.setChildEntry(index, child{
			compressedKey: n.children[index].compressedKey,
			id:            updatedChild.id,
			hasValue:      updatedChild.hasValue,
		})
	}

	// The IDs [n]'s descendants are up to date so we can calculate [n]'s ID.
	return n.calculateID(key, t.db.metrics, n.value), nil
}

// GetProof returns a proof that [bytesPath] is in or not in trie [t].
func (t *trieView) GetProof(ctx context.Context, key []byte) (*Proof, error) {
	_, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.GetProof")
	defer span.End()

	if err := t.calculateNodeIDs(ctx); err != nil {
		return nil, err
	}

	return t.getProof(ctx, key)
}

// Returns a proof that [bytesPath] is in or not in trie [t].
func (t *trieView) getProof(ctx context.Context, key []byte) (*Proof, error) {
	_, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.getProof")
	defer span.End()

	proof := &Proof{
		Key: ToKey(key),
	}

	value, err := t.getValue(proof.Key)
	if err != nil {
		return nil, err
	}
	proof.Value = value

	var (
		closestKey  Key
		closestNode *node
	)
	if err := t.visitPathToKey(proof.Key, func(key Key, n *node) error {
		closestKey = key
		closestNode = n
		_, err := t.getValue(key)
		if err != nil {
			return err
		}
		proof.Path = append(proof.Path, n.asProofNode(key, closestNode.value))
		return nil
	}); err != nil {
		return nil, err
	}

	if closestKey == proof.Key {
		return proof, nil
	}

	// There is no node with the given [key].
	// If there is a child at the index where the node would be
	// if it existed, include that child in the proof.
	nextIndex := proof.Key.Token(closestKey.length, t.tokenSize)
	child, ok := closestNode.children[nextIndex]
	if !ok {
		return proof, nil
	}

	childKey := closestKey.Extend(ToToken(nextIndex, t.tokenSize), child.compressedKey)
	childNode, err := t.getNode(
		childKey,
		child.hasValue,
	)
	if err != nil {
		return nil, err
	}
	_, err = t.getValue(childKey)
	if err != nil {
		return nil, err
	}
	proof.Path = append(proof.Path, childNode.asProofNode(childKey, childNode.value))
	if t.isInvalid() {
		return nil, ErrInvalid
	}
	return proof, nil
}

// GetRangeProof returns a range proof for (at least part of) the key range [start, end].
// The returned proof's [KeyValues] has at most [maxLength] values.
// [maxLength] must be > 0.
func (t *trieView) GetRangeProof(
	ctx context.Context,
	start maybe.Maybe[[]byte],
	end maybe.Maybe[[]byte],
	maxLength int,
) (*RangeProof, error) {
	ctx, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.GetRangeProof")
	defer span.End()

	if start.HasValue() && end.HasValue() && bytes.Compare(start.Value(), end.Value()) == 1 {
		return nil, ErrStartAfterEnd
	}

	if maxLength <= 0 {
		return nil, fmt.Errorf("%w but was %d", ErrInvalidMaxLength, maxLength)
	}

	if err := t.calculateNodeIDs(ctx); err != nil {
		return nil, err
	}

	var result RangeProof

	result.KeyValues = make([]KeyValue, 0, initKeyValuesSize)
	it := t.NewIteratorWithStart(start.Value())
	for it.Next() && len(result.KeyValues) < maxLength && (end.IsNothing() || bytes.Compare(it.Key(), end.Value()) <= 0) {
		// clone the value to prevent editing of the values stored within the trie
		result.KeyValues = append(result.KeyValues, KeyValue{
			Key:   it.Key(),
			Value: slices.Clone(it.Value()),
		})
	}
	it.Release()
	if err := it.Error(); err != nil {
		return nil, err
	}

	// This proof may not contain all key-value pairs in [start, end] due to size limitations.
	// The end proof we provide should be for the last key-value pair in the proof, not for
	// the last key-value pair requested, which may not be in this proof.
	var (
		endProof *Proof
		err      error
	)
	if len(result.KeyValues) > 0 {
		greatestKey := result.KeyValues[len(result.KeyValues)-1].Key
		endProof, err = t.getProof(ctx, greatestKey)
		if err != nil {
			return nil, err
		}
	} else if end.HasValue() {
		endProof, err = t.getProof(ctx, end.Value())
		if err != nil {
			return nil, err
		}
	}
	if endProof != nil {
		result.EndProof = endProof.Path
	}

	if start.HasValue() {
		startProof, err := t.getProof(ctx, start.Value())
		if err != nil {
			return nil, err
		}
		result.StartProof = startProof.Path

		// strip out any common nodes to reduce proof size
		i := 0
		for ; i < len(result.StartProof) &&
			i < len(result.EndProof) &&
			result.StartProof[i].Key == result.EndProof[i].Key; i++ {
		}
		result.StartProof = result.StartProof[i:]
	}

	if len(result.StartProof) == 0 && len(result.EndProof) == 0 && len(result.KeyValues) == 0 {
		// If the range is empty, return the root proof.
		rootProof, err := t.getProof(ctx, rootKey)
		if err != nil {
			return nil, err
		}
		result.EndProof = rootProof.Path
	}

	if t.isInvalid() {
		return nil, ErrInvalid
	}
	return &result, nil
}

// CommitToDB commits changes from this trie to the underlying DB.
func (t *trieView) CommitToDB(ctx context.Context) error {
	ctx, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.CommitToDB")
	defer span.End()

	t.db.commitLock.Lock()
	defer t.db.commitLock.Unlock()

	return t.commitToDB(ctx)
}

// Commits the changes from [trieToCommit] to this view,
// this view to its parent, and so on until committing to the db.
// Assumes [t.db.commitLock] is held.
func (t *trieView) commitToDB(ctx context.Context) error {
	t.commitLock.Lock()
	defer t.commitLock.Unlock()

	ctx, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.commitToDB", oteltrace.WithAttributes(
		attribute.Int("changeCount", len(t.changes.values)),
	))
	defer span.End()

	// Call this here instead of in [t.db.commitChanges]
	// because doing so there would be a deadlock.
	if err := t.calculateNodeIDs(ctx); err != nil {
		return err
	}

	if err := t.db.commitChanges(ctx, t); err != nil {
		return err
	}

	t.committed = true

	return nil
}

// Assumes [t.validityTrackingLock] isn't held.
func (t *trieView) isInvalid() bool {
	t.validityTrackingLock.RLock()
	defer t.validityTrackingLock.RUnlock()

	return t.invalidated
}

// Invalidates this view and all descendants.
// Assumes [t.validityTrackingLock] isn't held.
func (t *trieView) invalidate() {
	t.validityTrackingLock.Lock()
	defer t.validityTrackingLock.Unlock()

	t.invalidated = true

	for _, childView := range t.childViews {
		childView.invalidate()
	}

	// after invalidating the children, they no longer need to be tracked
	t.childViews = make([]*trieView, 0, defaultPreallocationSize)
}

func (t *trieView) updateParent(newParent TrieView) {
	t.validityTrackingLock.Lock()
	defer t.validityTrackingLock.Unlock()

	t.parentTrie = newParent
}

// GetMerkleRoot returns the ID of the root of this trie.
func (t *trieView) GetMerkleRoot(ctx context.Context) (ids.ID, error) {
	if err := t.calculateNodeIDs(ctx); err != nil {
		return ids.Empty, err
	}
	return t.changes.rootID, nil
}

func (t *trieView) GetValues(ctx context.Context, keys [][]byte) ([][]byte, []error) {
	_, span := t.db.debugTracer.Start(ctx, "MerkleDB.trieview.GetValues", oteltrace.WithAttributes(
		attribute.Int("keyCount", len(keys)),
	))
	defer span.End()

	results := make([][]byte, len(keys))
	valueErrors := make([]error, len(keys))

	for i, key := range keys {
		val, err := t.getValueCopy(ToKey(key))
		switch {
		case err != nil:
			valueErrors[i] = err
		case val.IsNothing():
			valueErrors[i] = database.ErrNotFound
		default:
			results[i] = val.Value()
		}
	}
	return results, valueErrors
}

// GetValue returns the value for the given [key].
// Returns database.ErrNotFound if it doesn't exist.
func (t *trieView) GetValue(ctx context.Context, key []byte) ([]byte, error) {
	_, span := t.db.debugTracer.Start(ctx, "MerkleDB.trieview.GetValue")
	defer span.End()

	val, err := t.getValueCopy(ToKey(key))
	if err != nil {
		return nil, err
	}
	if val.IsNothing() {
		return nil, database.ErrNotFound
	}
	return val.Value(), nil
}

// getValueCopy returns a copy of the value for the given [key].
// Returns database.ErrNotFound if it doesn't exist.
func (t *trieView) getValueCopy(key Key) (maybe.Maybe[[]byte], error) {
	val, err := t.getValue(key)
	if err != nil {
		return maybe.Nothing[[]byte](), err
	}
	return maybe.Bind(val, slices.Clone[[]byte]), nil
}

func (t *trieView) getValue(key Key) (maybe.Maybe[[]byte], error) {
	if t.isInvalid() {
		return maybe.Nothing[[]byte](), ErrInvalid
	}

	if change, ok := t.changes.values[key]; ok {
		t.db.metrics.ViewValueCacheHit()
		return change.after, nil
	}
	t.db.metrics.ViewValueCacheMiss()

	// if we don't have local copy of the key, then grab a copy from the parent trie
	value, err := t.getParentTrie().getValue(key)
	if err != nil {
		return maybe.Nothing[[]byte](), err
	}

	// ensure no ancestor changes occurred during execution
	if t.isInvalid() {
		return maybe.Nothing[[]byte](), ErrInvalid
	}

	return value, nil
}

// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) remove(key Key) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	if err := t.recordValueChange(key, maybe.Nothing[[]byte]()); err != nil {
		return err
	}

	val, valChanged := t.changes.values[key]
	if !valChanged || val.before.IsNothing() || val.after.HasValue() {
		return nil
	}

	// if the node exists and contains a value
	// mark all ancestor for change
	// grab parent and grandparent nodes for path compression
	var (
		grandParentKey, parentKey, nodeToDeleteKey Key
		grandParent, parent, nodeToDelete          *node
	)
	if err := t.visitPathToKey(key, func(key Key, n *node) error {
		grandParentKey = parentKey
		parentKey = nodeToDeleteKey
		nodeToDeleteKey = key

		grandParent = parent
		parent = nodeToDelete
		nodeToDelete = n
		return t.recordNodeChange(key, n)
	}); err != nil {
		return err
	}

	nodeToDelete.setValue(maybe.Nothing[[]byte]())
	if len(nodeToDelete.children) != 0 {
		// merge this node and its child into a single node if possible
		return t.compressNodePath(parent, parentKey, nodeToDelete, nodeToDeleteKey)
	}

	// if the removed node has no children, the node can be removed from the trie
	if err := t.recordNodeDeleted(nodeToDeleteKey, nodeToDelete); err != nil {
		return err
	}
	if parent != nil {
		delete(parent.children, nodeToDeleteKey.Token(parentKey.length, t.tokenSize))

		// merge the parent node and its child into a single node if possible
		return t.compressNodePath(grandParent, grandParentKey, parent, parentKey)
	}
	return nil
}

// Merges together nodes in the inclusive descendants of [node] that
// have no value and a single child into one node with a compressed
// path until a node that doesn't meet those criteria is reached.
// [parent] is [node]'s parent.
// Assumes at least one of the following is true:
// * [node] has a value.
// * [node] has children.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) compressNodePath(parent *node, parentKey Key, node *node, nodeKey Key) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	val, err := t.getValue(nodeKey)
	if err != nil {
		return err
	}
	// don't collapse into this node if it's the root, doesn't have 1 child, or has a value
	if parent == nil || len(node.children) != 1 || val.HasValue() {
		return nil
	}

	if err := t.recordNodeDeleted(nodeKey, node); err != nil {
		return err
	}

	var (
		childEntry child
		childKey   Key
	)
	// There is only one child, but we don't know the index.
	// "Cycle" over the key/values to find the only child.
	// Note this iteration once because len(node.children) == 1.
	for index, entry := range node.children {
		childKey = nodeKey.Extend(ToToken(index, t.tokenSize), entry.compressedKey)
		childEntry = entry
	}

	// [node] is the first node with multiple children.
	// combine it with the [node] passed in.
	parent.setChildEntry(childKey.Token(parentKey.length, t.tokenSize),
		child{
			compressedKey: childKey.Skip(parentKey.length + t.tokenSize),
			id:            childEntry.id,
			hasValue:      childEntry.hasValue,
		})
	return t.recordNodeChange(parentKey, parent)
}

// Returns the nodes along the path to [key].
// The first node is the root, and the last node is either the node with the
// given [key], if it's in the trie, or the node with the largest prefix of
// the [key] if it isn't in the trie.
// Always returns at least the root node.
func (t *trieView) visitPathToKey(key Key, visitNode func(Key, *node) error) error {
	var (
		// all node paths start at the root
		currentNode = t.root
		currentKey  = Key{}
		err         error
	)
	if err := visitNode(currentKey, currentNode); err != nil {
		return err
	}
	// while the entire path hasn't been matched
	for currentKey.length < key.length {
		// confirm that a child exists and grab its ID before attempting to load it
		nextChildEntry, hasChild := currentNode.children[key.Token(currentKey.length, t.tokenSize)]

		if !hasChild || !key.iteratedHasPrefix(nextChildEntry.compressedKey, currentKey.length+t.tokenSize, t.tokenSize) {
			// there was no child along the path or the child that was there doesn't match the remaining path
			return nil
		}
		// grab the next node along the path
		currentKey = key.Take(currentKey.length + t.tokenSize + nextChildEntry.compressedKey.length)
		currentNode, err = t.getNode(currentKey, nextChildEntry.hasValue)
		if err != nil {
			return err
		}
		if err := visitNode(currentKey, currentNode); err != nil {
			return err
		}
	}
	return nil
}

// Get a copy of the node matching the passed key from the trie.
// Used by views to get nodes from their ancestors.
func (t *trieView) getEditableNode(key Key, hadValue bool) (*node, error) {
	if t.isInvalid() {
		return nil, ErrInvalid
	}

	// grab the node in question
	n, err := t.getNode(key, hadValue)
	if err != nil {
		return nil, err
	}

	// ensure no ancestor changes occurred during execution
	if t.isInvalid() {
		return nil, ErrInvalid
	}

	// return a clone of the node, so it can be edited without affecting this trie
	return n.clone(), nil
}

// insert a key/value pair into the correct node of the trie.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) insert(
	key Key,
	value maybe.Maybe[[]byte],
) (*node, error) {
	if t.nodesAlreadyCalculated.Get() {
		return nil, ErrNodesAlreadyCalculated
	}

	if err := t.recordValueChange(key, value); err != nil {
		return nil, err
	}

	var (
		closestKey  Key
		closestNode *node
	)
	if err := t.visitPathToKey(key, func(key Key, n *node) error {
		closestKey = key
		closestNode = n
		return t.recordNodeChange(key, n)
	}); err != nil {
		return nil, err
	}

	// a node with that exact key already exists so update its value
	if closestKey == key {
		closestNode.setValue(value)
		// closestNode was already marked as changed in the ancestry loop above
		return closestNode, nil
	}

	// A node with the exact key doesn't exist so determine the portion of the
	// key that hasn't been matched yet
	// Note that [key] has prefix [closestNodeKey] but exactMatch was false,
	// so [key] must be longer than [key] and the following index and slice won't OOB.
	existingChildEntry, hasChild := closestNode.children[key.Token(closestKey.length, t.tokenSize)]
	if !hasChild {
		// there are no existing nodes along the key [key], so create a new node to insert [value]
		newNode := newNode()
		newNode.setValue(value)
		closestNode.setChildEntry(key.Token(closestKey.length, t.tokenSize), child{compressedKey: key.Skip(closestKey.length + t.tokenSize)})
		return newNode, t.recordNewNode(key, newNode)
	}

	// if we have reached this point, then the [key] we are trying to insert and
	// the existing path node have some common prefix.
	// a new branching node will be created that will represent this common prefix and
	// have the existing path node and the value being inserted as children.

	// generate the new branch node
	// find how many tokens are common between the existing child's compressed path and
	// the current key(offset by the closest node's key),
	// then move all the common tokens into the branch node
	commonPrefixLength := getLengthOfCommonPrefix(existingChildEntry.compressedKey, key, closestKey.length+t.tokenSize, t.tokenSize)

	// If the length of the existing child's compressed path is less than or equal to the branch node's key that implies that the existing child's key matched the key to be inserted.
	// Since it matched the key to be inserted, it should have been the last node returned by GetPathTo
	if existingChildEntry.compressedKey.length <= commonPrefixLength {
		return nil, ErrGetPathToFailure
	}

	branchNode := newNode()
	branchKey := key.Take(closestKey.length + t.tokenSize + commonPrefixLength)
	closestNode.setChildEntry(branchKey.Token(closestKey.length, t.tokenSize), child{compressedKey: branchKey.Skip(closestKey.length + t.tokenSize)})
	nodeWithValue := branchNode

	if key.length == branchKey.length {
		// the branch node has exactly the key to be inserted as its key, so set the value on the branch node
		branchNode.setValue(value)
	} else {
		// the key to be inserted is a child of the branch node
		// create a new node and add the value to it
		newNode := newNode()
		newNode.setValue(value)
		branchNode.setChildEntry(key.Token(branchKey.length, t.tokenSize), child{compressedKey: key.Skip(branchKey.length + t.tokenSize)})
		if err := t.recordNewNode(key, newNode); err != nil {
			return nil, err
		}
		nodeWithValue = newNode
	}

	// add the existing child onto the branch node
	branchNode.setChildEntry(
		existingChildEntry.compressedKey.Token(commonPrefixLength, t.tokenSize),
		child{
			compressedKey: existingChildEntry.compressedKey.Skip(commonPrefixLength + t.tokenSize),
			id:            existingChildEntry.id,
			hasValue:      existingChildEntry.hasValue,
		})

	return nodeWithValue, t.recordNewNode(branchKey, branchNode)
}

func getLengthOfCommonPrefix(first, second Key, secondOffset int, tokenSize int) int {
	commonIndex := 0
	for first.length > commonIndex && second.length > commonIndex+secondOffset &&
		first.Token(commonIndex, tokenSize) == second.Token(commonIndex+secondOffset, tokenSize) {
		commonIndex += tokenSize
	}
	return commonIndex
}

// Records that a node has been created.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) recordNewNode(key Key, after *node) error {
	return t.recordKeyChange(key, after, true /* newNode */)
}

// Records that an existing node has been changed.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) recordNodeChange(key Key, after *node) error {
	return t.recordKeyChange(key, after, false /* newNode */)
}

// Records that the node associated with the given key has been deleted.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) recordNodeDeleted(key Key, after *node) error {
	// don't delete the root.
	if key.length == 0 {
		return t.recordKeyChange(key, after, false /* newNode */)
	}
	return t.recordKeyChange(key, nil, false /* newNode */)
}

// Records that the node associated with the given key has been changed.
// If it is an existing node, record what its value was before it was changed.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) recordKeyChange(key Key, after *node, newNode bool) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	if existing, ok := t.changes.nodes[key]; ok {
		existing.after = after
		return nil
	}

	if newNode {
		t.changes.nodes[key] = &change[*node]{
			after: after,
		}
		return nil
	}

	val, err := t.getParentTrie().getValue(key)
	if err != nil {
		return err
	}

	before, err := t.getParentTrie().getEditableNode(key, val.HasValue())
	if err != nil && err != database.ErrNotFound {
		return err
	}
	t.changes.nodes[key] = &change[*node]{
		before: before,
		after:  after,
	}
	return nil
}

// Records that a key's value has been added or updated.
// Doesn't actually change the trie data structure.
// That's deferred until we call [calculateNodeIDs].
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) recordValueChange(key Key, value maybe.Maybe[[]byte]) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	// update the existing change if it exists
	if existing, ok := t.changes.values[key]; ok {
		existing.after = value
		return nil
	}

	// grab the before value
	before, err := t.getParentTrie().getValue(key)
	if err != nil {
		return err
	}
	t.changes.values[key] = &change[maybe.Maybe[[]byte]]{
		before: before,
		after:  value,
	}
	return nil
}

// Retrieves a node with the given [key].
// If the node is fetched from [t.parentTrie] and [id] isn't empty,
// sets the node's ID to [id].
// If the node is loaded from the baseDB, [hasValue] determines which database the node is stored in.
// Returns database.ErrNotFound if the node doesn't exist.
func (t *trieView) getNode(key Key, hasValue bool) (*node, error) {
	// check for the key within the changed nodes
	if nodeChange, isChanged := t.changes.nodes[key]; isChanged {
		t.db.metrics.ViewNodeCacheHit()
		if nodeChange.after == nil {
			return nil, database.ErrNotFound
		}
		return nodeChange.after, nil
	}

	// get the node from the parent trie and store a local copy
	parentTrieNode, err := t.getParentTrie().getEditableNode(key, hasValue)
	if err != nil {
		return nil, err
	}

	return parentTrieNode, nil
}

// Get the parent trie of the view
func (t *trieView) getParentTrie() TrieView {
	t.validityTrackingLock.RLock()
	defer t.validityTrackingLock.RUnlock()
	return t.parentTrie
}
