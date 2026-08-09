// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";

/// @title JuiceRail
/// @notice Non-upgradeable, adminless settlement rail. One deployment is one
///         domain, identified by (chain id, contract address).
/// @dev Three money transitions and one write-once binding rule. Every intent
///      executes or reverts; an exact replay is a silent no-op. There is no
///      stored liability total: solvency (held >= sum of balances) is checkable
///      off-chain from events.
contract JuiceRail {
    using SafeERC20 for IERC20;

    /// @dev Operation kinds. The kind is part of the terms hash, so one
    ///      identifier can never mean two different intents.
    uint8 internal constant KIND_DEPOSIT = 1;
    uint8 internal constant KIND_SETTLE = 2;
    uint8 internal constant KIND_WITHDRAW = 3;

    IERC20 public immutable usdt0;

    mapping(address account => uint256) public balanceOf;
    mapping(bytes32 id => bytes32 termsHash) public operations;

    event Deposited(bytes32 indexed id, address indexed account, uint256 amount);
    event Settled(bytes32 indexed id, address indexed debtor, address indexed creditor, uint256 amount);
    event Withdrawn(bytes32 indexed id, address indexed account, address to, uint256 amount);

    /// @notice The identifier is already bound to different terms.
    error TermsConflict(bytes32 id);
    /// @notice The debtor's balance does not cover the amount.
    error InsufficientBalance();
    /// @notice The token did not move exactly `amount` into this contract.
    error InexactTransfer();

    constructor(IERC20 token) {
        usdt0 = token;
    }

    /// @notice Pull `amount` from the caller and credit `account`.
    /// @dev The caller's address never determines who is credited.
    function deposit(bytes32 id, address account, uint256 amount) external {
        if (!_bind(id, depositTerms(account, amount))) return;

        uint256 balanceBefore = usdt0.balanceOf(address(this));
        usdt0.safeTransferFrom(msg.sender, address(this), amount);
        if (usdt0.balanceOf(address(this)) - balanceBefore != amount) revert InexactTransfer();

        balanceOf[account] += amount;
        emit Deposited(id, account, amount);
    }

    /// @notice Move `amount` of the caller's balance to `creditor`.
    /// @dev A debtor debits only itself; the sum of balances is preserved.
    function settle(bytes32 id, address creditor, uint256 amount) external {
        if (!_bind(id, settleTerms(msg.sender, creditor, amount))) return;

        uint256 balance = balanceOf[msg.sender];
        if (balance < amount) revert InsufficientBalance();
        unchecked {
            balanceOf[msg.sender] = balance - amount;
        }
        balanceOf[creditor] += amount;

        emit Settled(id, msg.sender, creditor, amount);
    }

    /// @notice Debit the caller and transfer `amount` of USDT0 to `to`.
    function withdraw(bytes32 id, address to, uint256 amount) external {
        if (!_bind(id, withdrawTerms(msg.sender, to, amount))) return;

        uint256 balance = balanceOf[msg.sender];
        if (balance < amount) revert InsufficientBalance();
        unchecked {
            balanceOf[msg.sender] = balance - amount;
        }

        emit Withdrawn(id, msg.sender, to, amount);
        usdt0.safeTransfer(to, amount);
    }

    function depositTerms(address account, uint256 amount) public view returns (bytes32) {
        return keccak256(abi.encode(KIND_DEPOSIT, block.chainid, address(this), account, amount));
    }

    function settleTerms(address debtor, address creditor, uint256 amount) public view returns (bytes32) {
        return keccak256(abi.encode(KIND_SETTLE, block.chainid, address(this), debtor, creditor, amount));
    }

    function withdrawTerms(address account, address to, uint256 amount) public view returns (bytes32) {
        return keccak256(abi.encode(KIND_WITHDRAW, block.chainid, address(this), account, to, amount));
    }

    /// @dev Write-once binding. Returns true when the caller must execute.
    ///      Unbound -> bind and execute. Same terms -> no-op. Different -> revert.
    function _bind(bytes32 id, bytes32 terms) private returns (bool execute) {
        bytes32 bound = operations[id];
        if (bound == terms) return false;
        if (bound != bytes32(0)) revert TermsConflict(id);
        operations[id] = terms;
        return true;
    }
}
