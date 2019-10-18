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

package snapshot

import (
	"testing"
	"time"

	"github.com/allegro/bigcache"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
)

// Tests that if a disk layer becomes stale, no active external references will
// be returned with junk data. This version of the test flattens every diff layer
// to check internal corner case around the bottom-most memory accumulator.
func TestDiskLayerExternalInvalidationFullFlatten(t *testing.T) {
	// Create an empty base layer and a snapshot tree out of it
	cache, _ := bigcache.NewBigCache(bigcache.DefaultConfig(time.Minute))
	base := &diskLayer{
		db:    rawdb.NewMemoryDatabase(),
		root:  common.HexToHash("0x01"),
		cache: cache,
	}
	snaps := &SnapshotTree{
		layers: map[common.Hash]snapshot{
			base.root: base,
		},
	}
	// Retrieve a reference to the base and commit a diff on top
	ref := snaps.Snapshot(base.root)

	accounts := map[common.Hash][]byte{
		common.HexToHash("0xa1"): randomAccount(),
	}
	storage := make(map[common.Hash]map[common.Hash][]byte)
	if err := snaps.Update(common.HexToHash("0x02"), common.HexToHash("0x01"), accounts, storage); err != nil {
		t.Fatalf("failed to create a diff layer: %v", err)
	}
	// Commit the diff layer onto the disk and ensure it's persisted
	if err := snaps.Cap(common.HexToHash("0x02"), 1, 0); err != nil {
		t.Fatalf("failed to merge diff layer onto disk: %v", err)
	}
	// Since the base layer was modified, ensure that data retrievald on the external reference fail
	if acc, err := ref.Account(common.HexToHash("0x01")); err != ErrSnapshotStale {
		t.Errorf("stale reference returned account: %#x (err: %v)", acc, err)
	}
	if slot, err := ref.Storage(common.HexToHash("0xa1"), common.HexToHash("0xb1")); err != ErrSnapshotStale {
		t.Errorf("stale reference returned storage slot: %#x (err: %v)", slot, err)
	}
}

// Tests that if a disk layer becomes stale, no active external references will
// be returned with junk data. This version of the test retains the bottom diff
// layer to check the usual mode of operation where the accumulator is retained.
func TestDiskLayerExternalInvalidationPartialFlatten(t *testing.T) {
	// Create an empty base layer and a snapshot tree out of it
	cache, _ := bigcache.NewBigCache(bigcache.DefaultConfig(time.Minute))
	base := &diskLayer{
		db:    rawdb.NewMemoryDatabase(),
		root:  common.HexToHash("0x01"),
		cache: cache,
	}
	snaps := &SnapshotTree{
		layers: map[common.Hash]snapshot{
			base.root: base,
		},
	}
	// Retrieve a reference to the base and commit two diffs on top
	ref := snaps.Snapshot(base.root)

	accounts := map[common.Hash][]byte{
		common.HexToHash("0xa1"): randomAccount(),
	}
	storage := make(map[common.Hash]map[common.Hash][]byte)
	if err := snaps.Update(common.HexToHash("0x02"), common.HexToHash("0x01"), accounts, storage); err != nil {
		t.Fatalf("failed to create a diff layer: %v", err)
	}
	if err := snaps.Update(common.HexToHash("0x03"), common.HexToHash("0x02"), accounts, storage); err != nil {
		t.Fatalf("failed to create a diff layer: %v", err)
	}
	// Commit the diff layer onto the disk and ensure it's persisted
	if err := snaps.Cap(common.HexToHash("0x03"), 2, 0); err != nil {
		t.Fatalf("failed to merge diff layer onto disk: %v", err)
	}
	// Since the base layer was modified, ensure that data retrievald on the external reference fail
	if acc, err := ref.Account(common.HexToHash("0x01")); err != ErrSnapshotStale {
		t.Errorf("stale reference returned account: %#x (err: %v)", acc, err)
	}
	if slot, err := ref.Storage(common.HexToHash("0xa1"), common.HexToHash("0xb1")); err != ErrSnapshotStale {
		t.Errorf("stale reference returned storage slot: %#x (err: %v)", slot, err)
	}
}

// Tests that if a diff layer becomes stale, no active external references will
// be returned with junk data. This version of the test flattens every diff layer
// to check internal corner case around the bottom-most memory accumulator.
func TestDiffLayerExternalInvalidationFullFlatten(t *testing.T) {
	// Create an empty base layer and a snapshot tree out of it
	cache, _ := bigcache.NewBigCache(bigcache.DefaultConfig(time.Minute))
	base := &diskLayer{
		db:    rawdb.NewMemoryDatabase(),
		root:  common.HexToHash("0x01"),
		cache: cache,
	}
	snaps := &SnapshotTree{
		layers: map[common.Hash]snapshot{
			base.root: base,
		},
	}
	// Commit two diffs on top and retrieve a reference to the bottommost
	accounts := map[common.Hash][]byte{
		common.HexToHash("0xa1"): randomAccount(),
	}
	storage := make(map[common.Hash]map[common.Hash][]byte)
	if err := snaps.Update(common.HexToHash("0x02"), common.HexToHash("0x01"), accounts, storage); err != nil {
		t.Fatalf("failed to create a diff layer: %v", err)
	}
	if err := snaps.Update(common.HexToHash("0x03"), common.HexToHash("0x02"), accounts, storage); err != nil {
		t.Fatalf("failed to create a diff layer: %v", err)
	}
	ref := snaps.Snapshot(common.HexToHash("0x02"))

	// Flatten the diff layer into the bottom accumulator
	if err := snaps.Cap(common.HexToHash("0x03"), 2, 1024*1024); err != nil {
		t.Fatalf("failed to flatten diff layer into accumulator: %v", err)
	}
	// Since the accumulator diff layer was modified, ensure that data retrievald on the external reference fail
	if acc, err := ref.Account(common.HexToHash("0x01")); err != ErrSnapshotStale {
		t.Errorf("stale reference returned account: %#x (err: %v)", acc, err)
	}
	if slot, err := ref.Storage(common.HexToHash("0xa1"), common.HexToHash("0xb1")); err != ErrSnapshotStale {
		t.Errorf("stale reference returned storage slot: %#x (err: %v)", slot, err)
	}
}

// Tests that if a diff layer becomes stale, no active external references will
// be returned with junk data. This version of the test retains the bottom diff
// layer to check the usual mode of operation where the accumulator is retained.
func TestDiffLayerExternalInvalidationPartialFlatten(t *testing.T) {
	// Create an empty base layer and a snapshot tree out of it
	cache, _ := bigcache.NewBigCache(bigcache.DefaultConfig(time.Minute))
	base := &diskLayer{
		db:    rawdb.NewMemoryDatabase(),
		root:  common.HexToHash("0x01"),
		cache: cache,
	}
	snaps := &SnapshotTree{
		layers: map[common.Hash]snapshot{
			base.root: base,
		},
	}
	// Commit three diffs on top and retrieve a reference to the bottommost
	accounts := map[common.Hash][]byte{
		common.HexToHash("0xa1"): randomAccount(),
	}
	storage := make(map[common.Hash]map[common.Hash][]byte)
	if err := snaps.Update(common.HexToHash("0x02"), common.HexToHash("0x01"), accounts, storage); err != nil {
		t.Fatalf("failed to create a diff layer: %v", err)
	}
	if err := snaps.Update(common.HexToHash("0x03"), common.HexToHash("0x02"), accounts, storage); err != nil {
		t.Fatalf("failed to create a diff layer: %v", err)
	}
	if err := snaps.Update(common.HexToHash("0x04"), common.HexToHash("0x03"), accounts, storage); err != nil {
		t.Fatalf("failed to create a diff layer: %v", err)
	}
	ref := snaps.Snapshot(common.HexToHash("0x02"))

	// Flatten the diff layer into the bottom accumulator
	if err := snaps.Cap(common.HexToHash("0x04"), 2, 1024*1024); err != nil {
		t.Fatalf("failed to flatten diff layer into accumulator: %v", err)
	}
	// Since the accumulator diff layer was modified, ensure that data retrievald on the external reference fail
	if acc, err := ref.Account(common.HexToHash("0x01")); err != ErrSnapshotStale {
		t.Errorf("stale reference returned account: %#x (err: %v)", acc, err)
	}
	if slot, err := ref.Storage(common.HexToHash("0xa1"), common.HexToHash("0xb1")); err != ErrSnapshotStale {
		t.Errorf("stale reference returned storage slot: %#x (err: %v)", slot, err)
	}
}
