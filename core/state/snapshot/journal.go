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
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/trie"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
)

// journalMarker is a disk layer entry containing the generator progress marker.
// Ideally this entire thing would be a byte slice, but rlp can't differentiate
// between an empty slice and a nil slice, so we need to expand it.
type journalMarker struct {
	Done     bool
	Marker   []byte
	Accounts uint64
	Slots    uint64
	Storage  uint64
}

// journalAccount is an account entry in a diffLayer's disk journal.
type journalAccount struct {
	Hash common.Hash
	Blob []byte
}

// journalStorage is an account's storage map in a diffLayer's disk journal.
type journalStorage struct {
	Hash common.Hash
	Keys []common.Hash
	Vals [][]byte
}

// loadSnapshot loads a pre-existing state snapshot backed by a key-value store.
func loadSnapshot(diskdb ethdb.KeyValueStore, triedb *trie.Database, journal string, root common.Hash) (snapshot, error) {
	// Retrieve the block number and hash of the snapshot, failing if no snapshot
	// is present in the database (or crashed mid-update).
	baseRoot := rawdb.ReadSnapshotRoot(diskdb)
	if baseRoot == (common.Hash{}) {
		return nil, errors.New("missing or corrupted snapshot")
	}
	base := &diskLayer{
		diskdb: diskdb,
		triedb: triedb,
		cache:  fastcache.New(512 * 1024 * 1024),
		root:   baseRoot,
	}
	// Open the journal, it must exist since even for 0 layer it stores whether
	// we've already generated the snapshot or are in progress only
	file, err := os.Open(journal)
	if err != nil {
		return nil, err
	}
	r := rlp.NewStream(file, 0)

	// Read the snapshot generation progress for the disk layer
	var marker journalMarker
	if err := r.Decode(&marker); err != nil {
		return nil, fmt.Errorf("failed to load snapshot progress marker: %v", err)
	}
	if !marker.Done {
		base.genMarker = marker.Marker
		if base.genMarker == nil {
			base.genMarker = []byte{}
		}
		base.genAbort = make(chan chan *generatorStats)

		go base.generate(&generatorStats{
			origin:   binary.BigEndian.Uint64(marker.Marker),
			start:    time.Now(),
			accounts: marker.Accounts,
			slots:    marker.Slots,
			storage:  common.StorageSize(marker.Storage),
		})
	}
	// Load all the snapshot diffs from the journal
	snapshot, err := loadDiffLayer(base, r)
	if err != nil {
		return nil, err
	}
	// Entire snapshot journal loaded, sanity check the head and return
	// Journal doesn't exist, don't worry if it's not supposed to
	if head := snapshot.Root(); head != root {
		return nil, fmt.Errorf("head doesn't match snapshot: have %#x, want %#x", head, root)
	}
	return snapshot, nil
}

// loadDiffLayer reads the next sections of a snapshot journal, reconstructing a new
// diff and verifying that it can be linked to the requested parent.
func loadDiffLayer(parent snapshot, r *rlp.Stream) (snapshot, error) {
	// Read the next diff journal entry
	var root common.Hash
	if err := r.Decode(&root); err != nil {
		// The first read may fail with EOF, marking the end of the journal
		if err == io.EOF {
			return parent, nil
		}
		return nil, fmt.Errorf("load diff root: %v", err)
	}
	var accounts []journalAccount
	if err := r.Decode(&accounts); err != nil {
		return nil, fmt.Errorf("load diff accounts: %v", err)
	}
	accountData := make(map[common.Hash][]byte)
	for _, entry := range accounts {
		accountData[entry.Hash] = entry.Blob
	}
	var storage []journalStorage
	if err := r.Decode(&storage); err != nil {
		return nil, fmt.Errorf("load diff storage: %v", err)
	}
	storageData := make(map[common.Hash]map[common.Hash][]byte)
	for _, entry := range storage {
		slots := make(map[common.Hash][]byte)
		for i, key := range entry.Keys {
			slots[key] = entry.Vals[i]
		}
		storageData[entry.Hash] = slots
	}
	return loadDiffLayer(newDiffLayer(parent, root, accountData, storageData), r)
}

// journal is the internal version of Journal that also returns the journal file
// so subsequent layers know where to write to.
func (dl *diskLayer) journal(path string) (io.WriteCloser, common.Hash, error) {
	// If the snapshot is currenty being generated, abort it
	stats := new(generatorStats)
	if dl.genAbort != nil {
		abort := make(chan *generatorStats)
		dl.genAbort <- abort

		if stats = <-abort; stats != nil {
			stats.Log("Journalling in-progress snapshot", dl.genMarker)
		}
	}
	// Ensure the layer didn't get stale
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	if dl.stale {
		return nil, common.Hash{}, ErrSnapshotStale
	}
	// We've reached the bottom, open the journal
	file, err := os.Create(path)
	if err != nil {
		return nil, common.Hash{}, err
	}
	// Write out the generator marker
	if err := rlp.Encode(file, journalMarker{
		Done:     dl.genMarker == nil,
		Marker:   dl.genMarker,
		Accounts: stats.accounts,
		Slots:    stats.slots,
		Storage:  uint64(stats.storage),
	}); err != nil {
		file.Close()
		return nil, common.Hash{}, err
	}
	return file, dl.root, nil
}

// journal is the internal version of Journal that also returns the journal file
// so subsequent layers know where to write to.
func (dl *diffLayer) journal(path string) (io.WriteCloser, common.Hash, error) {
	// Journal the parent first
	writer, base, err := dl.parent.journal(path)
	if err != nil {
		return nil, common.Hash{}, err
	}
	// Ensure the layer didn't get stale
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	if dl.stale {
		writer.Close()
		return nil, common.Hash{}, ErrSnapshotStale
	}
	// Everything below was journalled, persist this layer too
	buf := bufio.NewWriter(writer)
	if err := rlp.Encode(buf, dl.root); err != nil {
		buf.Flush()
		writer.Close()
		return nil, common.Hash{}, err
	}
	accounts := make([]journalAccount, 0, len(dl.accountData))
	for hash, blob := range dl.accountData {
		accounts = append(accounts, journalAccount{Hash: hash, Blob: blob})
	}
	if err := rlp.Encode(buf, accounts); err != nil {
		buf.Flush()
		writer.Close()
		return nil, common.Hash{}, err
	}
	storage := make([]journalStorage, 0, len(dl.storageData))
	for hash, slots := range dl.storageData {
		keys := make([]common.Hash, 0, len(slots))
		vals := make([][]byte, 0, len(slots))
		for key, val := range slots {
			keys = append(keys, key)
			vals = append(vals, val)
		}
		storage = append(storage, journalStorage{Hash: hash, Keys: keys, Vals: vals})
	}
	if err := rlp.Encode(buf, storage); err != nil {
		buf.Flush()
		writer.Close()
		return nil, common.Hash{}, err
	}
	buf.Flush()
	return writer, base, nil
}
