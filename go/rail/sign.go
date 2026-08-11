package rail

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The EIP-712 types the contract fixes. The three kinds share a field layout
// but not a type hash, so one identifier can never mean two kinds.
const (
	depositType  = "Deposit(bytes32 id,address payer,address account,uint256 amount,uint256 fee,address relayer,uint256 validBefore)"
	transferType = "Transfer(bytes32 id,address sender,address recipient,uint256 amount,uint256 fee,address relayer,uint256 validBefore)"
	withdrawType = "Withdraw(bytes32 id,address account,address destination,uint256 amount,uint256 fee,address relayer,uint256 validBefore)"

	eip712DomainType = "EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"
	authType         = "ReceiveWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)"

	railName    = "JuiceRail"
	railVersion = "2"
)

var (
	typBytes32 = mustType("bytes32")
	typAddress = mustType("address")
	typUint256 = mustType("uint256")

	// (typeHash, id, account, party, amount, fee, relayer, validBefore)
	termsArgs = abi.Arguments{
		{Type: typBytes32}, {Type: typBytes32}, {Type: typAddress}, {Type: typAddress},
		{Type: typUint256}, {Type: typUint256}, {Type: typAddress}, {Type: typUint256},
	}
	// (typeHash, name, version, chainId, verifyingContract)
	domainArgs = abi.Arguments{
		{Type: typBytes32}, {Type: typBytes32}, {Type: typBytes32}, {Type: typUint256}, {Type: typAddress},
	}
	// (typeHash, from, to, value, validAfter, validBefore, nonce)
	authArgs = abi.Arguments{
		{Type: typBytes32}, {Type: typAddress}, {Type: typAddress}, {Type: typUint256},
		{Type: typUint256}, {Type: typUint256}, {Type: typBytes32},
	}

	typeHashes = map[Kind]common.Hash{
		KindDeposit:  crypto.Keccak256Hash([]byte(depositType)),
		KindTransfer: crypto.Keccak256Hash([]byte(transferType)),
		KindWithdraw: crypto.Keccak256Hash([]byte(withdrawType)),
	}
	authTypeHash = crypto.Keccak256Hash([]byte(authType))
)

func mustType(s string) abi.Type {
	t, err := abi.NewType(s, "", nil)
	if err != nil {
		panic(fmt.Sprintf("rail: bad ABI type %q: %v", s, err))
	}
	return t
}

// DomainSeparator is the EIP-712 domain of one deployment. It binds the chain
// and the contract, so terms signed for one domain cannot execute on another.
func DomainSeparator(chainID *big.Int, railAddr common.Address) common.Hash {
	packed, err := domainArgs.Pack(
		crypto.Keccak256Hash([]byte(eip712DomainType)),
		crypto.Keccak256Hash([]byte(railName)),
		crypto.Keccak256Hash([]byte(railVersion)),
		chainID,
		railAddr,
	)
	if err != nil {
		panic(fmt.Sprintf("rail: encode domain: %v", err))
	}
	return crypto.Keccak256Hash(packed)
}

// TermsHash is the EIP-712 struct hash of a variant. The contract binds the
// identifier to exactly this value, and a deposit's token authorisation uses
// it as its nonce, which ties the two signatures together.
func TermsHash(v Variant) (common.Hash, error) {
	th, ok := typeHashes[v.Kind]
	if !ok {
		return common.Hash{}, fmt.Errorf("%w: unknown kind %d", errBadTerms, uint8(v.Kind))
	}
	packed, err := termsArgs.Pack(
		th, v.ID.Hash(), v.Account, v.Party, v.Amount, v.Fee, v.Relayer,
		new(big.Int).SetUint64(v.ValidBefore),
	)
	if err != nil {
		return common.Hash{}, fmt.Errorf("encode terms: %w", err)
	}
	return crypto.Keccak256Hash(packed), nil
}

// digest is what the account holder signs: the EIP-712 envelope over the
// domain and the struct hash.
func digest(domainSep, structHash common.Hash) common.Hash {
	return crypto.Keccak256Hash([]byte{0x19, 0x01}, domainSep.Bytes(), structHash.Bytes())
}

// authDigest is what the payer signs for the token: an EIP-3009 authorisation
// payable only to the rail, expiring with the operation itself.
func authDigest(tokenDomain common.Hash, from, to common.Address, value *big.Int, validBefore uint64, nonce common.Hash) (common.Hash, error) {
	packed, err := authArgs.Pack(
		authTypeHash, from, to, value, new(big.Int), new(big.Int).SetUint64(validBefore), nonce,
	)
	if err != nil {
		return common.Hash{}, fmt.Errorf("encode authorisation: %w", err)
	}
	return digest(tokenDomain, crypto.Keccak256Hash(packed)), nil
}

// signDigest produces the 65-byte signature the contracts expect.
func signDigest(key *ecdsa.PrivateKey, d common.Hash) ([]byte, error) {
	sig, err := crypto.Sign(d.Bytes(), key)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	// go-ethereum yields v in {0,1}; Ethereum signatures carry v in {27,28}.
	sig[64] += 27
	return sig, nil
}

// RecoverSigner returns the address whose key produced a signature. It is how
// a relayer checks a variant before spending gas on it.
func RecoverSigner(d common.Hash, sig []byte) (common.Address, error) {
	if len(sig) != 65 {
		return common.Address{}, fmt.Errorf("recover: signature is %d bytes, want 65", len(sig))
	}
	normalised := make([]byte, 65)
	copy(normalised, sig)
	if normalised[64] >= 27 {
		normalised[64] -= 27
	}
	pub, err := crypto.SigToPub(d.Bytes(), normalised)
	if err != nil {
		return common.Address{}, fmt.Errorf("recover: %w", err)
	}
	return crypto.PubkeyToAddress(*pub), nil
}
