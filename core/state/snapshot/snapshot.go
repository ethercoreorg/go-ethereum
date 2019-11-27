// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package snapshot implements a journalled, dynamic state dump.
package snapshot

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/trie"
)

var (
	snapshotCleanHitMeter   = metrics.NewRegisteredMeter("state/snapshot/clean/hit", nil)
	snapshotCleanMissMeter  = metrics.NewRegisteredMeter("state/snapshot/clean/miss", nil)
	snapshotCleanReadMeter  = metrics.NewRegisteredMeter("state/snapshot/clean/read", nil)
	snapshotCleanWriteMeter = metrics.NewRegisteredMeter("state/snapshot/clean/write", nil)

	// ErrSnapshotStale is returned from data accessors if the underlying snapshot
	// layer had been invalidated due to the chain progressing forward far enough
	// to not maintain the layer's original state.
	ErrSnapshotStale = errors.New("snapshot stale")

	// ErrNotCoveredYet is returned from data accessors if the underlying snapshot
	// is being generated currently and the requested data item is not yet in the
	// range of accounts covered.
	ErrNotCoveredYet = errors.New("not covered yet")

	// errSnapshotCycle is returned if a snapshot is attempted to be inserted
	// that forms a cycle in the snapshot tree.
	errSnapshotCycle = errors.New("snapshot cycle")
)

// Snapshot represents the functionality supported by a snapshot storage layer.
type Snapshot interface {
	// Root returns the root hash for which this snapshot was made.
	Root() common.Hash

	// Account directly retrieves the account associated with a particular hash in
	// the snapshot slim data format.
	Account(hash common.Hash) (*Account, error)

	// AccountRLP directly retrieves the account RLP associated with a particular
	// hash in the snapshot slim data format.
	AccountRLP(hash common.Hash) ([]byte, error)

	// Storage directly retrieves the storage data associated with a particular hash,
	// within a particular account.
	Storage(accountHash, storageHash common.Hash) ([]byte, error)
}

// snapshot is the internal version of the snapshot data layer that supports some
// additional methods compared to the public API.
type snapshot interface {
	Snapshot

	// Update creates a new layer on top of the existing snapshot diff tree with
	// the specified data items. Note, the maps are retained by the method to avoid
	// copying everything.
	Update(blockRoot common.Hash, accounts map[common.Hash][]byte, storage map[common.Hash]map[common.Hash][]byte) *diffLayer

	// journal commits an entire diff hierarchy to disk into a single journal file.
	// This is meant to be used during shutdown to persist the snapshot without
	// flattening everything down (bad for reorgs).
	journal(path string) (io.WriteCloser, common.Hash, error)

	// Stale return whether this layer has become stale (was flattened across) or
	// if it's still live.
	Stale() bool
}

// SnapshotTree is an Ethereum state snapshot tree. It consists of one persistent
// base layer backed by a key-value store, on top of which arbitrarily many in-
// memory diff layers are topped. The memory diffs can form a tree with branching,
// but the disk layer is singleton and common to all. If a reorg goes deeper than
// the disk layer, everything needs to be deleted.
//
// The goal of a state snapshot is twofold: to allow direct access to account and
// storage data to avoid expensive multi-level trie lookups; and to allow sorted,
// cheap iteration of the account/storage tries for sync aid.
type Tree struct {
	layers map[common.Hash]snapshot // Collection of all known layers // TODO(karalabe): split Clique overlaps
	lock   sync.RWMutex
}

// New attempts to load an already existing snapshot from a persistent key-value
// store (with a number of memory layers from a journal), ensuring that the head
// of the snapshot matches the expected one.
//
// If the snapshot is missing or inconsistent, the entirety is deleted and will
// be reconstructed from scratch based on the tries in the key-value store.
func New(diskdb ethdb.KeyValueStore, triedb *trie.Database, journal string, root common.Hash) (*Tree, error) {
	// Attempt to load a previously persisted snapshot
	head, err := loadSnapshot(diskdb, triedb, journal, root)
	if err != nil {
		log.Warn("Failed to load snapshot, regenerating", "err", err)
		if head, err = generateSnapshot(diskdb, triedb, root); err != nil {
			return nil, err
		}
	}
	// Existing snapshot loaded or one regenerated, seed all the layers
	snap := &Tree{
		layers: make(map[common.Hash]snapshot),
	}
	for head != nil {
		snap.layers[head.Root()] = head

		switch self := head.(type) {
		case *diffLayer:
			head = self.parent
		case *diskLayer:
			head = nil
		default:
			panic(fmt.Sprintf("unknown data layer: %T", self))
		}
	}
	return snap, nil
}

// Snapshot retrieves a snapshot belonging to the given block root, or nil if no
// snapshot is maintained for that block.
func (t *Tree) Snapshot(blockRoot common.Hash) Snapshot {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.layers[blockRoot]
}

// Update adds a new snapshot into the tree, if that can be linked to an existing
// old parent. It is disallowed to insert a disk layer (the origin of all).
func (t *Tree) Update(blockRoot common.Hash, parentRoot common.Hash, accounts map[common.Hash][]byte, storage map[common.Hash]map[common.Hash][]byte) error {
	// Reject noop updates to avoid self-loops in the snapshot tree. This is a
	// special case that can only happen for Clique networks where empty blocks
	// don't modify the state (0 block subsidy).
	//
	// Although we could silently ignore this internally, it should be the caller's
	// responsibility to avoid even attempting to insert such a snapshot.
	if blockRoot == parentRoot {
		return errSnapshotCycle
	}
	// Generate a new snapshot on top of the parent
	parent := t.Snapshot(parentRoot).(snapshot)
	if parent == nil {
		return fmt.Errorf("parent [%#x] snapshot missing", parentRoot)
	}
	snap := parent.Update(blockRoot, accounts, storage)

	// Save the new snapshot for later
	t.lock.Lock()
	defer t.lock.Unlock()

	t.layers[snap.root] = snap
	return nil
}

// Cap traverses downwards the snapshot tree from a head block hash until the
// number of allowed layers are crossed. All layers beyond the permitted number
// are flattened downwards.
func (t *Tree) Cap(root common.Hash, layers int, memory uint64) error {
	// Retrieve the head snapshot to cap from
	snap := t.Snapshot(root)
	if snap == nil {
		return fmt.Errorf("snapshot [%#x] missing", root)
	}
	diff, ok := snap.(*diffLayer)
	if !ok {
		return fmt.Errorf("snapshot [%#x] is disk layer", root)
	}
	// Run the internal capping and discard all stale layers
	t.lock.Lock()
	defer t.lock.Unlock()

	// Flattening the bottom-most diff layer requires special casing since there's
	// no child to rewire to the grandparent. In that case we can fake a temporary
	// child for the capping and then remove it.
	switch layers {
	case 0:
		// If full commit was requested, flatten the diffs and merge onto disk
		diff.lock.RLock()
		base := diffToDisk(diff.flatten().(*diffLayer))
		diff.lock.RUnlock()

		// Replace the entire snapshot tree with the flat base
		t.layers = map[common.Hash]snapshot{base.root: base}
		return nil

	case 1:
		// If full flattening was requested, flatten the diffs but only merge if the
		// memory limit was reached
		var (
			bottom *diffLayer
			base   *diskLayer
		)
		diff.lock.RLock()
		bottom = diff.flatten().(*diffLayer)
		if bottom.memory >= memory {
			base = diffToDisk(bottom)
		}
		diff.lock.RUnlock()

		// If all diff layers were removed, replace the entire snapshot tree
		if base != nil {
			t.layers = map[common.Hash]snapshot{base.root: base}
			return nil
		}
		// Merge the new aggregated layer into the snapshot tree, clean stales below
		t.layers[bottom.root] = bottom

	default:
		// Many layers requested to be retained, cap normally
		t.cap(diff, layers, memory)
	}
	// Remove any layer that is stale or links into a stale layer
	children := make(map[common.Hash][]common.Hash)
	for root, snap := range t.layers {
		if diff, ok := snap.(*diffLayer); ok {
			parent := diff.parent.Root()
			children[parent] = append(children[parent], root)
		}
	}
	var remove func(root common.Hash)
	remove = func(root common.Hash) {
		delete(t.layers, root)
		for _, child := range children[root] {
			remove(child)
		}
		delete(children, root)
	}
	for root, snap := range t.layers {
		if snap.Stale() {
			remove(root)
		}
	}
	return nil
}

// cap traverses downwards the diff tree until the number of allowed layers are
// crossed. All diffs beyond the permitted number are flattened downwards. If the
// layer limit is reached, memory cap is also enforced (but not before).
func (t *Tree) cap(diff *diffLayer, layers int, memory uint64) {
	// Dive until we run out of layers or reach the persistent database
	for ; layers > 2; layers-- {
		// If we still have diff layers below, continue down
		if parent, ok := diff.parent.(*diffLayer); ok {
			diff = parent
		} else {
			// Diff stack too shallow, return without modifications
			return
		}
	}
	// We're out of layers, flatten anything below, stopping if it's the disk or if
	// the memory limit is not yet exceeded.
	switch parent := diff.parent.(type) {
	case *diskLayer:
		return

	case *diffLayer:
		// Flatten the parent into the grandparent. The flattening internally obtains a
		// write lock on grandparent.
		flattened := parent.flatten().(*diffLayer)
		t.layers[flattened.root] = flattened

		diff.lock.Lock()
		defer diff.lock.Unlock()

		diff.parent = flattened
		if flattened.memory < memory {
			// Accumulator layer is smaller than the limit, so we can abort, unless
			// there's a snapshot being generated currently. In that case, the trie
			// will move fron underneath the generator so we **must** merge all the
			// partial data down into the snapshot and restart the generation.
			if flattened.parent.(*diskLayer).genAbort == nil {
				return
			}
		}
	default:
		panic(fmt.Sprintf("unknown data layer: %T", parent))
	}
	// If the bottom-most layer is larger than our memory cap, persist to disk
	bottom := diff.parent.(*diffLayer)

	bottom.lock.RLock()
	base := diffToDisk(bottom)
	bottom.lock.RUnlock()

	t.layers[base.root] = base
	diff.parent = base
}

// diffToDisk merges a bottom-most diff into the persistent disk layer underneath
// it. The method will panic if called onto a non-bottom-most diff layer.
func diffToDisk(bottom *diffLayer) *diskLayer {
	var (
		base  = bottom.parent.(*diskLayer)
		batch = base.diskdb.NewBatch()
		stats *generatorStats
	)
	// If the disk layer is running a snapshot generator, abort it
	if base.genAbort != nil {
		abort := make(chan *generatorStats)
		base.genAbort <- abort
		stats = <-abort
	}
	// Start by temporarily deleting the current snapshot block marker. This
	// ensures that in the case of a crash, the entire snapshot is invalidated.
	rawdb.DeleteSnapshotRoot(batch)

	// Mark the original base as stale as we're going to create a new wrapper
	base.lock.Lock()
	if base.stale {
		panic("parent disk layer is stale") // we've committed into the same base from two children, boo
	}
	base.stale = true
	base.lock.Unlock()

	// Push all the accounts into the database
	for hash, data := range bottom.accountData {
		// Skip any account not covered yet by the snapshot
		if base.genMarker != nil && bytes.Compare(hash[:], base.genMarker) > 0 {
			continue
		}
		if len(data) > 0 {
			// Account was updated, push to disk
			rawdb.WriteAccountSnapshot(batch, hash, data)
			base.cache.Set(hash[:], data)

			if batch.ValueSize() > ethdb.IdealBatchSize {
				if err := batch.Write(); err != nil {
					log.Crit("Failed to write account snapshot", "err", err)
				}
				batch.Reset()
			}
		} else {
			// Account was deleted, remove all storage slots too
			rawdb.DeleteAccountSnapshot(batch, hash)
			base.cache.Set(hash[:], nil)

			it := rawdb.IterateStorageSnapshots(base.diskdb, hash)
			for it.Next() {
				if key := it.Key(); len(key) == 65 { // TODO(karalabe): Yuck, we should move this into the iterator
					batch.Delete(key)
					base.cache.Del(key[1:])
				}
			}
			it.Release()
		}
	}
	// Push all the storage slots into the database
	for accountHash, storage := range bottom.storageData {
		// Skip any account not covered yet by the snapshot
		if base.genMarker != nil && bytes.Compare(accountHash[:], base.genMarker) > 0 {
			continue
		}
		// Generation might be mid-account, track that case too
		midAccount := base.genMarker != nil && bytes.Equal(accountHash[:], base.genMarker[:common.HashLength])

		for storageHash, data := range storage {
			// Skip any slot not covered yet by the snapshot
			if midAccount && bytes.Compare(storageHash[:], base.genMarker[common.HashLength:]) > 0 {
				continue
			}
			if len(data) > 0 {
				rawdb.WriteStorageSnapshot(batch, accountHash, storageHash, data)
				base.cache.Set(append(accountHash[:], storageHash[:]...), data)
			} else {
				rawdb.DeleteStorageSnapshot(batch, accountHash, storageHash)
				base.cache.Set(append(accountHash[:], storageHash[:]...), nil)
			}
		}
		if batch.ValueSize() > ethdb.IdealBatchSize {
			if err := batch.Write(); err != nil {
				log.Crit("Failed to write storage snapshot", "err", err)
			}
			batch.Reset()
		}
	}
	// Update the snapshot block marker and write any remainder data
	rawdb.WriteSnapshotRoot(batch, bottom.root)
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write leftover snapshot", "err", err)
	}
	res := &diskLayer{
		root:      bottom.root,
		cache:     base.cache,
		diskdb:    base.diskdb,
		triedb:    base.triedb,
		genMarker: base.genMarker,
	}
	// If snapshot generation hasn't finished yet, port over all the starts and
	// continue where the previous round left off.
	//
	// Note, the `base.genAbort` comparison is not used normally, it's checked
	// to allow the tests to play with the marker without triggering this path.
	if base.genMarker != nil && base.genAbort != nil {
		res.genMarker = base.genMarker
		res.genAbort = make(chan chan *generatorStats)
		go res.generate(stats)
	}
	return res
}

// Journal commits an entire diff hierarchy to disk into a single journal file.
// This is meant to be used during shutdown to persist the snapshot without
// flattening everything down (bad for reorgs).
//
// The method returns the root hash of the base layer that needs to be persisted
// to disk as a trie too to allow continuing any pending generation op.
func (t *Tree) Journal(root common.Hash, path string) (common.Hash, error) {
	// Retrieve the head snapshot to journal from var snap snapshot
	snap := t.Snapshot(root)
	if snap == nil {
		return common.Hash{}, fmt.Errorf("snapshot [%#x] missing", root)
	}
	// Run the journaling
	t.lock.Lock()
	defer t.lock.Unlock()

	writer, base, err := snap.(snapshot).journal(path)
	if err != nil {
		return common.Hash{}, err
	}
	return base, writer.Close()
}
