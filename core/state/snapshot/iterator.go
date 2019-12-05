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

type Iterator interface {
	// Next steps the iterator forward one element, and returns false if
	// the iterator is exhausted
	Next() bool
	// Key returns the current key
	Key() common.Hash
	// Seek steps the iterator forward as many elements as needed, so that after
	// calling Next(), the iterator will be at a key higher than the given hash
	Seek(common.Hash)
}

func (dl *diffLayer) newIterator() Iterator {
	dl.AccountList()
	return &dlIterator{dl, -1}
}

type dlIterator struct {
	layer *diffLayer
	index int
}

func (it *dlIterator) Next() bool {
	if it.index < len(it.layer.accountList) {
		it.index++
	}
	return it.index < len(it.layer.accountList)
}

func (it *dlIterator) Key() common.Hash {
	if it.index < len(it.layer.accountList) {
		return it.layer.accountList[it.index]
	}
	return common.Hash{}
}

func (it *dlIterator) Seek(key common.Hash) {
	// Search uses binary search to find and return the smallest index i
	// in [0, n) at which f(i) is true
	size := len(it.layer.accountList)
	index := sort.Search(size,
		func(i int) bool {
			v := it.layer.accountList[i]
			return bytes.Compare(key[:], v[:]) < 0
		})
	it.index = index - 1
}

type binaryIterator struct {
	a     Iterator
	b     Iterator
	aDone bool
	bDone bool
	k     common.Hash
}

func (dl *diffLayer) newBinaryIterator() Iterator {
	parent, ok := dl.parent.(*diffLayer)
	if !ok {
		// parent is the disk layer
		return dl.newIterator()
	}
	l := &binaryIterator{
		a: dl.newIterator(),
		b: parent.newBinaryIterator()}

	l.aDone = !l.a.Next()
	l.bDone = !l.b.Next()
	return l
}

func (it *binaryIterator) Next() bool {

	if it.aDone && it.bDone {
		return false
	}
	nextB := it.b.Key()
first:
	nextA := it.a.Key()
	if it.aDone {
		it.bDone = !it.b.Next()
		it.k = nextB
		return true
	}
	if it.bDone {
		it.aDone = !it.a.Next()
		it.k = nextA
		return true
	}
	if diff := bytes.Compare(nextA[:], nextB[:]); diff < 0 {
		it.aDone = !it.a.Next()
		it.k = nextA
		return true
	} else if diff == 0 {
		// Now we need to advance one of them
		it.aDone = !it.a.Next()
		goto first
	}
	it.bDone = !it.b.Next()
	it.k = nextB
	return true
}

func (it *binaryIterator) Key() common.Hash {
	return it.k
}
func (it *binaryIterator) Seek(key common.Hash) {
	panic("todo: implement")
}

func (dl *diffLayer) iterators() []Iterator {
	if parent, ok := dl.parent.(*diffLayer); ok {
		iterators := parent.iterators()
		return append(iterators, dl.newIterator())
	}
	return []Iterator{dl.newIterator()}
}

// fastIterator is a more optimized multi-layer iterator which maintains a
// direct mapping of all iterators leading down to the bottom layer
type fastIterator struct {
	iterators []Iterator
	initiated bool
}

// Len returns the number of active iterators
func (fi *fastIterator) Len() int {
	return len(fi.iterators)
}

// Less implements sort.Interface
func (fi *fastIterator) Less(i, j int) bool {
	a := fi.iterators[i].Key()
	b := fi.iterators[j].Key()
	return bytes.Compare(a[:], b[:]) < 0
}

// Swap implements sort.Interface
func (fi *fastIterator) Swap(i, j int) {
	fi.iterators[i], fi.iterators[j] = fi.iterators[j], fi.iterators[i]
}

// Next implements the Iterator interface. It returns false if no more elemnts
// can be retrieved (false == exhausted)
func (fi *fastIterator) Next() bool {
	if len(fi.iterators) == 0 {
		return false
	}
	if !fi.initiated {
		// Don't forward first time -- we had to 'Next' once in order to
		// do the sorting already
		fi.initiated = true
		return true
	}
	return fi.innerNext(0)
}

// innerNext handles the next operation internally,
// and should be invoked when we know that two elements in the list may have
// the same value.
// For example, if the list becomes [2,3,5,5,8,9,10], then we should invoke
// innerNext(3), which will call Next on elem 3 (the second '5'). It will continue
// along the list and apply the same operation if needed
func (fi *fastIterator) innerNext(pos int) bool {
	if !fi.iterators[pos].Next() {
		//Exhausted, remove this iterator
		fi.remove(pos)
		if len(fi.iterators) == 0 {
			return false
		}
		return true
	}
	if pos == len(fi.iterators)-1 {
		// Only one iterator left
		return true
	}
	// We next:ed the elem at 'pos'. Now we may have to re-sort that elem
	val, neighbour := fi.iterators[pos].Key(), fi.iterators[pos+1].Key()
	diff := bytes.Compare(val[:], neighbour[:])
	if diff < 0 {
		// It is still in correct place
		return true
	}
	if diff == 0 {
		// It has same value as the neighbour. So still in correct place, but
		// we need to iterate on the neighbour
		fi.innerNext(pos + 1)
		return true
	}
	// At this point, the elem is in the wrong location, but the
	// remaining list is sorted. Find out where to move the elem
	iterationNeeded := false
	index := sort.Search(len(fi.iterators), func(n int) bool {
		if n <= pos {
			// No need to search 'behind' us
			return false
		}
		if n == len(fi.iterators)-1 {
			// Can always place an elem last
			return true
		}
		neighbour := fi.iterators[n+1].Key()
		diff := bytes.Compare(val[:], neighbour[:])
		if diff == 0 {
			// The elem we're placing it next to has the same value,
			// so it's going to need further iteration
			iterationNeeded = true
		}
		return diff < 0
	})
	fi.move(pos, index)
	if iterationNeeded {
		fi.innerNext(index)
	}
	return true
}

// move moves an iterator to another position in the list
func (fi *fastIterator) move(index, newpos int) {
	if newpos > len(fi.iterators)-1 {
		newpos = len(fi.iterators) - 1
	}
	var (
		elem   = fi.iterators[index]
		middle = fi.iterators[index+1 : newpos+1]
		suffix []Iterator
	)
	if newpos < len(fi.iterators)-1 {
		suffix = fi.iterators[newpos+1:]
	}
	fi.iterators = append(fi.iterators[:index], middle...)
	fi.iterators = append(fi.iterators, elem)
	fi.iterators = append(fi.iterators, suffix...)
}

// remove drops an iterator from the list
func (fi *fastIterator) remove(index int) {
	fi.iterators = append(fi.iterators[:index], fi.iterators[index+1:]...)
}

// Key returns the current key
func (fi *fastIterator) Key() common.Hash {
	return fi.iterators[0].Key()
}

func (fi *fastIterator) Seek(key common.Hash) {
	// We need to apply this across all iterators
	var seen = make(map[common.Hash]struct{})

	length := len(fi.iterators)
	for i, it := range fi.iterators {
		it.Seek(key)
		for {
			if !it.Next() {
				// To be removed
				// swap it to the last position for now
				fi.iterators[i], fi.iterators[length-1] = fi.iterators[length-1], fi.iterators[i]
				length--
				break
			}
			v := it.Key()
			if _, exist := seen[v]; !exist {
				seen[v] = struct{}{}
				break
			}
		}
	}
	// Now remove those that were placed in the end
	fi.iterators = fi.iterators[:length]
	// The list is now totally unsorted, need to re-sort the entire list
	sort.Sort(fi)
	fi.initiated = false
}

// The fast iterator does not query parents as much.
func (dl *diffLayer) newFastIterator() Iterator {
	f := &fastIterator{dl.iterators(), false}
	f.Seek(common.Hash{})
	return f
}

// Debug is a convencience helper during testing
func (fi *fastIterator) Debug() {
	for _, it := range fi.iterators {
		fmt.Printf(" %v ", it.Key()[31])
	}
	fmt.Println()
}
