package snapshot

import (
	"math/big"
	"math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

func randomAccount() []byte {
	root := randomHash()
	a := Account{
		Balance:  big.NewInt(rand.Int63()),
		Nonce:    rand.Uint64(),
		Root:     root[:],
		CodeHash: emptyCode[:],
	}
	data, _ := rlp.EncodeToBytes(a)
	return data
}

// TestMergeBasics tests some simple merges
func TestMergeBasics(t *testing.T) {
	var (
		accounts = make(map[common.Hash][]byte)
		storage  = make(map[common.Hash]map[common.Hash][]byte)
	)
	// Fill up a parent
	for i := 0; i < 100; i++ {
		h := randomHash()
		data := randomAccount()

		accounts[h] = data
		if rand.Intn(20) < 10 {
			accStorage := make(map[common.Hash][]byte)
			value := make([]byte, 32)
			rand.Read(value)
			accStorage[randomHash()] = value
			storage[h] = accStorage
		}
	}
	// Add some (identical) layers on top
	parent := newDiffLayer(nil, 1, common.Hash{}, accounts, storage)
	child := newDiffLayer(parent, 1, common.Hash{}, accounts, storage)
	child = newDiffLayer(child, 1, common.Hash{}, accounts, storage)
	child = newDiffLayer(child, 1, common.Hash{}, accounts, storage)
	child = newDiffLayer(child, 1, common.Hash{}, accounts, storage)
	// And flatten
	merged := (child.flatten()).(*diffLayer)
	if got, exp := len(merged.accountList), len(accounts); got != exp {
		t.Errorf("accountList wrong, got %v exp %v", got, exp)
	}
	if got, exp := len(merged.storageList), len(storage); got != exp {
		t.Errorf("storageList wrong, got %v exp %v", got, exp)
	}
}

// TestMergeDelete tests some deletion
func TestMergeDelete(t *testing.T) {
	var (
		accountsA = make(map[common.Hash][]byte)
		storage   = make(map[common.Hash]map[common.Hash][]byte)
		accountsB = make(map[common.Hash][]byte)
	)
	// Fill up a parent
	h1 := common.HexToHash("0x01")
	h2 := common.HexToHash("0x02")

	accountsA[h1] = randomAccount()
	accountsA[h2] = nil

	accountsB[h1] = nil
	accountsB[h2] = randomAccount()

	// Add some flip-flopping layers on top
	parent := newDiffLayer(nil, 1, common.Hash{}, accountsA, storage)
	child := newDiffLayer(parent, 2, common.Hash{}, accountsB, storage)
	child = newDiffLayer(child, 3, common.Hash{}, accountsA, storage)
	if child.Account(h1) == nil {
		t.Errorf("last diff layer: expected %x to be non-nil", h1)
	}
	if child.Account(h2) != nil {
		t.Errorf("last diff layer: expected %x to be nil", h2)
	}
	// And flatten
	merged := (child.flatten()).(*diffLayer)
	// These fails because the accounts-slice is reused, and not copied
	// thus the flattening will affect the upper layers. Maybe that's totally
	// fine, but if not, that needs to be taken care of
	if merged.Account(h1) == nil {
		t.Errorf("merged layer: expected %x to be non-nil", h1)
	}
	if merged.Account(h2) != nil {
		t.Errorf("merged layer: expected %x to be nil", h2)
	}
}

// TestMergeDelete2 tests some deletion
func TestMergeDelete2(t *testing.T) {
	var (
		storage = make(map[common.Hash]map[common.Hash][]byte)
	)
	// Fill up a parent
	h1 := common.HexToHash("0x01")
	h2 := common.HexToHash("0x02")

	flip := func() map[common.Hash][]byte {
		accs := make(map[common.Hash][]byte)
		accs[h1] = randomAccount()
		accs[h2] = nil
		return accs
	}
	flop := func() map[common.Hash][]byte {
		accs := make(map[common.Hash][]byte)
		accs[h1] = nil
		accs[h2] = randomAccount()
		return accs
	}

	// Add some flip-flopping layers on top
	parent := newDiffLayer(nil, 1, common.Hash{}, flip(), storage)
	child := newDiffLayer(parent, 2, common.Hash{}, flop(), storage)
	child = newDiffLayer(child, 3, common.Hash{}, flip(), storage)
	child = newDiffLayer(child, 3, common.Hash{}, flop(), storage)
	child = newDiffLayer(child, 3, common.Hash{}, flip(), storage)
	child = newDiffLayer(child, 3, common.Hash{}, flop(), storage)
	child = newDiffLayer(child, 3, common.Hash{}, flip(), storage)
	if child.Account(h1) == nil {
		t.Errorf("last diff layer: expected %x to be non-nil", h1)
	}
	if child.Account(h2) != nil {
		t.Errorf("last diff layer: expected %x to be nil", h2)
	}
	// And flatten
	merged := (child.flatten()).(*diffLayer)
	if merged.Account(h1) == nil {
		t.Errorf("merged layer: expected %x to be non-nil", h1)
	}
	if merged.Account(h2) != nil {
		t.Errorf("merged layer: expected %x to be nil", h2)
	}
	if got, exp := merged.memory, child.memory; got != exp {
		t.Errorf("mem wrong, got %d, exp %d", got, exp)
	}
}
