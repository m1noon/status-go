package transactions

import (
	"bytes"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/status-im/status-go/geth/rpc"
)

// ErrInvalidSendTxArgs is returned when the structure of SendTxArgs is ambigious.
var ErrInvalidSendTxArgs = errors.New("Transaction arguments are invalid (are both 'input' and 'data' fields used?)")

// SendTxArgs represents the arguments to submit a new transaction into the transaction pool.
// This struct is based on go-ethereum's type in internal/ethapi/api.go, but we have freedom
// over the exact layout of this struct.
type SendTxArgs struct {
	From     common.Address  `json:"from"`
	To       *common.Address `json:"to"`
	Gas      *hexutil.Uint64 `json:"gas"`
	GasPrice *hexutil.Big    `json:"gasPrice"`
	Value    *hexutil.Big    `json:"value"`
	Nonce    *hexutil.Uint64 `json:"nonce"`
	// We keep both "input" and "data" for backward compatibility.
	// "input" is a preferred field.
	// see `vendor/github.com/ethereum/go-ethereum/internal/ethapi/api.go:1107`
	Input hexutil.Bytes `json:"input"`
	Data  hexutil.Bytes `json:"data"`
}

// Valid checks whether this structure is filled in correctly.
func (args SendTxArgs) Valid() bool {
	// if at least one of the fields is empty, it is a valid struct
	if isNilOrEmpty(args.Input) || isNilOrEmpty(args.Data) {
		return true
	}

	// we only allow both fields to present if they have the same data
	return bytes.Equal(args.Input, args.Data)
}

// GetInput returns either Input or Data field's value dependent on what is filled.
func (args SendTxArgs) GetInput() hexutil.Bytes {
	if !isNilOrEmpty(args.Input) {
		return args.Input
	}

	return args.Data
}

func isNilOrEmpty(bytes hexutil.Bytes) bool {
	return bytes == nil || len(bytes) == 0
}

// RPCCalltoSendTxArgs creates SendTxArgs based on RPC parameters
func RPCCalltoSendTxArgs(args ...interface{}) SendTxArgs {
	var err error
	var fromAddr, toAddr common.Address

	rpcCall := rpc.Call{Params: args}
	fromAddr, err = rpcCall.ParseFromAddress()
	if err != nil {
		fromAddr = common.HexToAddress("0x0")
	}

	toAddr, err = rpcCall.ParseToAddress()
	if err != nil {
		toAddr = common.HexToAddress("0x0")
	}

	input := rpcCall.ParseInput()
	data := rpcCall.ParseData()
	return SendTxArgs{
		To:       &toAddr,
		From:     fromAddr,
		Value:    rpcCall.ParseValue(),
		Input:    input,
		Data:     data,
		Gas:      rpcCall.ParseGas(),
		GasPrice: rpcCall.ParseGasPrice(),
	}
}
