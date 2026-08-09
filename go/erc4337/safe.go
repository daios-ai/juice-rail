package erc4337

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Contracts pins the canonical account-abstraction deployment of one domain.
type Contracts struct {
	EntryPoint        common.Address
	SafeSingleton     common.Address
	SafeProxyFactory  common.Address
	SafeModule        common.Address // Safe4337Module: module and fallback handler
	SafeModuleSetup   common.Address
	MultiSendCallOnly common.Address
}

// Account is a counterfactual 1-of-1 Safe controlled by one rail key. Its
// address is derivable before deployment, so tokens may arrive first.
type Account struct {
	Owner     common.Address
	Contracts Contracts
	SaltNonce *big.Int
}

const (
	safeABIJSON = `[
      {"type":"function","name":"setup","inputs":[
        {"name":"_owners","type":"address[]"},{"name":"_threshold","type":"uint256"},
        {"name":"to","type":"address"},{"name":"data","type":"bytes"},
        {"name":"fallbackHandler","type":"address"},{"name":"paymentToken","type":"address"},
        {"name":"payment","type":"uint256"},{"name":"paymentReceiver","type":"address"}]}]`

	proxyFactoryABIJSON = `[
      {"type":"function","name":"createProxyWithNonce","inputs":[
        {"name":"_singleton","type":"address"},{"name":"initializer","type":"bytes"},
        {"name":"saltNonce","type":"uint256"}],"outputs":[{"type":"address"}]},
      {"type":"function","name":"proxyCreationCode","inputs":[],"outputs":[{"type":"bytes"}],"stateMutability":"pure"}]`

	moduleSetupABIJSON = `[
      {"type":"function","name":"enableModules","inputs":[{"name":"modules","type":"address[]"}]}]`

	entryPointABIJSON = `[
      {"type":"function","name":"getNonce","inputs":[
        {"name":"sender","type":"address"},{"name":"key","type":"uint192"}],
        "outputs":[{"type":"uint256"}],"stateMutability":"view"}]`
)

var (
	safeABI         = mustABI(safeABIJSON)
	proxyFactoryABI = mustABI(proxyFactoryABIJSON)
	moduleSetupABI  = mustABI(moduleSetupABIJSON)
	entryPointABI   = mustABI(entryPointABIJSON)
)

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic(fmt.Sprintf("erc4337: bad ABI: %v", err))
	}
	return a
}

// Initializer is the Safe.setup call that makes the account a 4337 account:
// one owner, threshold one, the module both enabled and installed as the
// fallback handler.
func (a Account) Initializer() ([]byte, error) {
	enable, err := moduleSetupABI.Pack("enableModules", []common.Address{a.Contracts.SafeModule})
	if err != nil {
		return nil, fmt.Errorf("enableModules: %w", err)
	}
	return safeABI.Pack("setup",
		[]common.Address{a.Owner},
		big.NewInt(1),
		a.Contracts.SafeModuleSetup,
		enable,
		a.Contracts.SafeModule,
		common.Address{},
		big.NewInt(0),
		common.Address{},
	)
}

// FactoryData is the proxy-factory call that deploys this account.
func (a Account) FactoryData() ([]byte, error) {
	init, err := a.Initializer()
	if err != nil {
		return nil, err
	}
	return proxyFactoryABI.Pack("createProxyWithNonce", a.Contracts.SafeSingleton, init, a.salt())
}

func (a Account) salt() *big.Int {
	if a.SaltNonce == nil {
		return big.NewInt(0)
	}
	return a.SaltNonce
}

// Address derives the counterfactual address. The proxy creation code is read
// from the deployed factory, so the derivation follows the factory actually in
// use rather than a vendored copy of it.
func (a Account) Address(ctx context.Context, chain ContractCaller) (common.Address, error) {
	init, err := a.Initializer()
	if err != nil {
		return common.Address{}, err
	}
	creationCode, err := a.proxyCreationCode(ctx, chain)
	if err != nil {
		return common.Address{}, err
	}

	// SafeProxyFactory: salt = keccak256(keccak256(initializer) ++ saltNonce)
	var saltNonce [32]byte
	a.salt().FillBytes(saltNonce[:])
	salt := crypto.Keccak256Hash(concat(crypto.Keccak256(init), saltNonce[:]))

	// deployment code = proxy creation code ++ abi.encode(singleton)
	var singleton [32]byte
	copy(singleton[12:], a.Contracts.SafeSingleton.Bytes())
	initHash := crypto.Keccak256Hash(concat(creationCode, singleton[:]))

	return crypto.CreateAddress2(a.Contracts.SafeProxyFactory, salt, initHash.Bytes()), nil
}

func (a Account) proxyCreationCode(ctx context.Context, chain ContractCaller) ([]byte, error) {
	in, err := proxyFactoryABI.Pack("proxyCreationCode")
	if err != nil {
		return nil, err
	}
	to := a.Contracts.SafeProxyFactory
	out, err := chain.CallContract(ctx, ethereum.CallMsg{To: &to, Data: in}, nil)
	if err != nil {
		return nil, fmt.Errorf("proxyCreationCode: %w", err)
	}
	values, err := proxyFactoryABI.Unpack("proxyCreationCode", out)
	if err != nil || len(values) != 1 {
		return nil, fmt.Errorf("proxyCreationCode: decode: %w", err)
	}
	code, ok := values[0].([]byte)
	if !ok {
		return nil, fmt.Errorf("proxyCreationCode: unexpected type")
	}
	return code, nil
}

// ContractCaller is the read access the account needs.
type ContractCaller interface {
	CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	CodeAt(ctx context.Context, account common.Address, blockNumber *big.Int) ([]byte, error)
}

// Deployed reports whether the account exists on chain yet.
func (a Account) Deployed(ctx context.Context, chain ContractCaller, addr common.Address) (bool, error) {
	code, err := chain.CodeAt(ctx, addr, nil)
	if err != nil {
		return false, fmt.Errorf("code at %s: %w", addr, err)
	}
	return len(code) > 0, nil
}

// Nonce reads the next sequence for a nonce key. Each intent uses its own key,
// so one stuck operation never blocks another intent.
func (a Account) Nonce(ctx context.Context, chain ContractCaller, addr common.Address, key *big.Int) (*big.Int, error) {
	in, err := entryPointABI.Pack("getNonce", addr, key)
	if err != nil {
		return nil, err
	}
	to := a.Contracts.EntryPoint
	out, err := chain.CallContract(ctx, ethereum.CallMsg{To: &to, Data: in}, nil)
	if err != nil {
		return nil, fmt.Errorf("getNonce: %w", err)
	}
	if len(out) != 32 {
		return nil, fmt.Errorf("getNonce: want 32 bytes, got %d", len(out))
	}
	return new(big.Int).SetBytes(out), nil
}

// --- Safe operation signing (Safe4337Module v0.3.0) ---

// safeOpTypeHash is keccak256 of the SafeOp type string; see Safe4337Module.
var safeOpTypeHash = crypto.Keccak256Hash([]byte(
	"SafeOp(address safe,uint256 nonce,bytes initCode,bytes callData,uint128 verificationGasLimit," +
		"uint128 callGasLimit,uint256 preVerificationGas,uint128 maxPriorityFeePerGas,uint128 maxFeePerGas," +
		"bytes paymasterAndData,uint48 validAfter,uint48 validUntil,address entryPoint)"))

var domainSeparatorTypeHash = crypto.Keccak256Hash([]byte(
	"EIP712Domain(uint256 chainId,address verifyingContract)"))

// SafeOpDigest is the EIP-712 digest the account owner signs.
func SafeOpDigest(op *UserOperation, c Contracts, chainID *big.Int, validAfter, validUntil uint64) common.Hash {
	domain := crypto.Keccak256Hash(concat(
		domainSeparatorTypeHash.Bytes(),
		word(chainID),
		word(new(big.Int).SetBytes(c.SafeModule.Bytes())),
	))
	structHash := crypto.Keccak256Hash(concat(
		safeOpTypeHash.Bytes(),
		word(new(big.Int).SetBytes(op.Sender.Bytes())),
		word(op.Nonce),
		crypto.Keccak256(op.InitCode()),
		crypto.Keccak256(op.CallData),
		word(op.VerificationGasLimit),
		word(op.CallGasLimit),
		word(op.PreVerificationGas),
		word(op.MaxPriorityFeePerGas),
		word(op.MaxFeePerGas),
		crypto.Keccak256(op.PaymasterAndData()),
		word(new(big.Int).SetUint64(validAfter)),
		word(new(big.Int).SetUint64(validUntil)),
		word(new(big.Int).SetBytes(c.EntryPoint.Bytes())),
	))
	return crypto.Keccak256Hash(concat([]byte{0x19, 0x01}, domain.Bytes(), structHash.Bytes()))
}

// SignSafeOp sets op.Signature to the module's expected encoding:
// validAfter ++ validUntil ++ owner signature.
func SignSafeOp(op *UserOperation, c Contracts, chainID *big.Int, validAfter, validUntil uint64, key *ecdsa.PrivateKey) error {
	digest := SafeOpDigest(op, c, chainID, validAfter, validUntil)
	sig, err := crypto.Sign(digest.Bytes(), key)
	if err != nil {
		return fmt.Errorf("sign safe operation: %w", err)
	}
	// Safe expects v as 27/28 for a plain owner signature.
	sig[64] += 27

	out := make([]byte, 0, 12+len(sig))
	out = append(out, uint48Bytes(validAfter)...)
	out = append(out, uint48Bytes(validUntil)...)
	op.Signature = append(out, sig...)
	return nil
}

func uint48Bytes(v uint64) []byte {
	var b [6]byte
	new(big.Int).SetUint64(v).FillBytes(b[:])
	return b[:]
}
