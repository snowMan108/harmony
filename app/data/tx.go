package data

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/vmihailenco/msgpack/v5"
)

// MemPoolTx - This is how tx is placed in mempool, after performing
// RPC call for fetching currently pending/ queued tx(s) in mempool
// it'll be destructured into this format, for further computation
type MemPoolTx struct {
	BlockHash        *common.Hash    `json:"blockHash"`
	BlockNumber      *hexutil.Big    `json:"blockNumber"`
	From             common.Address  `json:"from"`
	Gas              hexutil.Uint64  `json:"gas"`
	GasPrice         *hexutil.Big    `json:"gasPrice"`
	Hash             common.Hash     `json:"hash"`
	Input            hexutil.Bytes   `json:"input"`
	Nonce            hexutil.Uint64  `json:"nonce"`
	To               *common.Address `json:"to"`
	TransactionIndex *hexutil.Uint64 `json:"transactionIndex"`
	Value            *hexutil.Big    `json:"value"`
	Type             hexutil.Uint64  `json:"type"`
	ChainID          *hexutil.Big    `json:"chainId,omitempty"`
	V                *hexutil.Big    `json:"v"`
	R                *hexutil.Big    `json:"r"`
	S                *hexutil.Big    `json:"s"`
	PendingFrom      time.Time
	QueuedAt         time.Time
	Pool             string
}

// IsNonceExhausted - Multiple tx(s) of same/ different value
// can be sent to network with same nonce, where one of them
// which seems most profitable to miner, will be picked up, while mining next block
//
// This function will help us in checking whether nonce of this tx is exhausted or not
// i.e. whether some other tx is same nonce is mined or not
//
// If mined, we can drop this tx from mempool
func (m *MemPoolTx) IsNonceExhausted(ctx context.Context, rpc *rpc.Client) (bool, error) {

	var result hexutil.Uint64

	if err := rpc.CallContext(ctx, &result, "eth_getTransactionCount", m.From.Hex(), "latest"); err != nil {
		return false, err
	}

	return m.Nonce < result, nil

}

// IsUnstuck - Checking whether this tx is unstuck now
//
// @note Tx(s) generally get stuck in queued pool
// due to nonce gaps
func (m *MemPoolTx) IsUnstuck(ctx context.Context, rpc *rpc.Client) (bool, error) {

	var result hexutil.Uint64

	if err := rpc.CallContext(ctx, &result, "eth_getTransactionCount", m.From.Hex(), "latest"); err != nil {
		return false, err
	}

	return m.Nonce <= result, nil

}

// IsConfirmed - Checks whether this mempool tx is already
// included in any block or not
func (m *MemPoolTx) IsConfirmed(ctx context.Context, rpc *rpc.Client) (bool, error) {

	var result MemPoolTx

	if err := rpc.CallContext(ctx, &result, "eth_getTransactionByHash", m.Hash.Hex()); err != nil {
		return false, err
	}

	return true, nil

}

// ToMessagePack - Serialize to message pack encoded byte array format
func (m *MemPoolTx) ToMessagePack() ([]byte, error) {

	return msgpack.Marshal(m)

}

// FromMessagePack - Given serialized byte array, attempts to deserialize
// into structured format
func FromMessagePack(data []byte) (*MemPoolTx, error) {

	var tx *MemPoolTx

	if err := msgpack.Unmarshal(data, tx); err != nil {
		return nil, err
	}

	return tx, nil

}
