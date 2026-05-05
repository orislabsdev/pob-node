// consensus/pob/snapshot.go
//
// Snapshot stores the validator pool at a specific block height.
// Every `Epoch` blocks a new snapshot is computed by scanning the known
// account set and re-ranking candidates by balance.  Between epoch
// boundaries the snapshot is inherited from the most recent epoch block.

package pob

import (
	"encoding/json"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// Snapshot is the state of the validator pool at a given block.
type Snapshot struct {
	Config *params.PoBConfig

	Number    uint64                      `json:"number"`    // Block number the snapshot was created at
	Hash      common.Hash                 `json:"hash"`      // Block hash
	Validators map[common.Address]*big.Int `json:"validators"` // address -> balance at snapshot time
	Recents   map[uint64]common.Address   `json:"recents"`   // recent block numbers to recent signers
}

// newSnapshot creates a brand-new snapshot with the given validator set.
func newSnapshot(config *params.PoBConfig, number uint64, hash common.Hash, validators []Candidate) *Snapshot {
	snap := &Snapshot{
		Config:     config,
		Number:     number,
		Hash:       hash,
		Validators: make(map[common.Address]*big.Int),
		Recents:    make(map[uint64]common.Address),
	}
	for _, v := range validators {
		snap.Validators[v.Address] = new(big.Int).Set(v.Balance)
	}
	return snap
}

// loadSnapshot loads an existing snapshot from the database.
func loadSnapshot(config *params.PoBConfig, db ethdb.Database, hash common.Hash) (*Snapshot, error) {
	blob, err := db.Get(append([]byte("pob-"), hash[:]...))
	if err != nil {
		return nil, err
	}
	snap := new(Snapshot)
	if err := json.Unmarshal(blob, snap); err != nil {
		return nil, err
	}
	snap.Config = config
	return snap, nil
}

// store saves a snapshot into the database.
func (s *Snapshot) store(db ethdb.Database) error {
	blob, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return db.Put(append([]byte("pob-"), s.Hash[:]...), blob)
}

// copy creates a deep copy of the snapshot.
func (s *Snapshot) copy() *Snapshot {
	cpy := &Snapshot{
		Config:     s.Config,
		Number:     s.Number,
		Hash:       s.Hash,
		Validators: make(map[common.Address]*big.Int, len(s.Validators)),
		Recents:    make(map[uint64]common.Address, len(s.Recents)),
	}
	for addr, bal := range s.Validators {
		cpy.Validators[addr] = new(big.Int).Set(bal)
	}
	for block, addr := range s.Recents {
		cpy.Recents[block] = addr
	}
	return cpy
}

// apply processes a batch of headers and updates the snapshot accordingly.
func (s *Snapshot) apply(headers []*types.Header, pob *PoB, candidates func() []Candidate) (*Snapshot, error) {
	if len(headers) == 0 {
		return s, nil
	}
	snap := s.copy()

	for _, header := range headers {
		number := header.Number.Uint64()

		// Evict oldest recents to allow signers to sign again
		if limit := uint64(len(snap.Validators)/2 + 1); number >= limit {
			delete(snap.Recents, number-limit)
		}

		// Retrieve the signer for this block
		signer, err := ecrecover(header, pob.signatures, pob.chainConfig.ChainID)
		if err != nil {
			return nil, err
		}
		snap.Recents[number] = signer
		pob.addCandidate(signer)

		// At epoch boundaries, refresh the validator set from on-chain balances.
		if number%snap.Config.Epoch == 0 && candidates != nil {
			pool := candidates()
			snap.Validators = make(map[common.Address]*big.Int, len(pool))
			for _, c := range pool {
				snap.Validators[c.Address] = c.Balance
			}
			log.Info("PoB: validator pool refreshed",
				"block", number,
				"validators", len(snap.Validators))
		}

		snap.Number = number
		snap.Hash = header.Hash()
	}
	return snap, nil
}

// Pool returns the current active pool as a sorted slice of Candidates.
func (s *Snapshot) Pool() []Candidate {
	pool := make([]Candidate, 0, len(s.Validators))
	for addr, bal := range s.Validators {
		pool = append(pool, Candidate{Address: addr, Balance: new(big.Int).Set(bal)})
	}
	sort.Slice(pool, func(i, j int) bool {
		return pool[i].Balance.Cmp(pool[j].Balance) > 0
	})
	return pool
}

// Proposer returns the expected proposer for the next block.
func (s *Snapshot) Proposer(parentHash common.Hash, nextNumber uint64) common.Address {
	return SelectProposer(s.Pool(), parentHash, nextNumber)
}

// IsValidator returns whether the given address is in the current pool.
func (s *Snapshot) IsValidator(addr common.Address) bool {
	_, ok := s.Validators[addr]
	return ok
}

// TotalBalance returns the sum of all validator balances.
func (s *Snapshot) TotalBalance() *big.Int {
	total := new(big.Int)
	for _, bal := range s.Validators {
		total.Add(total, bal)
	}
	return total
}
