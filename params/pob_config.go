// params/pob_config.go
// Custom chain configuration for Proof-of-Balance (PoB) EVM Chain
// ChainID: 14558 — includes all BSC fork activations at block 0.

package params

import (
	"math/big"
)

// PoBChainConfig is the canonical chain configuration for the PoB network.
var PoBChainConfig = &ChainConfig{
	ChainID:             big.NewInt(14558),
	HomesteadBlock:      big.NewInt(0),
	EIP150Block:         big.NewInt(0),
	EIP155Block:         big.NewInt(0),
	EIP158Block:         big.NewInt(0),
	ByzantiumBlock:      big.NewInt(0),
	ConstantinopleBlock: big.NewInt(0),
	PetersburgBlock:     big.NewInt(0),
	IstanbulBlock:       big.NewInt(0),
	MuirGlacierBlock:   big.NewInt(0),
	BerlinBlock:        big.NewInt(0),
	LondonBlock:        big.NewInt(0),

	// ── BSC / BNB chain fork schedule (all activated at genesis) ────────────
	RamanujanBlock:  big.NewInt(0),
	NielsBlock:      big.NewInt(0),
	MirrorSyncBlock: big.NewInt(0),
	BrunoBlock:      big.NewInt(0),
	EulerBlock:      big.NewInt(0),
	GibbsBlock:      big.NewInt(0),
	NanoBlock:       big.NewInt(0),
	MoranBlock:      big.NewInt(0),
	PlanckBlock:     big.NewInt(0),
	LubanBlock:      big.NewInt(0),
	PlatoBlock:      big.NewInt(0),
	HertzBlock:      big.NewInt(0),
	HertzfixBlock:   big.NewInt(0),

	// Time-based forks (unix timestamp 0 = genesis)
	ShanghaiTime:   newUint64(0),
	KeplerTime:     newUint64(0),
	FeynmanTime:    newUint64(0),
	FeynmanFixTime: newUint64(0),
	CancunTime:     newUint64(0),
	HaberTime:      newUint64(0),
	HaberFixTime:   newUint64(0),
	BohrTime:       newUint64(0),
	PascalTime:     newUint64(0),
	PragueTime:     newUint64(0),
	LorentzTime:    newUint64(0),
	MaxwellTime:    newUint64(0),
	FermiTime:      newUint64(0),
	MendelTime:     newUint64(0),

	// ── Proof-of-Balance engine ─────────────────────────────────────────────
	Parlia: nil, // disable Parlia (BSC default)
	Pob:    &PoBConfig{
		Period:         3,                                    // seconds between blocks
		Epoch:          200,                                  // blocks per validator-set snapshot
		PoolSize:       21,                                   // max active validators
		MinBalanceWei:  big.NewInt(1e18),                    // 1 native token minimum
		RewardPerBlock: big.NewInt(1e18),                    // 1 token block reward
	},
	BlobScheduleConfig: DefaultBlobSchedule,
}

// PoBConfig holds the configuration values for the Proof-of-Balance consensus engine.
type PoBConfig struct {
	// Period is the minimum number of seconds between consecutive blocks.
	Period uint64 `json:"period"`

	// Epoch is how many blocks between full validator-set recalculations.
	Epoch uint64 `json:"epoch"`

	// PoolSize is the maximum number of validators in the active mining pool.
	// Candidates are ranked by descending balance; top PoolSize addresses win
	// a seat.
	PoolSize int `json:"poolSize"`

	// MinBalanceWei is the smallest balance (in wei) required for a node to
	// be eligible to enter the mining pool.
	MinBalanceWei *big.Int `json:"minBalanceWei"`

	// RewardPerBlock is the block reward (in wei) paid to the block proposer.
	RewardPerBlock *big.Int `json:"rewardPerBlock"`

	// MaxSupply is the total supply of tokens (in wei).
	// If not set, defaults to 21 million tokens for PoB.
	MaxSupply *big.Int `json:"maxSupply,omitempty"`

	// GenesisAllocation is the total amount of tokens (in wei) pre-allocated in the genesis block.
	// This is calculated at genesis time and subtracted from the reward pool.
	GenesisAllocation *big.Int `json:"genesisAllocation,omitempty"`
}

// String implements the stringer interface, returning the consensus engine details.
func (c *PoBConfig) String() string {
	return "pob"
}
