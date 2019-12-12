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
	"container/heap"

	"github.com/ethereum/go-ethereum/common"
)

// iteratorHeap is a set of iterators implementing the heap.Interface.
type iteratorHeap []*weightedAccountIterator

// Len implements sort.Interface, returning the number of active iterators.
func (ih iteratorHeap) Len() int { return len(ih) }

// Less implements sort.Interface, returning which of two iterators in the stack
// is before the other.
func (ih iteratorHeap) Less(i, j int) bool {
	// Order the iterators primarilly by the account hashes
	hashI := ih[i].it.Hash()
	hashJ := ih[j].it.Hash()

	switch bytes.Compare(hashI[:], hashJ[:]) {
	case -1:
		return true
	case 1:
		return false
	}
	// Same account in multiple layers, split by priority
	return ih[i].priority < ih[j].priority
}

// Swap implements sort.Interface, swapping two entries in the iterator stack.
func (ih iteratorHeap) Swap(i, j int) {
	ih[i], ih[j] = ih[j], ih[i]
}

// Push implements heap.Interface, appending a new item to the end of the heap.
func (ih *iteratorHeap) Push(x interface{}) {
	*ih = append(*ih, x.(*weightedAccountIterator))
}

// Pop implements heap.Interface, removing an item from the end of the heap.
func (ih *iteratorHeap) Pop() interface{} {
	x := (*ih)[len(*ih)-1]
	*ih = (*ih)[:len(*ih)-1]
	return x
}

// heapAccountIterator is a more optimized multi-layer iterator which maintains a
// direct mapping of all iterators leading down to the bottom layer.
type heapAccountIterator struct {
	iterators *iteratorHeap
	initiated bool
	fail      error
}

// newHeapAccountIterator creates a new hierarhical account iterator with one
// element per diff layer. The returned combo iterator can be used to walk over
// the entire snapshot diff stack simultaneously.
func newHeapAccountIterator(snap snapshot) AccountIterator {
	hi := &heapAccountIterator{
		iterators: new(iteratorHeap),
	}
	for depth := 0; snap != nil; depth++ {
		*hi.iterators = append(*hi.iterators, &weightedAccountIterator{
			it:       snap.AccountIterator(),
			priority: depth,
		})
		snap = snap.Parent()
	}
	hi.Seek(common.Hash{})
	return hi
}

// Seek steps the iterator forward as many elements as needed, so that after
// calling Next(), the iterator will be at a key higher than the given hash.
func (hi *heapAccountIterator) Seek(hash common.Hash) {
	for i := 0; i < len(*hi.iterators); i++ {
		// Position the next iterator in line, dropping it if is insta-exhausted
		it := (*hi.iterators)[i]
		if it.it.Seek(hash); !it.it.Next() {
			it.it.Release()
			last := len(*hi.iterators) - 1

			(*hi.iterators)[i] = (*hi.iterators)[last]
			(*hi.iterators)[last] = nil
			*hi.iterators = (*hi.iterators)[:last]

			i--
		}
	}
	// Re-sort the entire list
	heap.Init(hi.iterators)
	hi.initiated = false
}

// Next steps the iterator forward one element, returning false if exhausted.
func (hi *heapAccountIterator) Next() bool {
	// If the iterator is already exhausted or not yet initiated, short circuit
	if len(*hi.iterators) == 0 {
		return false
	}
	if !hi.initiated {
		// Don't forward first time, we had to 'Next' once in order to do the
		// heap initialization already
		hi.initiated = true
		return true
	}
	// It can happen that multiple layers contain the same account hash. In that
	// case, when advancing the first iterator, we need to skip along with the
	// other clashers too. Save the current hash to serve as a skip marker.
	head := (*hi.iterators)[0].it
	skip := head.Hash()

	// Keep advancing iterators until the system stabilizes or we run out
	for head.Hash() == skip {
		// Advance the first iterator and pop off if it's exhausted
		if !head.Next() {
			head.Release()
			heap.Remove(hi.iterators, 0)
		} else {
			heap.Fix(hi.iterators, 0)
		}
		if len(*hi.iterators) == 0 {
			return false
		}
		head = (*hi.iterators)[0].it
	}
	return true
}

// Error returns any failure that occurred during iteration, which might have
// caused a premature iteration exit (e.g. snapshot stack becoming stale).
func (hi *heapAccountIterator) Error() error {
	return hi.fail
}

// Hash returns the current key
func (hi *heapAccountIterator) Hash() common.Hash {
	return (*hi.iterators)[0].it.Hash()
}

// Account returns the current key
func (hi *heapAccountIterator) Account() []byte {
	return (*hi.iterators)[0].it.Account()
}

// Release iterates over all the remaining live layer iterators and releases each
// of thme individually.
func (hi *heapAccountIterator) Release() {
	for _, it := range *hi.iterators {
		it.it.Release()
	}
	hi.iterators = nil
}
