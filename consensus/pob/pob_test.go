package pob

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestSelectProposer(t *testing.T) {
	pool := []Candidate{
		{Address: common.HexToAddress("0x1"), Balance: big.NewInt(100)},
		{Address: common.HexToAddress("0x2"), Balance: big.NewInt(200)},
		{Address: common.HexToAddress("0x3"), Balance: big.NewInt(700)},
	}

	parentHash := common.Hash{0xaa}
	counts := make(map[common.Address]int)
	iterations := 10000

	for i := 0; i < iterations; i++ {
		p := SelectProposer(pool, parentHash, uint64(i))
		counts[p]++
	}

	// 0x1 (10%) -> ~1000
	// 0x2 (20%) -> ~2000
	// 0x3 (70%) -> ~7000

	t.Logf("Counts: %v", counts)

	checkPct := func(addr common.Address, expectedPct float64) {
		got := float64(counts[addr]) / float64(iterations)
		if got < expectedPct-0.05 || got > expectedPct+0.05 {
			t.Errorf("Proposer %s: expected ~%v ratio, got %v", addr.Hex(), expectedPct, got)
		}
	}

	checkPct(common.HexToAddress("0x1"), 0.1)
	checkPct(common.HexToAddress("0x2"), 0.2)
	checkPct(common.HexToAddress("0x3"), 0.7)
}

func TestProposerDifficulty(t *testing.T) {
	pool := []Candidate{
		{Address: common.HexToAddress("0x1"), Balance: big.NewInt(100)},
		{Address: common.HexToAddress("0x2"), Balance: big.NewInt(200)},
		{Address: common.HexToAddress("0x3"), Balance: big.NewInt(700)},
	}

	d1 := ProposerDifficulty(pool[0], pool) // 10% -> 100,000,000
	d2 := ProposerDifficulty(pool[1], pool) // 20% -> 200,000,000
	d3 := ProposerDifficulty(pool[2], pool) // 70% -> 700,000,000

	if d1.Uint64() != 100000000 {
		t.Errorf("Expected difficulty 100000000, got %v", d1)
	}
	if d2.Uint64() != 200000000 {
		t.Errorf("Expected difficulty 200000000, got %v", d2)
	}
	if d3.Uint64() != 700000000 {
		t.Errorf("Expected difficulty 700000000, got %v", d3)
	}
}

func TestSealHash(t *testing.T) {
	header := &types.Header{
		ParentHash: common.Hash{0x1},
		Coinbase:   common.HexToAddress("0x1"),
		Number:     big.NewInt(10),
		Time:       1000,
		Extra:      append(make([]byte, extraVanity), make([]byte, extraSeal)...),
	}
	chainId := big.NewInt(1)
	
	h1 := SealHash(header, chainId)
	
	// Change vanity
	header.Extra[0] = 0xff
	h2 := SealHash(header, chainId)
	if h1 == h2 {
		t.Errorf("SealHash should depend on vanity")
	}

	// Change signature part (should NOT affect SealHash)
	header.Extra[len(header.Extra)-1] = 0xee
	h3 := SealHash(header, chainId)
	if h2 != h3 {
		t.Errorf("SealHash should NOT depend on signature suffix")
	}
}

func TestSystemRewardReceiptCumulativeGasUsedIsZero(t *testing.T) {
	coinbase := common.HexToAddress("0xd29Cb528C4d8Bc5890dCD505fE211E0F8Fefaf78")
	header := &types.Header{
		ParentHash: common.Hash{0x1},
		Coinbase:   coinbase,
		Number:     big.NewInt(73),
		Time:       1000,
		Extra:      append(make([]byte, extraVanity), make([]byte, extraSeal)...),
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    72,
		GasPrice: big.NewInt(0),
		Gas:      0,
		To:       &coinbase,
		Value:    big.NewInt(1),
		Data:     nil,
		V:        big.NewInt(0),
		R:        big.NewInt(0),
		S:        big.NewInt(0),
	})

	receipt := systemRewardReceipt(tx, header)
	if receipt.CumulativeGasUsed != 0 {
		t.Fatalf("expected system receipt cumulativeGasUsed=0, got %d", receipt.CumulativeGasUsed)
	}
	if receipt.GasUsed != 0 {
		t.Fatalf("expected system receipt gasUsed=0, got %d", receipt.GasUsed)
	}
	if receipt.TxHash != tx.Hash() {
		t.Fatalf("expected receipt TxHash=%s, got %s", tx.Hash(), receipt.TxHash)
	}
}

func TestIsSystemTransactionStrictShapeAndRecipients(t *testing.T) {
	chainCfg := &params.ChainConfig{
		ChainID:       big.NewInt(1),
		Pob:           &params.PoBConfig{Epoch: 200, PoolSize: 21, MinBalanceWei: big.NewInt(1), RewardPerBlock: big.NewInt(1), MaxSupply: big.NewInt(1000)},
		PobTraceBurn:  true,
	}
	engine := New(chainCfg.Pob, chainCfg, memorydb.New(), nil, common.Hash{})

	coinbase := common.HexToAddress("0xd29Cb528C4d8Bc5890dCD505fE211E0F8Fefaf78")
	header := &types.Header{Coinbase: coinbase, Number: big.NewInt(1)}

	reward := big.NewInt(1)
	rewardTx := types.NewTx(&types.LegacyTx{
		Nonce:    0,
		GasPrice: big.NewInt(0),
		Gas:      0,
		To:       &coinbase,
		Value:    reward,
		Data:     nil,
		V:        big.NewInt(0),
		R:        big.NewInt(0),
		S:        big.NewInt(0),
	})
	if ok, err := engine.IsSystemTransaction(rewardTx, header); !ok || err != nil {
		t.Fatalf("expected reward tx to be system tx, ok=%v err=%v", ok, err)
	}

	burnTo := params.PoBBurnAddress
	burnTx := types.NewTx(&types.LegacyTx{
		Nonce:    1,
		GasPrice: big.NewInt(0),
		Gas:      0,
		To:       &burnTo,
		Value:    big.NewInt(0),
		Data:     nil,
		V:        big.NewInt(0),
		R:        big.NewInt(0),
		S:        big.NewInt(0),
	})
	if ok, err := engine.IsSystemTransaction(burnTx, header); !ok || err != nil {
		t.Fatalf("expected burn tx to be system tx, ok=%v err=%v", ok, err)
	}

	// Unsigned tx with non-zero gasPrice must be rejected (not treated as system).
	bad := types.NewTx(&types.LegacyTx{
		Nonce:    2,
		GasPrice: big.NewInt(1),
		Gas:      0,
		To:       &coinbase,
		Value:    big.NewInt(0),
		Data:     nil,
		V:        big.NewInt(0),
		R:        big.NewInt(0),
		S:        big.NewInt(0),
	})
	if ok, err := engine.IsSystemTransaction(bad, header); err == nil || ok {
		t.Fatalf("expected invalid system tx to error, ok=%v err=%v", ok, err)
	}
}
