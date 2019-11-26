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
	"bytes"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

// wipeSnapshot iterates over the entire key-value database and deletes all the
// data associated with the snapshot (accounts, storage, metadata). After all is
// done, the snapshot range of the database is compacted to free up unused data
// blocks.
func wipeSnapshot(db ethdb.KeyValueStore) error {
	// Iterate over the snapshot key-range and delete all of them
	log.Info("Deleting previous snapshot account leftovers")
	if err := wipeKeyRange(db, "accounts", rawdb.SnapshotAccountPrefix, len(rawdb.SnapshotAccountPrefix)+common.HashLength); err != nil {
		return err
	}
	log.Info("Deleting previous snapshot storage leftovers")
	if err := wipeKeyRange(db, "storage", rawdb.SnapshotStoragePrefix, len(rawdb.SnapshotStoragePrefix)+2*common.HashLength); err != nil {
		return err
	}
	rawdb.DeleteSnapshotRoot(db)

	// Compact the snapshot section of the database to get rid of unused space
	log.Info("Compacting snapshot area in database")
	start := time.Now()

	end := common.CopyBytes(rawdb.SnapshotAccountPrefix)
	end[len(end)-1]++

	if err := db.Compact(rawdb.SnapshotAccountPrefix, end); err != nil {
		return err
	}
	end = common.CopyBytes(rawdb.SnapshotStoragePrefix)
	end[len(end)-1]++

	if err := db.Compact(rawdb.SnapshotStoragePrefix, end); err != nil {
		return err
	}
	log.Info("Compacted snapshot area in database", "elapsed", time.Since(start))

	return nil
}

// wipeKeyRange deletes a range of keys from the database starting with prefix
// and having a specific total key length.
func wipeKeyRange(db ethdb.KeyValueStore, kind string, prefix []byte, keylen int) error {
	// Batch deletions together to avoid holding an iterator for too long
	var (
		batch = db.NewBatch()
		items int
	)
	// Iterate over the key-range and delete all of them
	start, logged := time.Now(), time.Now()

	it := db.NewIteratorWithStart(prefix)
	for it.Next() {
		// Skip any keys with the correct prefix but wrong lenth (trie nodes)
		key := it.Key()
		if !bytes.HasPrefix(key, prefix) {
			break
		}
		if len(key) != keylen {
			continue
		}
		// Delete the key and periodically recreate the batch and iterator
		batch.Delete(key)
		items++

		if items%10000 == 0 {
			// Batch too large (or iterator too long lived, flush and recreate)
			it.Release()
			if err := batch.Write(); err != nil {
				return err
			}
			batch.Reset()
			it = db.NewIteratorWithStart(key)

			if time.Since(logged) > 8*time.Second {
				log.Info("Deleting snapshot entries", "kind", kind, "wiped", items, "elapsed", time.Since(start))
				logged = time.Now()
			}
		}
	}
	it.Release()
	return batch.Write()
}
