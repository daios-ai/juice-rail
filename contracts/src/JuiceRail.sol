// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {EIP712} from "@openzeppelin/contracts/utils/cryptography/EIP712.sol";
import {ECDSA} from "@openzeppelin/contracts/utils/cryptography/ECDSA.sol";

/// @notice The signed token authorisation used for deposits. Only the payee
///         form is used: it can execute nowhere but inside a deposit.
interface IERC3009 {
    function receiveWithAuthorization(
        address from,
        address to,
        uint256 value,
        uint256 validAfter,
        uint256 validBefore,
        bytes32 nonce,
        uint8 v,
        bytes32 r,
        bytes32 s
    ) external;
}

/// @title JuiceRail
/// @notice Non-upgradeable, adminless money rail. One deployment is one domain,
///         identified by (chain id, contract address).
/// @dev Three money transitions, each authorised by the debited account's
///      EIP-712 signature and carried by the relayer it names. The binding of
///      an identifier to its terms is write-once and per account, so no other
///      account can bind or burn an identifier. There is no stored liability
///      total: solvency (held >= sum of balances) is checkable off-chain from
///      events.
contract JuiceRail is EIP712 {
    using SafeERC20 for IERC20;

    /// @dev The terms of one operation. The three kinds share this shape but
    ///      not their type hash, so an identifier can never mean two intents.
    ///      `validBefore` is the single deadline, and is also the deadline of
    ///      the token authorisation a deposit carries.
    struct DepositTerms {
        bytes32 id;
        address payer;
        address account;
        uint256 amount;
        uint256 fee;
        address relayer;
        uint256 validBefore;
    }

    struct TransferTerms {
        bytes32 id;
        address sender;
        address recipient;
        uint256 amount;
        uint256 fee;
        address relayer;
        uint256 validBefore;
    }

    struct WithdrawTerms {
        bytes32 id;
        address account;
        address destination;
        uint256 amount;
        uint256 fee;
        address relayer;
        uint256 validBefore;
    }

    bytes32 public constant DEPOSIT_TYPEHASH = keccak256(
        "Deposit(bytes32 id,address payer,address account,uint256 amount,uint256 fee,address relayer,uint256 validBefore)"
    );
    bytes32 public constant TRANSFER_TYPEHASH = keccak256(
        "Transfer(bytes32 id,address sender,address recipient,uint256 amount,uint256 fee,address relayer,uint256 validBefore)"
    );
    bytes32 public constant WITHDRAW_TYPEHASH = keccak256(
        "Withdraw(bytes32 id,address account,address destination,uint256 amount,uint256 fee,address relayer,uint256 validBefore)"
    );

    IERC20 public immutable token;

    mapping(address account => uint256) public balanceOf;
    mapping(address account => mapping(bytes32 id => bytes32 termsHash)) public operations;

    event Deposited(
        address indexed payer,
        bytes32 indexed id,
        address account,
        uint256 amount,
        uint256 fee,
        address relayer,
        uint256 validBefore
    );
    event Transferred(
        address indexed sender,
        bytes32 indexed id,
        address recipient,
        uint256 amount,
        uint256 fee,
        address relayer,
        uint256 validBefore
    );
    event Withdrawn(
        address indexed account,
        bytes32 indexed id,
        address destination,
        uint256 amount,
        uint256 fee,
        address relayer,
        uint256 validBefore
    );

    /// @notice The account's identifier is already bound to different terms.
    error TermsConflict(address account, bytes32 id);
    /// @notice Only the relayer named in the signed terms may carry them.
    error WrongRelayer();
    /// @notice The deadline in the signed terms has passed.
    error Expired();
    /// @notice The terms were not signed by the account they debit.
    error BadSignature();
    /// @notice The account's balance does not cover amount + fee.
    error InsufficientBalance();
    /// @notice The token did not move exactly amount + fee into this contract.
    error InexactTransfer();
    /// @notice A signature is not 65 bytes.
    error MalformedSignature();

    constructor(IERC20 asset) EIP712("JuiceRail", "2") {
        token = asset;
    }

    /// @notice Pull `amount + fee` from the payer and credit the account.
    /// @dev The payer signs twice: these terms, and the token authorisation
    ///      whose nonce is this operation's terms hash, which ties the two
    ///      together. The payer's address never determines who is credited.
    function deposit(DepositTerms calldata t, bytes calldata termsSig, bytes calldata authSig) external {
        bytes32 h = depositHash(t);
        if (!_bind(t.payer, t.id, h)) return;
        _authorise(t.payer, h, termsSig, t.relayer, t.validBefore);

        _pull(t.payer, t.amount + t.fee, t.validBefore, h, authSig);

        balanceOf[t.account] += t.amount;
        balanceOf[t.relayer] += t.fee;

        emit Deposited(t.payer, t.id, t.account, t.amount, t.fee, t.relayer, t.validBefore);
    }

    /// @notice Move `amount` from the sender to the recipient, and `fee` to the
    ///         relayer.
    /// @dev The sum of balances is preserved: an account debits only itself.
    function transfer(TransferTerms calldata t, bytes calldata sig) external {
        bytes32 h = transferHash(t);
        if (!_bind(t.sender, t.id, h)) return;
        _authorise(t.sender, h, sig, t.relayer, t.validBefore);

        uint256 total = t.amount + t.fee;
        uint256 balance = balanceOf[t.sender];
        if (balance < total) revert InsufficientBalance();
        unchecked {
            balanceOf[t.sender] = balance - total;
        }
        balanceOf[t.recipient] += t.amount;
        balanceOf[t.relayer] += t.fee;

        emit Transferred(t.sender, t.id, t.recipient, t.amount, t.fee, t.relayer, t.validBefore);
    }

    /// @notice Debit the account and send `amount` of the token to the signed
    ///         destination; the fee stays inside as the relayer's balance.
    function withdraw(WithdrawTerms calldata t, bytes calldata sig) external {
        bytes32 h = withdrawHash(t);
        if (!_bind(t.account, t.id, h)) return;
        _authorise(t.account, h, sig, t.relayer, t.validBefore);

        uint256 total = t.amount + t.fee;
        uint256 balance = balanceOf[t.account];
        if (balance < total) revert InsufficientBalance();
        unchecked {
            balanceOf[t.account] = balance - total;
        }
        balanceOf[t.relayer] += t.fee;

        emit Withdrawn(t.account, t.id, t.destination, t.amount, t.fee, t.relayer, t.validBefore);
        token.safeTransfer(t.destination, t.amount);
    }

    /// @notice The terms hash bound to the identifier, and the nonce of the
    ///         token authorisation a deposit carries.
    function depositHash(DepositTerms calldata t) public pure returns (bytes32) {
        return keccak256(
            abi.encode(DEPOSIT_TYPEHASH, t.id, t.payer, t.account, t.amount, t.fee, t.relayer, t.validBefore)
        );
    }

    function transferHash(TransferTerms calldata t) public pure returns (bytes32) {
        return keccak256(
            abi.encode(TRANSFER_TYPEHASH, t.id, t.sender, t.recipient, t.amount, t.fee, t.relayer, t.validBefore)
        );
    }

    function withdrawHash(WithdrawTerms calldata t) public pure returns (bytes32) {
        return keccak256(
            abi.encode(WITHDRAW_TYPEHASH, t.id, t.account, t.destination, t.amount, t.fee, t.relayer, t.validBefore)
        );
    }

    /// @notice The EIP-712 digest signed by the account holder.
    function digest(bytes32 termsHash) public view returns (bytes32) {
        return _hashTypedDataV4(termsHash);
    }

    /// @dev Write-once binding, per account. Returns true when the caller must
    ///      execute. Unbound -> bind and execute. Same terms -> no-op.
    ///      Different terms -> revert.
    ///
    ///      Every other check follows this one, so an executed operation replays
    ///      as a silent no-op even once its deadline has passed and its token
    ///      authorisation is spent. The write is undone by any later revert, so
    ///      an identifier is bound exactly when its operation executed.
    function _bind(address account, bytes32 id, bytes32 terms) private returns (bool execute) {
        bytes32 bound = operations[account][id];
        if (bound == terms) return false;
        if (bound != bytes32(0)) revert TermsConflict(account, id);
        operations[account][id] = terms;
        return true;
    }

    /// @dev The three conditions on a fresh execution: the named relayer is
    ///      carrying it, the deadline has not passed, and the debited account
    ///      signed exactly these terms.
    function _authorise(address account, bytes32 termsHash, bytes calldata sig, address relayer, uint256 validBefore)
        private
        view
    {
        if (msg.sender != relayer) revert WrongRelayer();
        if (block.timestamp >= validBefore) revert Expired();
        if (ECDSA.recover(_hashTypedDataV4(termsHash), sig) != account) revert BadSignature();
    }

    /// @dev Pulls exactly `total` from the payer under its own authorisation,
    ///      whose nonce is this operation's terms hash. The token balance is
    ///      read only to measure what arrived; no stored state derives from it.
    function _pull(address payer, uint256 total, uint256 validBefore, bytes32 nonce, bytes calldata authSig) private {
        uint256 before = token.balanceOf(address(this));
        (uint8 v, bytes32 r, bytes32 s) = _split(authSig);
        IERC3009(address(token)).receiveWithAuthorization(payer, address(this), total, 0, validBefore, nonce, v, r, s);
        if (token.balanceOf(address(this)) - before != total) revert InexactTransfer();
    }

    function _split(bytes calldata sig) private pure returns (uint8 v, bytes32 r, bytes32 s) {
        if (sig.length != 65) revert MalformedSignature();
        r = bytes32(sig[0:32]);
        s = bytes32(sig[32:64]);
        v = uint8(sig[64]);
    }
}
