package erc4337

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// PaymasterDataLen is validUntil (6) ++ maxGasCost (32) ++ signature (65).
const PaymasterDataLen = 103

// PaymasterDigest is the authorisation the paymaster signer produces. It
// covers the operation with both signature fields excluded, bound to the
// domain (chain id, entry point) and to this authorisation (expiry, ceiling).
//
// It must reproduce JuiceRailPaymaster.getHash exactly.
func PaymasterDigest(
	op *UserOperation,
	paymaster, entryPoint common.Address,
	chainID *big.Int,
	validUntil uint64,
	maxGasCost *big.Int,
) common.Hash {
	accountGasLimits := op.AccountGasLimits()
	gasFees := op.GasFees()

	// The two paymaster gas limits, read as one word: paymasterAndData[20:52].
	var pmGasLimits [32]byte
	copy(pmGasLimits[0:16], uint128Bytes(op.PaymasterVerificationGasLimit))
	copy(pmGasLimits[16:32], uint128Bytes(op.PaymasterPostOpGasLimit))

	return crypto.Keccak256Hash(concat(
		word(new(big.Int).SetBytes(op.Sender.Bytes())),
		word(op.Nonce),
		crypto.Keccak256(op.InitCode()),
		crypto.Keccak256(op.CallData),
		accountGasLimits[:],
		pmGasLimits[:],
		word(op.PreVerificationGas),
		gasFees[:],
		word(chainID),
		word(new(big.Int).SetBytes(paymaster.Bytes())),
		word(new(big.Int).SetBytes(entryPoint.Bytes())),
		word(new(big.Int).SetUint64(validUntil)),
		word(maxGasCost),
	))
}

// SignPaymasterData produces the paymaster field for an operation. It must be
// applied before the account signs, because the account signature covers it.
func SignPaymasterData(
	op *UserOperation,
	paymaster, entryPoint common.Address,
	chainID *big.Int,
	validUntil uint64,
	maxGasCost *big.Int,
	key *ecdsa.PrivateKey,
) error {
	digest := PaymasterDigest(op, paymaster, entryPoint, chainID, validUntil, maxGasCost)
	sig, err := crypto.Sign(accounts.TextHash(digest.Bytes()), key)
	if err != nil {
		return fmt.Errorf("sign paymaster authorisation: %w", err)
	}
	sig[64] += 27 // ECDSA.recover expects 27/28

	data := make([]byte, 0, PaymasterDataLen)
	data = append(data, uint48Bytes(validUntil)...)
	data = append(data, word(maxGasCost)...)
	op.PaymasterData = append(data, sig...)
	op.Paymaster = &paymaster
	return nil
}
