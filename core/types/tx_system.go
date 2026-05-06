package types

import (
	"bytes"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

// SystemTxKind identifies the semantic type of a PoB system transaction.
type SystemTxKind uint8

const (
	SystemTxKindReward SystemTxKind = 1
	SystemTxKindBurn   SystemTxKind = 2
)

// SystemTx is a PoB-specific EIP-2718 typed transaction payload used for consensus
// system operations. It has an explicit From field and is not signed.
//
// Canonical hash preimage: 0x7e || rlp([chainId, from, nonce, to, value, data, kind])
type SystemTx struct {
	ChainID *big.Int      // destination chain ID
	From    common.Address // explicit sender (must be PoBRewardAddress by consensus)
	Nonce   uint64         // nonce of the system sender
	To      common.Address // recipient (e.g. coinbase, burn address)
	Value   *big.Int       // wei amount
	Data    []byte         // optional payload
	Kind    SystemTxKind   // semantic kind (reward/burn/...)
}

// copy creates a deep copy of the transaction data and initializes all fields.
func (tx *SystemTx) copy() TxData {
	cpy := &SystemTx{
		From:  tx.From,
		Nonce: tx.Nonce,
		To:    tx.To,
		Data:  common.CopyBytes(tx.Data),
		Kind:  tx.Kind,
		Value: new(big.Int),
		ChainID: new(big.Int),
	}
	if tx.Value != nil {
		cpy.Value.Set(tx.Value)
	}
	if tx.ChainID != nil {
		cpy.ChainID.Set(tx.ChainID)
	}
	return cpy
}

// accessors for innerTx.
func (tx *SystemTx) txType() byte           { return SystemTxType }
func (tx *SystemTx) chainID() *big.Int      { return tx.ChainID }
func (tx *SystemTx) accessList() AccessList { return nil }
func (tx *SystemTx) data() []byte           { return tx.Data }
func (tx *SystemTx) gas() uint64            { return 0 }
func (tx *SystemTx) gasPrice() *big.Int     { return common.Big0 }
func (tx *SystemTx) gasTipCap() *big.Int    { return common.Big0 }
func (tx *SystemTx) gasFeeCap() *big.Int    { return common.Big0 }
func (tx *SystemTx) value() *big.Int        { return tx.Value }
func (tx *SystemTx) nonce() uint64          { return tx.Nonce }
func (tx *SystemTx) to() *common.Address    { return &tx.To }

func (tx *SystemTx) effectiveGasPrice(dst *big.Int, baseFee *big.Int) *big.Int {
	return dst.Set(common.Big0)
}

func (tx *SystemTx) rawSignatureValues() (v, r, s *big.Int) {
	// No signature for system transactions.
	return common.Big0, common.Big0, common.Big0
}

func (tx *SystemTx) setSignatureValues(chainID, v, r, s *big.Int) {
	// System transactions are not signed.
}

func (tx *SystemTx) encode(b *bytes.Buffer) error {
	// Use explicit list encoding to avoid including any accidental fields.
	return rlp.Encode(b, []any{tx.ChainID, tx.From, tx.Nonce, tx.To, tx.Value, tx.Data, uint8(tx.Kind)})
}

func (tx *SystemTx) decode(input []byte) error {
	var dec struct {
		ChainID *big.Int
		From    common.Address
		Nonce   uint64
		To      common.Address
		Value   *big.Int
		Data    []byte
		Kind    uint8
	}
	if err := rlp.DecodeBytes(input, &dec); err != nil {
		return err
	}
	tx.ChainID = dec.ChainID
	tx.From = dec.From
	tx.Nonce = dec.Nonce
	tx.To = dec.To
	tx.Value = dec.Value
	tx.Data = dec.Data
	tx.Kind = SystemTxKind(dec.Kind)
	return nil
}

func (tx *SystemTx) sigHash(chainID *big.Int) common.Hash {
	// Not used for system transactions (no signature), but return a stable hash anyway.
	return prefixedRlpHash(SystemTxType, []any{chainID, tx.From, tx.Nonce, tx.To, tx.Value, tx.Data, uint8(tx.Kind)})
}

