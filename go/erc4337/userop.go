// Package erc4337 is the byte mechanics of account abstraction: operation
// packing and hashing, Safe account derivation, paymaster authorisation, and
// the bundler RPC client. It holds no policy and no durable state.
package erc4337

import (
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// UserOperation is an EntryPoint v0.7 operation in its unpacked form, which is
// what the bundler RPC speaks. Packing is applied for hashing and execution.
type UserOperation struct {
	Sender               common.Address
	Nonce                *big.Int
	Factory              *common.Address
	FactoryData          []byte
	CallData             []byte
	CallGasLimit         *big.Int
	VerificationGasLimit *big.Int
	PreVerificationGas   *big.Int
	MaxFeePerGas         *big.Int
	MaxPriorityFeePerGas *big.Int

	Paymaster                     *common.Address
	PaymasterVerificationGasLimit *big.Int
	PaymasterPostOpGasLimit       *big.Int
	PaymasterData                 []byte

	Signature []byte
}

// InitCode is the packed factory call, empty when the account already exists.
func (op *UserOperation) InitCode() []byte {
	if op.Factory == nil {
		return nil
	}
	return append(op.Factory.Bytes(), op.FactoryData...)
}

// AccountGasLimits packs the verification and call gas limits.
func (op *UserOperation) AccountGasLimits() [32]byte {
	return packPair(op.VerificationGasLimit, op.CallGasLimit)
}

// GasFees packs the priority fee and the fee cap.
func (op *UserOperation) GasFees() [32]byte {
	return packPair(op.MaxPriorityFeePerGas, op.MaxFeePerGas)
}

// PaymasterAndData is the packed sponsorship field: paymaster, its two gas
// limits, then paymaster-specific data.
func (op *UserOperation) PaymasterAndData() []byte {
	if op.Paymaster == nil {
		return nil
	}
	out := make([]byte, 0, 52+len(op.PaymasterData))
	out = append(out, op.Paymaster.Bytes()...)
	out = append(out, uint128Bytes(op.PaymasterVerificationGasLimit)...)
	out = append(out, uint128Bytes(op.PaymasterPostOpGasLimit)...)
	return append(out, op.PaymasterData...)
}

// Hash is the identifier the EntryPoint and the bundler use for this operation.
func (op *UserOperation) Hash(entryPoint common.Address, chainID *big.Int) common.Hash {
	accountGasLimits := op.AccountGasLimits()
	gasFees := op.GasFees()

	inner := crypto.Keccak256(concat(
		word(new(big.Int).SetBytes(op.Sender.Bytes())),
		word(op.Nonce),
		crypto.Keccak256(op.InitCode()),
		crypto.Keccak256(op.CallData),
		accountGasLimits[:],
		word(op.PreVerificationGas),
		gasFees[:],
		crypto.Keccak256(op.PaymasterAndData()),
	))
	return crypto.Keccak256Hash(concat(
		inner,
		word(new(big.Int).SetBytes(entryPoint.Bytes())),
		word(chainID),
	))
}

// Packed is the on-chain PackedUserOperation tuple, for handleOps.
type Packed struct {
	Sender             common.Address
	Nonce              *big.Int
	InitCode           []byte
	CallData           []byte
	AccountGasLimits   [32]byte
	PreVerificationGas *big.Int
	GasFees            [32]byte
	PaymasterAndData   []byte
	Signature          []byte
}

// Pack converts to the on-chain representation.
func (op *UserOperation) Pack() Packed {
	return Packed{
		Sender:             op.Sender,
		Nonce:              op.Nonce,
		InitCode:           op.InitCode(),
		CallData:           op.CallData,
		AccountGasLimits:   op.AccountGasLimits(),
		PreVerificationGas: op.PreVerificationGas,
		GasFees:            op.GasFees(),
		PaymasterAndData:   op.PaymasterAndData(),
		Signature:          op.Signature,
	}
}

// rpcUserOperation is the wire shape of an EntryPoint v0.7 operation.
type rpcUserOperation struct {
	Sender               common.Address  `json:"sender"`
	Nonce                string          `json:"nonce"`
	Factory              *common.Address `json:"factory,omitempty"`
	FactoryData          string          `json:"factoryData,omitempty"`
	CallData             string          `json:"callData"`
	CallGasLimit         string          `json:"callGasLimit"`
	VerificationGasLimit string          `json:"verificationGasLimit"`
	PreVerificationGas   string          `json:"preVerificationGas"`
	MaxFeePerGas         string          `json:"maxFeePerGas"`
	MaxPriorityFeePerGas string          `json:"maxPriorityFeePerGas"`

	Paymaster                     *common.Address `json:"paymaster,omitempty"`
	PaymasterVerificationGasLimit string          `json:"paymasterVerificationGasLimit,omitempty"`
	PaymasterPostOpGasLimit       string          `json:"paymasterPostOpGasLimit,omitempty"`
	PaymasterData                 string          `json:"paymasterData,omitempty"`

	Signature string `json:"signature"`
}

func (op UserOperation) MarshalJSON() ([]byte, error) {
	r := rpcUserOperation{
		Sender:               op.Sender,
		Nonce:                hexBig(op.Nonce),
		Factory:              op.Factory,
		CallData:             hexutil.Encode(op.CallData),
		CallGasLimit:         hexBig(op.CallGasLimit),
		VerificationGasLimit: hexBig(op.VerificationGasLimit),
		PreVerificationGas:   hexBig(op.PreVerificationGas),
		MaxFeePerGas:         hexBig(op.MaxFeePerGas),
		MaxPriorityFeePerGas: hexBig(op.MaxPriorityFeePerGas),
		Signature:            hexutil.Encode(op.Signature),
	}
	if op.Factory != nil {
		r.FactoryData = hexutil.Encode(op.FactoryData)
	}
	if op.Paymaster != nil {
		r.Paymaster = op.Paymaster
		r.PaymasterVerificationGasLimit = hexBig(op.PaymasterVerificationGasLimit)
		r.PaymasterPostOpGasLimit = hexBig(op.PaymasterPostOpGasLimit)
		r.PaymasterData = hexutil.Encode(op.PaymasterData)
	}
	return json.Marshal(r)
}

func (op *UserOperation) UnmarshalJSON(b []byte) error {
	var r rpcUserOperation
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	var err error
	set := func(dst **big.Int, s string, name string) {
		if err != nil {
			return
		}
		v, e := parseBig(s)
		if e != nil {
			err = fmt.Errorf("%s: %w", name, e)
			return
		}
		*dst = v
	}
	setBytes := func(dst *[]byte, s string, name string) {
		if err != nil || s == "" {
			return
		}
		v, e := hexutil.Decode(s)
		if e != nil {
			err = fmt.Errorf("%s: %w", name, e)
			return
		}
		*dst = v
	}

	op.Sender = r.Sender
	op.Factory = r.Factory
	op.Paymaster = r.Paymaster
	set(&op.Nonce, r.Nonce, "nonce")
	set(&op.CallGasLimit, r.CallGasLimit, "callGasLimit")
	set(&op.VerificationGasLimit, r.VerificationGasLimit, "verificationGasLimit")
	set(&op.PreVerificationGas, r.PreVerificationGas, "preVerificationGas")
	set(&op.MaxFeePerGas, r.MaxFeePerGas, "maxFeePerGas")
	set(&op.MaxPriorityFeePerGas, r.MaxPriorityFeePerGas, "maxPriorityFeePerGas")
	setBytes(&op.CallData, r.CallData, "callData")
	setBytes(&op.FactoryData, r.FactoryData, "factoryData")
	setBytes(&op.Signature, r.Signature, "signature")
	setBytes(&op.PaymasterData, r.PaymasterData, "paymasterData")
	if r.Paymaster != nil {
		set(&op.PaymasterVerificationGasLimit, r.PaymasterVerificationGasLimit, "paymasterVerificationGasLimit")
		set(&op.PaymasterPostOpGasLimit, r.PaymasterPostOpGasLimit, "paymasterPostOpGasLimit")
	}
	return err
}

func parseBig(s string) (*big.Int, error) {
	if s == "" {
		return new(big.Int), nil
	}
	v, err := hexutil.DecodeBig(s)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func hexBig(v *big.Int) string {
	if v == nil {
		return "0x0"
	}
	return hexutil.EncodeBig(v)
}

// packPair puts high into the top 16 bytes and low into the bottom 16.
func packPair(high, low *big.Int) [32]byte {
	var out [32]byte
	copy(out[0:16], uint128Bytes(high))
	copy(out[16:32], uint128Bytes(low))
	return out
}

func uint128Bytes(v *big.Int) []byte {
	var b [16]byte
	if v != nil {
		v.FillBytes(b[:])
	}
	return b[:]
}

func word(v *big.Int) []byte {
	var w [32]byte
	if v != nil {
		v.FillBytes(w[:])
	}
	return w[:]
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
