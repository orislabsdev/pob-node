// consensus/pob/pob.go
//
// Proof-of-Balance (PoB) consensus engine.
//
// How it works
// ────────────
//  1. Any account whose on-chain balance ≥ MinBalanceWei is a *candidate*.
//  2. Every `Epoch` blocks the engine re-ranks all candidates by descending
//     balance and keeps the top `PoolSize` addresses as the *active validator
//     pool* (snapshot).
//  3. For each block the proposer is chosen via a **weighted lottery**:
//       weight_i = balance_i / Σ balance_j   (j ∈ active pool)
//     A deterministic seed derived from the parent-block hash and the block
//     number is used so every node agrees on the winner without communication.
//  4. The chosen proposer's address must match the block's `Coinbase`; any
//     other sealer is rejected.
//  5. Difficulty encodes the proposer's normalised weight × 1e9 so that
//     uncle / fork-choice still prefers the highest-balance proposer.

package pob

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	lru "github.com/hashicorp/golang-lru"
)

const (
	inmemorySnapshots  = 128  // number of recent snapshots to keep in memory
	inmemorySignatures = 4096 // recent block signatures cached in memory
	wiggleTime         = 500 * time.Millisecond
)

var (
	errUnknownBlock       = errors.New("pob: unknown block")
	errMissingVanity      = errors.New("pob: extra-data must be at least 32 bytes")
	errMissingSignature   = errors.New("pob: extra-data must hold a 65-byte secp256k1 signature")
	errInvalidMixDigest   = errors.New("pob: mix digest must be zero")
	errInvalidUncleHash   = errors.New("pob: uncles not allowed")
	errUnauthorizedSigner = errors.New("pob: address is not an authorised validator")
	errWrongDifficulty    = errors.New("pob: wrong difficulty")
	errInvalidPoolSize    = errors.New("pob: pool size must be > 0")
	errZeroEpoch          = errors.New("pob: epoch must be > 0")

	extraVanity = 32 // fixed number of extra-data prefix bytes reserved for signer vanity
	extraSeal   = 65 // fixed number of extra-data suffix bytes reserved for signer seal

	uncleHash = types.CalcUncleHash(nil) // uncles are always empty in PoB
)

// SignerFn is a signer callback function to request a header to be signed by a
// backing account.
type SignerFn func(signer accounts.Account, mimeType string, message []byte) ([]byte, error)

// PoB is the Proof-of-Balance consensus engine.
type PoB struct {
	config      *params.PoBConfig
	chainConfig *params.ChainConfig
	db          ethdb.Database
	recents     *lru.ARCCache // Snapshots for recent blocks
	signatures  *lru.ARCCache // Recovered signers for recent blocks

	proposals map[common.Address]bool // current list of proposals we are pushing

	signer      common.Address // Ethereum address of the signing key
	signFn      SignerFn       // Signer function to authorise hashes with
	lock        sync.RWMutex   // Protects the signer and proposals fields

	ethAPI      *ethapi.BlockChainAPI
	genesisHash common.Hash

	candidates  map[common.Address]struct{} // addresses seen active on chain
	candLock    sync.RWMutex

	fakeDiff bool // Skip difficulty verifications
}

// New creates a Proof-of-Balance consensus engine with the given configuration.
func New(config *params.PoBConfig, chainConfig *params.ChainConfig, db ethdb.Database, ee *ethapi.BlockChainAPI, genesisHash common.Hash) *PoB {
	cfg := *config
	if cfg.Epoch == 0 {
		cfg.Epoch = 200
	}
	if cfg.PoolSize == 0 {
		cfg.PoolSize = 21
	}
	if cfg.MinBalanceWei == nil || cfg.MinBalanceWei.Sign() == 0 {
		cfg.MinBalanceWei = new(big.Int).SetUint64(1e18)
	}
	if cfg.RewardPerBlock == nil || cfg.RewardPerBlock.Sign() == 0 {
		cfg.RewardPerBlock = new(big.Int).SetUint64(1e18)
	}

	recents, _ := lru.NewARC(inmemorySnapshots)
	signatures, _ := lru.NewARC(inmemorySignatures)

	p := &PoB{
		config:      &cfg,
		chainConfig: chainConfig,
		db:          db,
		recents:     recents,
		signatures:  signatures,
		proposals:   make(map[common.Address]bool),
		ethAPI:      ee,
		genesisHash: genesisHash,
		candidates:  make(map[common.Address]struct{}),
	}
	p.loadCandidates()
	return p
}

// IsSystemTransaction reports whether the transaction is a system transaction.
// PoB uses system transactions for block rewards. These are unsigned.
func (p *PoB) IsSystemTransaction(tx *types.Transaction, header *types.Header) (bool, error) {
	// System transactions in PoB are unsigned (V, R, S are zero)
	v, r, s := tx.RawSignatureValues()
	if v.Sign() != 0 || r.Sign() != 0 || s.Sign() != 0 {
		return false, nil
	}

	// For rewards: PoBRewardAddress -> Coinbase
	if tx.To() != nil && *tx.To() == header.Coinbase {
		return true, nil
	}
	
	return false, nil
}

// IsSystemContract reports whether the address is a system contract.
func (p *PoB) IsSystemContract(to *common.Address) bool {
	return to != nil && *to == params.PoBRewardAddress
}

// EnoughDistance reports whether the distance between the current block and the
// last finalized block is enough.
func (p *PoB) EnoughDistance(chain consensus.ChainReader, header *types.Header) bool {
	return true
}

// IsLocalBlock reports whether the block was mined locally.
func (p *PoB) IsLocalBlock(header *types.Header) bool {
	return true
}

// GetJustifiedNumberAndHash returns the highest justified block number and hash.
func (p *PoB) GetJustifiedNumberAndHash(chain consensus.ChainHeaderReader, headers []*types.Header) (uint64, common.Hash, error) {
	return 0, common.Hash{}, nil
}

// GetFinalizedHeader returns the highest finalized block header.
func (p *PoB) GetFinalizedHeader(chain consensus.ChainHeaderReader, header *types.Header) *types.Header {
	return nil
}

// CheckFinalityAndNotify checks the finality of the given block.
func (p *PoB) CheckFinalityAndNotify(chain consensus.ChainHeaderReader, targetBlockHash common.Hash, notifyFn func(finalizedHeader *types.Header)) {
}

// VerifyVote will verify the vote.
func (p *PoB) VerifyVote(chain consensus.ChainHeaderReader, vote *types.VoteEnvelope) error {
	return nil
}

// IsActiveValidatorAt reports whether the validator is active at the given header.
func (p *PoB) IsActiveValidatorAt(chain consensus.ChainHeaderReader, header *types.Header, checkVoteKeyFn func(bLSPublicKey *types.BLSPublicKey) bool) bool {
	return true
}

// NextProposalBlock returns the next proposal block number and timestamp.
func (p *PoB) NextProposalBlock(chain consensus.ChainHeaderReader, header *types.Header, proposer common.Address) (uint64, uint64, error) {
	return 0, 0, nil
}

// SignRecently reports whether the validator has signed recently.
func (p *PoB) SignRecently(chain consensus.ChainReader, parent *types.Header) (bool, error) {
	return false, nil
}

// BlockInterval returns the block interval.
func (p *PoB) BlockInterval(chain consensus.ChainHeaderReader, header *types.Header) (uint64, error) {
	return uint64(p.config.Period * 1000), nil
}

// EstimateGasReservedForSystemTxs returns the gas reserved for system transactions.
func (p *PoB) EstimateGasReservedForSystemTxs(chain consensus.ChainHeaderReader, header *types.Header) uint64 {
	// PoB doesn't reserve gas for system transactions yet
	return 0
}

var candidatesKey = []byte("pob-candidates")


func (p *PoB) loadCandidates() {
	p.candLock.Lock()
	defer p.candLock.Unlock()

	blob, err := p.db.Get(candidatesKey)
	if err != nil {
		return
	}
	var addrs []common.Address
	if err := json.Unmarshal(blob, &addrs); err != nil {
		log.Error("PoB: failed to unmarshal candidates", "err", err)
		return
	}
	for _, addr := range addrs {
		p.candidates[addr] = struct{}{}
	}
	log.Info("PoB: loaded candidates from disk", "count", len(p.candidates))
}

func (p *PoB) saveCandidates() {
	p.candLock.RLock()
	var addrs []common.Address
	for addr := range p.candidates {
		addrs = append(addrs, addr)
	}
	p.candLock.RUnlock()

	blob, err := json.Marshal(addrs)
	if err != nil {
		log.Error("PoB: failed to marshal candidates", "err", err)
		return
	}
	if err := p.db.Put(candidatesKey, blob); err != nil {
		log.Error("PoB: failed to save candidates to disk", "err", err)
	}
}

// Author returns the Ethereum address recovered from the signature in the
// header's extra-data section.
func (p *PoB) Author(header *types.Header) (common.Address, error) {
	return ecrecover(header, p.signatures, p.chainConfig.ChainID)
}

// VerifyHeader checks whether a header conforms to the consensus rules.
func (p *PoB) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	return p.verifyHeader(chain, header, nil)
}

// VerifyHeaders verifies a batch of headers concurrently.
func (p *PoB) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			err := p.verifyHeader(chain, header, headers[:i])
			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

func (p *PoB) verifyHeader(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	if header.Number == nil {
		return errUnknownBlock
	}
	// Extra data: vanity (32) + seal (65)
	if len(header.Extra) < extraVanity {
		return errMissingVanity
	}
	if len(header.Extra) < extraVanity+extraSeal {
		return errMissingSignature
	}
	// MixDigest must be zero
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}
	// No uncles
	if header.UncleHash != types.CalcUncleHash(nil) {
		return errInvalidUncleHash
	}
	// All basic checks passed, verify the seal
	return p.verifySeal(chain, header, parents)
}

func (p *PoB) verifySeal(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	number := header.Number.Uint64()
	if number == 0 {
		return nil
	}
	// Recover the signer
	signer, err := ecrecover(header, p.signatures, p.chainConfig.ChainID)
	if err != nil {
		return err
	}
	if signer != header.Coinbase {
		return errUnauthorizedSigner
	}

	// Retrieve the snapshot
	snap, err := p.snapshot(chain, number-1, header.ParentHash, parents)
	if err != nil {
		return err
	}

	// Verify the proposer
	proposer := snap.Proposer(header.ParentHash, number)
	if header.Coinbase != proposer {
		return errUnauthorizedSigner
	}

	// Verify difficulty
	if !p.fakeDiff {
		pool := snap.Pool()
		var candidate Candidate
		found := false
		for _, c := range pool {
			if c.Address == header.Coinbase {
				candidate = c
				found = true
				break
			}
		}
		if !found {
			return errUnauthorizedSigner
		}
		expectedDiff := ProposerDifficulty(candidate, pool)
		if header.Difficulty.Cmp(expectedDiff) != 0 {
			return errWrongDifficulty
		}
	}

	return nil
}

func (p *PoB) snapshot(chain consensus.ChainHeaderReader, number uint64, hash common.Hash, parents []*types.Header) (*Snapshot, error) {
	// Search in memory
	if s, ok := p.recents.Get(hash); ok {
		return s.(*Snapshot), nil
	}
	// Load from disk
	snap, err := loadSnapshot(p.config, p.db, hash)
	if err == nil {
		p.recents.Add(hash, snap)
		return snap, nil
	}
	// Not found, recursively compute
	if number == 0 {
		// Genesis snapshot
		genesis := chain.GenesisHeader()
		var initialValidator common.Address

		// If coinbase is set, use it as the first candidate
		if genesis.Coinbase != (common.Address{}) {
			initialValidator = genesis.Coinbase
		} else if len(genesis.Extra) >= extraVanity+common.AddressLength {
			// Fallback: extract from ExtraData (vanity (32) + address (20) + ...)
			copy(initialValidator[:], genesis.Extra[extraVanity:extraVanity+common.AddressLength])
		}

		if initialValidator == (common.Address{}) {
			log.Warn("PoB: No initial validator found in genesis", "hash", genesis.Hash())
			// This will likely cause the first block proposal to fail unless candidates are added via transactions
		}

		snap = newSnapshot(p.config, 0, genesis.Hash(), []Candidate{{Address: initialValidator, Balance: p.config.MinBalanceWei}})
		if err := snap.store(p.db); err != nil {
			return nil, err
		}
		p.addCandidate(initialValidator)
		p.recents.Add(genesis.Hash(), snap)
		return snap, nil
	}

	// Retrieve parent snapshot
	var parentHash common.Hash
	if len(parents) > 0 && parents[len(parents)-1].Number.Uint64() == number-1 {
		parentHash = parents[len(parents)-1].Hash()
	} else {
		parentHash = chain.GetHeaderByNumber(number).ParentHash
	}
	parentSnap, err := p.snapshot(chain, number-1, parentHash, parents)
	if err != nil {
		return nil, err
	}

	// Apply current header
	var header *types.Header
	if len(parents) > 0 && parents[len(parents)-1].Number.Uint64() == number {
		header = parents[len(parents)-1]
	} else {
		header = chain.GetHeader(hash, number)
	}

	snap, err = parentSnap.apply([]*types.Header{header}, p, func() []Candidate {
		// Pool refresh logic
		p.candLock.RLock()
		var addrs []common.Address
		for addr := range p.candidates {
			addrs = append(addrs, addr)
		}
		p.candLock.RUnlock()

		st, err := p.ethAPI.StateAt(hash)
		if err != nil {
			log.Error("PoB: failed to get state for pool refresh", "err", err, "hash", hash)
			return nil
		}
		return p.BuildPool(st, addrs)
	})
	if err != nil {
		return nil, err
	}

	if err := snap.store(p.db); err != nil {
		return nil, err
	}
	p.recents.Add(hash, snap)
	return snap, nil
}

// VerifyUncles verifies that a block's uncles conform to the consensus rules.
// PoB does not allow uncles.
func (p *PoB) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("pob: uncles not allowed")
	}
	return nil
}

// Prepare initialises the consensus fields of a block header according to
// the rules of a particular engine.
func (p *PoB) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return errUnknownBlock
	}
	// Initialise difficulty
	snap, err := p.snapshot(chain, header.Number.Uint64()-1, header.ParentHash, nil)
	if err != nil {
		return err
	}
	proposer := snap.Proposer(header.ParentHash, header.Number.Uint64())
	pool := snap.Pool()
	var candidate Candidate
	for _, c := range pool {
		if c.Address == proposer {
			candidate = c
			break
		}
	}
	header.Difficulty = ProposerDifficulty(candidate, pool)

	header.MixDigest = common.Hash{}
	if len(header.Extra) < extraVanity {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraVanity-len(header.Extra))...)
	}
	header.Extra = header.Extra[:extraVanity]
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	// Ensure block time is at least parent + Period
	if header.Time < parent.Time+p.config.Period {
		header.Time = parent.Time + p.config.Period
	}

	// Initialize post-London/Shanghai fields if active
	if config := chain.Config(); config != nil {
		if config.IsLondon(header.Number) && header.BaseFee == nil {
			header.BaseFee = eip1559.CalcBaseFee(config, parent)
		}
		if config.IsShanghai(header.Number, header.Time) && header.WithdrawalsHash == nil {
			header.WithdrawalsHash = &types.EmptyWithdrawalsHash
		}
		if config.IsCancun(header.Number, header.Time) {
			if header.BlobGasUsed == nil {
				var zero uint64
				header.BlobGasUsed = &zero
			}
			if header.ExcessBlobGas == nil {
				var zero uint64
				header.ExcessBlobGas = &zero
			}
			if header.ParentBeaconRoot == nil {
				header.ParentBeaconRoot = new(common.Hash)
			}
		}
		if config.IsPrague(header.Number, header.Time) && header.RequestsHash == nil {
			header.RequestsHash = &types.EmptyRequestsHash
		}
	}

	return nil
}

// Finalize runs any post-transaction state modifications (e.g. block rewards).
func (p *PoB) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state vm.StateDB, txs *[]*types.Transaction,
	uncles []*types.Header, withdrawals []*types.Withdrawal, receipts *[]*types.Receipt, systemTxs *[]*types.Transaction, usedGas *uint64, tracer *tracing.Hooks) error {
	p.trackActivity(header, txs)

	// Process any system transactions (e.g. block rewards).
	// PoB system txs are placed at the START of the block (index 0), so their
	// receipts must be prepended to preserve the TransactionIndex ordering.
	var systemReceipts []*types.Receipt
	var systemTxList []*types.Transaction
	for len(*systemTxs) > 0 {
		tx := (*systemTxs)[0]
		*systemTxs = (*systemTxs)[1:]
		if err := p.applyTransaction(tx, state, header, &systemTxList, &systemReceipts, usedGas, false, tracer); err != nil {
			return err
		}
	}
	if len(systemReceipts) > 0 {
		// Prepend system txs and their receipts so they align with block index 0.
		*txs = append(systemTxList, *txs...)
		*receipts = append(systemReceipts, *receipts...)
	}
	return nil
}

// FinalizeAndAssemble runs post-transaction state modifications and assembles
// the block.
func (p *PoB) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, body *types.Body, receipts []*types.Receipt, tracer *tracing.Hooks) (*types.Block, []*types.Receipt, error) {
	p.trackActivity(header, &body.Transactions)

	// In assembly mode, system transactions are already in body.Transactions.
	// We need to ensure they are applied to the state since they were skipped during the normal tx loop.
	var (
		txs     = make([]*types.Transaction, 0) // This will be filled by applyTransaction
		usedGas = header.GasUsed
	)
	
	// Process system transactions and collect their receipts separately.
	// PoB system txs are placed at the START of the block (index 0), so their
	// receipts must be prepended before user tx receipts to preserve TransactionIndex ordering.
	var systemReceipts []*types.Receipt
	
	for _, tx := range body.Transactions {
		if isSystem, _ := p.IsSystemTransaction(tx, header); isSystem {
			if err := p.applyTransaction(tx, state, header, &txs, &systemReceipts, &usedGas, true, tracer); err != nil {
				return nil, nil, err
			}
		}
	}
	// Prepend system receipts so they align with the block tx index (system txs come first).
	receipts = append(systemReceipts, receipts...)
	header.GasUsed = usedGas
	
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))

	// Ensure WithdrawalsHash is set if Shanghai is active by providing an empty slice if nil.
	if config := chain.Config(); config != nil {
		if config.IsShanghai(header.Number, header.Time) && body.Withdrawals == nil {
			body.Withdrawals = make([]*types.Withdrawal, 0)
		}
		// BlobGasUsed and ExcessBlobGas are usually set in Prepare, but we ensure they are 0 if Cancun is active
		// and not set.
		if config.IsCancun(header.Number, header.Time) {
			if header.BlobGasUsed == nil {
				var zero uint64
				header.BlobGasUsed = &zero
			}
			if header.ExcessBlobGas == nil {
				var zero uint64
				header.ExcessBlobGas = &zero
			}
			if header.ParentBeaconRoot == nil {
				header.ParentBeaconRoot = new(common.Hash)
			}
		}
		if config.IsPrague(header.Number, header.Time) && header.RequestsHash == nil {
			header.RequestsHash = &types.EmptyRequestsHash
		}
	}

	return types.NewBlock(header, body, receipts, trie.NewStackTrie(nil)), receipts, nil
}

func (p *PoB) applyTransaction(tx *types.Transaction, state vm.StateDB, header *types.Header, txs *[]*types.Transaction, receipts *[]*types.Receipt, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	// For PoB, system transactions are block rewards from PoBRewardAddress to Coinbase
	if to := tx.To(); to == nil || *to != header.Coinbase {
		return fmt.Errorf("invalid system transaction recipient: %v", to)
	}
	
	reward := tx.Value()
	
	// 1. Update state: add balance to coinbase
	state.AddBalance(header.Coinbase, uint256.MustFromBig(reward), tracing.BalanceIncreasePoBValidatorReward)
	
	// 2. Update nonce of system reward address
	state.SetNonce(params.PoBRewardAddress, tx.Nonce()+1, tracing.NonceChangePoBSystem)
	
	// 3. Update issued rewards tracking
	p.UpdateIssuedRewards(state, reward, header.Number.Uint64())
	
	// 4. Create receipt
	receipt := &types.Receipt{
		Type:              types.LegacyTxType,
		Status:            types.ReceiptStatusSuccessful,
		CumulativeGasUsed: *usedGas,
		Bloom:             types.Bloom{},
		TxHash:            tx.Hash(),
		GasUsed:           0,
		BlockNumber:       header.Number,
		BlockHash:         header.Hash(),
	}
	*receipts = append(*receipts, receipt)
	*txs = append(*txs, tx)
	
	return nil
}

func (p *PoB) addCandidate(addr common.Address) {
	if addr == (common.Address{}) {
		return
	}
	p.candLock.Lock()
	if _, ok := p.candidates[addr]; !ok {
		p.candidates[addr] = struct{}{}
		p.candLock.Unlock()
		p.saveCandidates()
		return
	}
	p.candLock.Unlock()
}

func (p *PoB) trackActivity(header *types.Header, txs *[]*types.Transaction) {
	p.candLock.Lock()
	changed := false

	// Always track the block proposer
	if header != nil {
		if _, ok := p.candidates[header.Coinbase]; !ok {
			p.candidates[header.Coinbase] = struct{}{}
			changed = true
		}
	}

	// Track transaction senders and recipients
	if txs != nil {
		for _, tx := range *txs {
			if sender, err := types.LatestSigner(p.chainConfig).Sender(tx); err == nil {
				if _, ok := p.candidates[sender]; !ok {
					p.candidates[sender] = struct{}{}
					changed = true
				}
			}
			if to := tx.To(); to != nil {
				if _, ok := p.candidates[*to]; !ok {
					p.candidates[*to] = struct{}{}
					changed = true
				}
			}
		}
	}
	p.candLock.Unlock()

	if changed {
		p.saveCandidates()
	}
}

func (p *PoB) accumulateRewards(state vm.StateDB, header *types.Header) {
	// No-op: rewards are handled via system transactions at the start of block processing.
}

var (
	TotalIssuedRewardsKey   = common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001")
	RewardDecayStopEpochKey = common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000002")
)

// CalculateReward computes the block reward for a given block number and state.
// It implements discrete 0.5% reduction per epoch until 70% of max supply is circulating.
func (p *PoB) CalculateReward(number uint64, state vm.StateDB) *big.Int {
	if p.config.RewardPerBlock == nil || p.config.RewardPerBlock.Sign() <= 0 {
		return common.Big0
	}

	// 1. Calculate Total Issued Rewards from system storage
	totalIssued := new(big.Int).SetBytes(state.GetState(params.PoBRewardAddress, TotalIssuedRewardsKey).Bytes())

	// 2. Check if we've reached MaxSupply
	genesisAlloc := p.config.GenesisAllocation
	if genesisAlloc == nil {
		genesisAlloc = common.Big0
	}

	totalCirculating := new(big.Int).Add(genesisAlloc, totalIssued)
	if totalCirculating.Cmp(p.config.MaxSupply) >= 0 {
		return common.Big0
	}

	// 3. Calculate current reward with decay
	epoch := number / p.config.Epoch
	reward := new(big.Int).Set(p.config.RewardPerBlock)

	// Determine effective epoch for reduction
	effectiveEpoch := epoch
	stopEpochHash := state.GetState(params.PoBRewardAddress, RewardDecayStopEpochKey)
	stopEpoch := new(big.Int).SetBytes(stopEpochHash.Bytes()).Uint64()
	if stopEpoch > 0 {
		effectiveEpoch = stopEpoch
	}

	// Apply 0.5% reduction per epoch iteratively
	for i := uint64(0); i < effectiveEpoch; i++ {
		// reward = reward * 995 / 1000
		reward.Mul(reward, big.NewInt(995))
		reward.Div(reward, big.NewInt(1000))
	}

	// 4. Ensure we don't exceed MaxSupply in this block
	remaining := new(big.Int).Sub(p.config.MaxSupply, totalCirculating)
	if reward.Cmp(remaining) > 0 {
		reward.Set(remaining)
	}

	return reward
}

// UpdateIssuedRewards updates the total issued rewards in the state.
func (p *PoB) UpdateIssuedRewards(state vm.StateDB, reward *big.Int, number uint64) {
	if reward.Sign() <= 0 {
		return
	}
	// Update total issued
	totalIssued := new(big.Int).SetBytes(state.GetState(params.PoBRewardAddress, TotalIssuedRewardsKey).Bytes())
	totalIssued.Add(totalIssued, reward)
	state.SetState(params.PoBRewardAddress, TotalIssuedRewardsKey, uint256.MustFromBig(totalIssued).Bytes32())

	// Check threshold to stop decay
	maxSupply := p.config.MaxSupply
	genesisAlloc := p.config.GenesisAllocation
	if genesisAlloc == nil {
		genesisAlloc = common.Big0
	}
	totalCirculating := new(big.Int).Add(genesisAlloc, totalIssued)
	threshold := new(big.Int).Div(new(big.Int).Mul(maxSupply, big.NewInt(70)), big.NewInt(100))

	if totalCirculating.Cmp(threshold) >= 0 {
		stopEpochHash := state.GetState(params.PoBRewardAddress, RewardDecayStopEpochKey)
		stopEpoch := new(big.Int).SetBytes(stopEpochHash.Bytes()).Uint64()
		if stopEpoch == 0 {
			epoch := number / p.config.Epoch
			state.SetState(params.PoBRewardAddress, RewardDecayStopEpochKey, uint256.NewInt(epoch).Bytes32())
		}
	}
}

// Seal signs the given block and pushes the result into the return channel.
func (p *PoB) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	header := block.Header()

	p.lock.RLock()
	signer, signFn := p.signer, p.signFn
	p.lock.RUnlock()

	if signer == (common.Address{}) {
		return errors.New("pob: signing not configured")
	}

	// Only sign if we are the expected proposer for this block.
	snap, err := p.snapshot(chain, header.Number.Uint64()-1, header.ParentHash, nil)
	if err != nil {
		return err
	}
	proposer := snap.Proposer(header.ParentHash, header.Number.Uint64())
	if signer != proposer {
		return errUnauthorizedSigner
	}

	// Wait until the block time is reached
	delay := time.Unix(int64(header.Time), 0).Sub(time.Now())
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-stop:
			return nil
		}
	}

	sighash, err := signFn(accounts.Account{Address: signer}, accounts.MimetypeDataWithValidator, PoBRLP(header, p.chainConfig.ChainID))
	if err != nil {
		log.Error("PoB sealing failed", "err", err)
		return err
	}
	header = types.CopyHeader(header)
	copy(header.Extra[len(header.Extra)-extraSeal:], sighash)

	select {
	case results <- block.WithSeal(header):
	default:
		log.Warn("PoB sealing result was not read by miner", "sealhash", SealHash(header, p.chainConfig.ChainID))
	}

	return nil
}

// SealHash returns the hash of a block prior to it being sealed.
func (p *PoB) SealHash(header *types.Header) common.Hash {
	return SealHash(header, p.chainConfig.ChainID)
}

// CalcDifficulty is the difficulty adjustment algorithm.
func (p *PoB) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	// Return 1 as a safe default; the snapshot-aware version is called
	// during full verification in VerifyHeader path.
	return big.NewInt(1)
}

// APIs implements consensus.Engine, returning the JSON-RPC APIs for PoB.
func (p *PoB) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return []rpc.API{{
		Namespace: "pob",
		Service:   &API{chain: chain, pob: p},
	}}
}

// Close terminates any background threads maintained by the consensus engine.
func (p *PoB) Close() error { return nil }

// Authorize injects a private key into the consensus engine to mint new blocks.
func (p *PoB) Authorize(signer common.Address, signFn SignerFn) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.signer = signer
	p.signFn = signFn
}

// ─── Validator pool selection ─────────────────────────────────────────────────

// Candidate represents an account eligible to join the mining pool.
type Candidate struct {
	Address common.Address
	Balance *big.Int
}

// BuildPool collects all accounts with balance ≥ MinBalanceWei from the state
// and returns the top PoolSize by descending balance.
func (p *PoB) BuildPool(state *state.StateDB, addresses []common.Address) []Candidate {
	var candidates []Candidate
	for _, addr := range addresses {
		bal := state.GetBalance(addr)
		if bal == nil {
			continue
		}
		minBal, _ := uint256.FromBig(p.config.MinBalanceWei)
		if bal.Cmp(minBal) >= 0 {
			candidates = append(candidates, Candidate{Address: addr, Balance: bal.ToBig()})
		}
	}
	// Sort descending by balance
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Balance.Cmp(candidates[j].Balance) > 0
	})
	if len(candidates) > p.config.PoolSize {
		candidates = candidates[:p.config.PoolSize]
	}
	return candidates
}

func (p *PoB) Delay(chain consensus.ChainReader, header *types.Header, leftOver *time.Duration) *time.Duration {
	d := time.Duration(p.config.Period) * time.Second
	return &d
}

// VerifyRequests verifies the consistency between Requests and header.RequestsHash.
func (p *PoB) VerifyRequests(header *types.Header, Requests [][]byte) error {
	return nil
}

// NextInTurnValidator return the next in-turn validator for header
func (p *PoB) NextInTurnValidator(chain consensus.ChainHeaderReader, header *types.Header) (common.Address, error) {
	return common.Address{}, nil
}

// SignBAL signs the BAL of the block
func (p *PoB) SignBAL(blockAccessList *types.BlockAccessListEncode) error {
	return nil
}

// VerifyBAL verifies the BAL of the block
func (p *PoB) VerifyBAL(block *types.Block, bal *types.BlockAccessListEncode) error {
	return nil
}

// SelectProposer picks a proposer from the pool using a weighted lottery seeded
// by the parent hash and block number. This is deterministic across all nodes.
//
//	weight_i = balance_i / total
func SelectProposer(pool []Candidate, parentHash common.Hash, blockNumber uint64) common.Address {
	if len(pool) == 0 {
		return common.Address{}
	}

	// Seed: first 8 bytes of keccak256(parentHash ++ blockNumber)
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, blockNumber)
	seed := crypto.Keccak256(append(parentHash.Bytes(), buf...))
	rng := rand.New(rand.NewSource(int64(binary.BigEndian.Uint64(seed[:8]))))

	// Total balance
	total := new(big.Int)
	for _, c := range pool {
		total.Add(total, c.Balance)
	}
	if total.Sign() == 0 {
		return pool[0].Address
	}

	// Pick a point in [0, total)
	point := new(big.Int).Rand(rng, total)

	// Walk the sorted pool accumulating balance until we pass the point
	acc := new(big.Int)
	for _, c := range pool {
		acc.Add(acc, c.Balance)
		if acc.Cmp(point) > 0 {
			return c.Address
		}
	}
	return pool[len(pool)-1].Address
}

// ProposerDifficulty returns the difficulty value for a block produced by a
// given candidate relative to the whole pool.
//
//	difficulty = (balance / totalBalance) × 1_000_000_000   (min = 1)
func ProposerDifficulty(candidate Candidate, pool []Candidate) *big.Int {
	total := new(big.Int)
	for _, c := range pool {
		total.Add(total, c.Balance)
	}
	if total.Sign() == 0 {
		return big.NewInt(1)
	}
	// difficulty = candidate.Balance * 1e9 / total
	scale := new(big.Int).SetUint64(1_000_000_000)
	diff := new(big.Int).Mul(candidate.Balance, scale)
	diff.Div(diff, total)
	if diff.Sign() == 0 {
		diff.SetInt64(1)
	}
	return diff
}

// ─── Crypto helpers ───────────────────────────────────────────────────────────

// SealHash returns the hash of a block prior to it being sealed.
func SealHash(header *types.Header, chainId *big.Int) (hash common.Hash) {
	hasher := newHasher()
	encodeSigHeader(hasher, header, chainId)
	hasher.Sum(hash[:0])
	return hash
}

// PoBRLP returns the rlp-encoded header bytes for signing.
func PoBRLP(header *types.Header, chainId *big.Int) []byte {
	b := new(bytes.Buffer)
	encodeSigHeader(b, header, chainId)
	return b.Bytes()
}

func encodeSigHeader(w interface{ Write([]byte) (int, error) }, header *types.Header, chainId *big.Int) {
	types.EncodeSigHeader(w, header, chainId)
}

// ecrecover extracts the Ethereum account address from a signed header.
func ecrecover(header *types.Header, sigcache *lru.ARCCache, chainId *big.Int) (common.Address, error) {
	// If the signature's already cached, return that
	hash := header.Hash()
	if val, ok := sigcache.Get(hash); ok {
		return val.(common.Address), nil
	}
	// Retrieve the signature from the header extra-data
	if len(header.Extra) < extraSeal {
		return common.Address{}, errMissingSignature
	}
	signature := header.Extra[len(header.Extra)-extraSeal:]

	// Recover the public key and the Ethereum address
	pubkey, err := crypto.Ecrecover(SealHash(header, chainId).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])

	sigcache.Add(hash, signer)
	return signer, nil
}

// newHasher returns a new crypto.KeccakState hasher.
func newHasher() crypto.KeccakState { return crypto.NewKeccakState() }
