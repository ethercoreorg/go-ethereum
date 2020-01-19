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

package trie

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	"golang.org/x/crypto/sha3"
)

// LeafChanSize is the size of the leafCh. It's a pretty arbitrary number, to allow
// some paralellism but not incur too much memory overhead.
const LeafChanSize = 200

// Leaf represents a trie leaf value
type Leaf struct {
	size   int         // size of the rlp data (estimate)
	hash   common.Hash // hash of rlp data
	node   node        // the node to commit
	vnodes bool        // set to true if the node (possibly) contains a valueNode
}

// committer is a type used for the trie Commit operation. A committer has some
// internal preallocated temp space, and also a callback that is invoked when
// leaves are committed. The leafs are passed through the `leafCh`,  to allow
// some level of paralellism.
// By 'some level' of parallelism, it's still the case that all leaves will be
// processed sequentially - onleaf will never be called in parallel or out of order.
type committer struct {
	tmp sliceBuffer
	sha keccakState

	onleaf LeafCallback
	leafCh chan *Leaf
}

// committers live in a global db.
var committerPool = sync.Pool{
	New: func() interface{} {
		return &committer{
			tmp: make(sliceBuffer, 0, 550), // cap is as large as a full fullNode.
			sha: sha3.NewLegacyKeccak256().(keccakState),
		}
	},
}

// newCommitter creates a new committer or picks one from the pool, and
// initializes the leafCh, if needed.
// In case no onleaf-callback is provided, the committer does not
// use a channel-based commit, but inlined.
// Typically, the account trie is committed with a channel-based leaf-commit,
// whereas storage tries are committed 'inline'.
func newCommitter(onleaf LeafCallback) *committer {
	h := committerPool.Get().(*committer)
	h.onleaf = onleaf
	if onleaf != nil {
		h.leafCh = make(chan *Leaf, LeafChanSize)
	}
	return h
}

func returnCommitterToPool(h *committer) {
	h.onleaf = nil
	h.leafCh = nil
	committerPool.Put(h)
}

// commitNeeded returns 'false' if the given node is already in sync with db
func (h *committer) commitNeeded(n node) bool {
	hash, dirty := n.cache()
	return hash == nil || dirty
}

// hash collapses a node down into a hash node, also returning a copy of the
// original node initialized with the computed hash to replace the original one.
func (h *committer) commit(n node, db *Database, force bool) (node, error) {
	// If we're not storing the node, just hashing, use available cached data
	hash, dirty := n.cache()
	if hash != nil && !dirty {
		return hash, nil
	}
	if db == nil {
		return nil, errors.New("no db provided")
	}
	// Commit children. then parent
	// Remove the dirty flag.
	switch cn := n.(type) {
	case *shortNode:
		// Commit child
		collapsed := cn.copy()
		if _, ok := cn.Val.(valueNode); !ok {
			if childV, err := h.commit(cn.Val, db, false); err != nil {
				return nil, err
			} else {
				collapsed.Val = childV
			}
		}
		// The key needs to be copied, since we're delivering it to database
		collapsed.Key = hexToCompact(cn.Key)
		hashedNode := h.store(collapsed, db, force, true)
		if hn, ok := hashedNode.(hashNode); ok {
			cn.flags.dirty = false
			return hn, nil
		} else {
			return collapsed, nil
		}
	case *fullNode:
		hashedKids, hasVnodes, err := h.commitChildren(cn, db, force)
		if err != nil {
			return nil, err
		}
		collapsed := cn.copy()
		collapsed.Children = hashedKids

		hashedNode := h.store(collapsed, db, force, hasVnodes)
		if hn, ok := hashedNode.(hashNode); ok {
			cn.flags.dirty = false
			return hn, nil
		} else {
			return collapsed, nil
		}
	case valueNode:
		return h.store(cn, db, force, false), nil
	// hashnodes aren't stored
	case hashNode:
		return cn, nil
	}
	return hash, nil
}

// commitChildren commits the children of the given fullnode
func (h *committer) commitChildren(n *fullNode, db *Database, force bool) ([17]node, bool, error) {
	var children [17]node
	var hasValueNodeChildren = false
	for i, child := range n.Children {
		if child == nil {
			continue
		}
		hnode, err := h.commit(child, db, false)
		if err != nil {
			return children, false, err
		}
		children[i] = hnode
		if _, ok := hnode.(valueNode); ok {
			hasValueNodeChildren = true
		}
	}
	return children, hasValueNodeChildren, nil
}

// store hashes the node n and if we have a storage layer specified, it writes
// the key/value pair to it and tracks any node->child references as well as any
// node->external trie references.
func (h *committer) store(n node, db *Database, force bool, hasVnodeChildren bool) node {
	// Larger nodes are replaced by their hash and stored in the database.
	var (
		hash, _ = n.cache()
		size    int
	)
	if hash == nil {
		if vn, ok := n.(valueNode); ok {
			h.tmp.Reset()
			if err := rlp.Encode(&h.tmp, vn); err != nil {
				panic("encode error: " + err.Error())
			}
			size = len(h.tmp)
			if size < 32 && !force {
				return n // Nodes smaller than 32 bytes are stored inside their parent
			}
			hash = h.makeHashNode(h.tmp)
		} else {
			// This was not generated - must be a small node stored in the parent
			// No need to do anything here
			return n
		}
	} else {
		// We have the hash already, estimate the RLP encoding-size of the node.
		// The size is used for mem tracking, does not need to be exact
		size = estimateSize(n)
	}
	// If we're using channel-based leaf-reporting, send to channel.
	// The leaf channel will be active only when there an active leaf-callback
	if h.leafCh != nil {
		h.leafCh <- &Leaf{
			size:   size,
			hash:   common.BytesToHash(hash),
			node:   n,
			vnodes: hasVnodeChildren,
		}
	} else if db != nil {
		// No leaf-callback used, but there's still a database. Do serial
		// insertion
		db.lock.Lock()
		db.insert(common.BytesToHash(hash), size, n)
		db.lock.Unlock()
	}
	return hash
}

// commitLoop does the actual insert + leaf callback for nodes
func (h *committer) commitLoop(db *Database) {
	for item := range h.leafCh {
		var (
			hash      = item.hash
			size      = item.size
			n         = item.node
			hasVnodes = item.vnodes
		)
		// We are pooling the trie nodes into an intermediate memory cache
		db.lock.Lock()
		db.insert(hash, size, n)
		db.lock.Unlock()
		if h.onleaf != nil && hasVnodes {
			switch n := n.(type) {
			case *shortNode:
				if child, ok := n.Val.(valueNode); ok {
					h.onleaf(child, hash)
				}
			case *fullNode:
				for i := 0; i < 16; i++ {
					if child, ok := n.Children[i].(valueNode); ok {
						h.onleaf(child, hash)
					}
				}
			}
		}
	}
}

func (h *committer) makeHashNode(data []byte) hashNode {
	n := make(hashNode, h.sha.Size())
	h.sha.Reset()
	h.sha.Write(data)
	h.sha.Read(n)
	return n
}

// estimateSize estimates the size of an rlp-encoded node, without actually
// rlp-encoding it (zero allocs). This method has been experimentally tried, and with a trie
// with 1000 leafs, the only errors above 1% are on small shortnodes, where this
// method overestimates by 2 or 3 bytes (e.g. 37 instead of 35)
func estimateSize(n node) int {
	switch n := n.(type) {
	case *shortNode:
		// A short node contains a compacted key, and a value.
		return 3 + len(n.Key) + estimateSize(n.Val)
	case *fullNode:
		// A full node contains up to 16 hashes (some nils), and a key
		s := 3
		for i := 0; i < 16; i++ {
			if child := n.Children[i]; child != nil {
				s += estimateSize(child)
			} else {
				s += 1
			}
		}
		return s
	case valueNode:
		return 1 + len(n)
	case hashNode:
		return 1 + len(n)
	default:
		panic(fmt.Sprintf("node type %T", n))

	}
}
