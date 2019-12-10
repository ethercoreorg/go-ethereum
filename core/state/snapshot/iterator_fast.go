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
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
)

// weightedAccountIterator is an account iterator with an assigned weight. It is
// used to prioritise which account is the correct one if multiple iterators find
// the same one (modified in multiple consecutive blocks).
type weightedAccountIterator struct {
	it       AccountIterator
	priority int
}

// fastAccountIterator is a more optimized multi-layer iterator which maintains a
// direct mapping of all iterators leading down to the bottom layer.
type fastAccountIterator struct {
	iterators []*weightedAccountIterator
	initiated bool
	fail      error
}

// newFastAccountIterator creates a new hierarhical account iterator with one
// element per diff layer. The returned combo iterator can be used to walk over
// the entire snapshot diff stack simultaneously.
func (dl *diffLayer) newFastAccountIterator() AccountIterator {
	fi := new(fastAccountIterator)
	for i, it := range dl.iterators() {
		fi.iterators = append(fi.iterators, &weightedAccountIterator{
			it:       it,
			priority: -i,
		})
	}
	fi.Seek(common.Hash{})
	return fi
}

// Len implements sort.Interface, returning the number of active iterators.
func (fi *fastAccountIterator) Len() int {
	return len(fi.iterators)
}

// Less implements sort.Interface, returning which of two iterators in the stack
// is before the other.
func (fi *fastAccountIterator) Less(i, j int) bool {
	// Order the iterators primarilly by the account hashes
	hashI := fi.iterators[i].it.Hash()
	hashJ := fi.iterators[j].it.Hash()

	switch bytes.Compare(hashI[:], hashJ[:]) {
	case -1:
		return true
	case 1:
		return false
	}
	// Same account in multiple layers, split by priority
	return fi.iterators[i].priority < fi.iterators[j].priority
}

// Swap implements sort.Interface, swapping two entries in the iterator stack.
func (fi *fastAccountIterator) Swap(i, j int) {
	fi.iterators[i], fi.iterators[j] = fi.iterators[j], fi.iterators[i]
}

// Seek steps the iterator forward as many elements as needed, so that after
// calling Next(), the iterator will be at a key higher than the given hash.
func (fi *fastAccountIterator) Seek(hash common.Hash) {
	// Track which account hashes are iterators positioned on
	var positioned = make(map[common.Hash]int)

	// Position all iterators and track how many remain live
	for i := 0; i < len(fi.iterators); i++ {
		// Position the next iterator in line
		it := fi.iterators[i]
		it.it.Seek(hash)

		// Retrieve the first element and if it clashes with a previous iterator,
		// advance either the current one or the old one. Repeat until nothing is
		// clashing any more.
		for {
			// If the iterator is exhausted, drop it off the end
			if !it.it.Next() {
				last := len(fi.iterators) - 1

				fi.iterators[i] = fi.iterators[last]
				fi.iterators[last] = nil
				fi.iterators = fi.iterators[:last]

				i-- // TODO(karalabe): this was missing, add a test that catches it
				break
			}
			// The iterator is still alive, check for collisions with previous ones
			hash := it.it.Hash()
			if other, exist := positioned[hash]; !exist {
				positioned[hash] = i
				break
			} else {
				// Iterators collide, one needs to be progressed, use priority to
				// determine which.
				//
				// This whole else-block can be avoided, if we instead
				// do an inital priority-sort of the iterators. If we do that,
				// then we'll only wind up here if a lower-priority (preferred) iterator
				// has the same value, and then we will always just continue.
				// However, it costs an extra sort, so it's probably not better
				if fi.iterators[other].priority < it.priority {
					// The 'it' should be progressed
					continue
				} else {
					// The 'other' should be progressed, swap them
					it = fi.iterators[other]
					fi.iterators[other], fi.iterators[i] = fi.iterators[i], fi.iterators[other]
					continue
				}
			}
		}
	}
	// Re-sort the entire list
	sort.Sort(fi)
	fi.initiated = false
}

// Next steps the iterator forward one element, returning false if exhausted.
func (fi *fastAccountIterator) Next() bool {
	if len(fi.iterators) == 0 {
		return false
	}
	if !fi.initiated {
		// Don't forward first time -- we had to 'Next' once in order to
		// do the sorting already
		fi.initiated = true
		return true
	}
	return fi.next(0)
}

// next handles the next operation internally and should be invoked when we know
// that two elements in the list may have the same value.
//
// For example, if the iterated hashes become [2,3,5,5,8,9,10], then we should
// invoke next(3), which will call Next on elem 3 (the second '5') and will
// cascade along the list, applying the same operation if needed.
func (fi *fastAccountIterator) next(idx int) bool {
	// If this particular iterator got exhausted, remove it and return true (the
	// next one is surely not exhausted yet, otherwise it would have been removed
	// already).
	if !fi.iterators[idx].it.Next() {
		fi.iterators = append(fi.iterators[:idx], fi.iterators[idx+1:]...)
		return len(fi.iterators) > 0
	}
	// If there's noone left to cascade into, return
	if idx == len(fi.iterators)-1 {
		return true
	}
	// We next-ed the iterator at 'idx', now we may have to re-sort that element
	var (
		cur, next         = fi.iterators[idx], fi.iterators[idx+1]
		curHash, nextHash = cur.it.Hash(), next.it.Hash()
	)
	if diff := bytes.Compare(curHash[:], nextHash[:]); diff < 0 {
		// It is still in correct place
		return true
	} else if diff == 0 && cur.priority < next.priority {
		// So still in correct place, but we need to iterate on the next
		fi.next(idx + 1)
		return true
	}
	// At this point, the iterator is in the wrong location, but the remaining
	// list is sorted. Find out where to move the item.
	clash := -1
	index := sort.Search(len(fi.iterators), func(n int) bool {
		// The iterator always advances forward, so anything before the old slot
		// is known to be behind us, so just skip them altogether. This actually
		// is an important clause since the sort order got invalidated.
		if n < idx {
			return false
		}
		if n == len(fi.iterators)-1 {
			// Can always place an elem last
			return true
		}
		nextHash := fi.iterators[n+1].it.Hash()
		if diff := bytes.Compare(curHash[:], nextHash[:]); diff < 0 {
			return true
		} else if diff > 0 {
			return false
		}
		// The elem we're placing it next to has the same value,
		// so whichever winds up on n+1 will need further iteraton
		clash = n + 1
		if cur.priority < fi.iterators[n+1].priority {
			// We can drop the iterator here
			return true
		}
		// We need to move it one step further
		return false
		// TODO benchmark which is best, this works too:
		//clash = n
		//return true
		// Doing so should finish the current search earlier
	})
	fi.move(idx, index)
	if clash != -1 {
		fi.next(clash)
	}
	return true
}

// move advances an iterator to another position in the list.
func (fi *fastAccountIterator) move(index, newpos int) {
	elem := fi.iterators[index]
	copy(fi.iterators[index:], fi.iterators[index+1:newpos+1])
	fi.iterators[newpos] = elem
}

// Error returns any failure that occurred during iteration, which might have
// caused a premature iteration exit (e.g. snapshot stack becoming stale).
func (fi *fastAccountIterator) Error() error {
	return fi.fail
}

// Hash returns the current key
func (fi *fastAccountIterator) Hash() common.Hash {
	return fi.iterators[0].it.Hash()
}

// Account returns the current key
func (fi *fastAccountIterator) Account() []byte {
	return fi.iterators[0].it.Account()
}

// Debug is a convencience helper during testing
func (fi *fastAccountIterator) Debug() {
	for _, it := range fi.iterators {
		fmt.Printf("[p=%v v=%v] ", it.priority, it.it.Hash()[0])
	}
	fmt.Println()
}
