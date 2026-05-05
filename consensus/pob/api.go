// consensus/pob/api.go
// JSON-RPC API exposed by the Proof-of-Balance engine under the "pob" namespace.

package pob

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
)

// API provides an RPC API to query the current state of the PoB engine.
type API struct {
	chain consensus.ChainHeaderReader
	pob   *PoB
}

// ValidatorInfo holds public info about one pool member.
type ValidatorInfo struct {
	Address    common.Address `json:"address"`
	Balance    *big.Int       `json:"balance"`
	WeightPct  float64        `json:"weightPct"` // balance / totalBalance × 100
}

// PoolStatus is the top-level response for pob_getPoolStatus.
type PoolStatus struct {
	BlockNumber    uint64          `json:"blockNumber"`
	PoolSize       int             `json:"poolSize"`
	TotalBalance   *big.Int        `json:"totalBalance"`
	NextProposer   common.Address  `json:"nextProposer"`
	Validators     []ValidatorInfo `json:"validators"`
}

// GetPoolStatus returns the current validator pool at the latest block.
func (api *API) GetPoolStatus() (*PoolStatus, error) {
	header := api.chain.CurrentHeader()
	snap, err := loadSnapshot(api.pob.config, api.pob.db, header.Hash())
	if err != nil {
		return nil, err
	}

	pool := snap.Pool()
	total := snap.TotalBalance()

	infos := make([]ValidatorInfo, 0, len(pool))
	for _, c := range pool {
		pct := 0.0
		if total.Sign() > 0 {
			f, _ := new(big.Float).
				Quo(new(big.Float).SetInt(c.Balance),
					new(big.Float).SetInt(total)).
				Float64()
			pct = f * 100
		}
		infos = append(infos, ValidatorInfo{
			Address:   c.Address,
			Balance:   c.Balance,
			WeightPct: pct,
		})
	}

	nextProposer := snap.Proposer(header.Hash(), header.Number.Uint64()+1)

	return &PoolStatus{
		BlockNumber:  header.Number.Uint64(),
		PoolSize:     len(pool),
		TotalBalance: total,
		NextProposer: nextProposer,
		Validators:   infos,
	}, nil
}

// GetProposerAt returns the expected proposer for a specific block number.
func (api *API) GetProposerAt(blockNumber uint64) (common.Address, error) {
	header := api.chain.GetHeaderByNumber(blockNumber - 1)
	if header == nil {
		return common.Address{}, errUnknownBlock
	}
	snap, err := loadSnapshot(api.pob.config, api.pob.db, header.Hash())
	if err != nil {
		return common.Address{}, err
	}
	return snap.Proposer(header.Hash(), blockNumber), nil
}

// GetCandidateWeight returns the probability weight (0-1) of the given address
// at the latest snapshot.
func (api *API) GetCandidateWeight(addr common.Address) (float64, error) {
	header := api.chain.CurrentHeader()
	snap, err := loadSnapshot(api.pob.config, api.pob.db, header.Hash())
	if err != nil {
		return 0, err
	}
	bal, ok := snap.Validators[addr]
	if !ok {
		return 0, nil
	}
	total := snap.TotalBalance()
	if total.Sign() == 0 {
		return 0, nil
	}
	f, _ := new(big.Float).
		Quo(new(big.Float).SetInt(bal), new(big.Float).SetInt(total)).
		Float64()
	return f, nil
}
