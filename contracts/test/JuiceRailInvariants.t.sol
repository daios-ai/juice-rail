// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {JuiceRail} from "../src/JuiceRail.sol";
import {MockUSDT0} from "./MockUSDT0.sol";

/// @notice Drives the rail with bounded random traffic and records what the
///         invariants are checked against.
contract Handler is Test {
    JuiceRail public immutable rail;
    MockUSDT0 public immutable token;

    address[] public actors;
    bytes32[] public ids;
    mapping(bytes32 id => bytes32) public firstTerms;

    uint256 public deposited;
    uint256 public withdrawn;

    constructor(JuiceRail rail_, MockUSDT0 token_) {
        rail = rail_;
        token = token_;
        for (uint256 i = 0; i < 4; i++) {
            actors.push(address(uint160(0x1000 + i)));
        }
    }

    function actorCount() external view returns (uint256) {
        return actors.length;
    }

    function idCount() external view returns (uint256) {
        return ids.length;
    }

    function _actor(uint256 seed) internal view returns (address) {
        return actors[seed % actors.length];
    }

    function _id(uint256 seed) internal returns (bytes32 id) {
        // A small id space so replays and conflicts actually occur.
        id = bytes32(seed % 8);
        for (uint256 i = 0; i < ids.length; i++) {
            if (ids[i] == id) return id;
        }
        ids.push(id);
    }

    function _remember(bytes32 id) internal {
        bytes32 bound = rail.operations(id);
        if (bound != bytes32(0) && firstTerms[id] == bytes32(0)) firstTerms[id] = bound;
    }

    function deposit(uint256 idSeed, uint256 accountSeed, uint256 amount) external {
        bytes32 id = _id(idSeed);
        address account = _actor(accountSeed);
        amount = bound(amount, 0, 1e12);

        bool fresh = rail.operations(id) == bytes32(0);
        token.mint(address(this), amount);
        token.approve(address(rail), amount);

        try rail.deposit(id, account, amount) {
            if (fresh) deposited += amount;
        } catch {}
        _remember(id);
    }

    function settle(uint256 idSeed, uint256 debtorSeed, uint256 creditorSeed, uint256 amount) external {
        bytes32 id = _id(idSeed);
        address debtor = _actor(debtorSeed);
        amount = bound(amount, 0, rail.balanceOf(debtor));

        vm.prank(debtor);
        try rail.settle(id, _actor(creditorSeed), amount) {} catch {}
        _remember(id);
    }

    function withdraw(uint256 idSeed, uint256 accountSeed, uint256 amount) external {
        bytes32 id = _id(idSeed);
        address account = _actor(accountSeed);
        amount = bound(amount, 0, rail.balanceOf(account));

        bool fresh = rail.operations(id) == bytes32(0);
        vm.prank(account);
        try rail.withdraw(id, account, amount) {
            if (fresh) withdrawn += amount;
        } catch {}
        _remember(id);
    }

    /// @dev Unsolicited tokens must never become balances.
    function donate(uint256 amount) external {
        amount = bound(amount, 0, 1e12);
        token.mint(address(rail), amount);
    }
}

contract JuiceRailInvariantsTest is Test {
    JuiceRail internal rail;
    MockUSDT0 internal token;
    Handler internal handler;

    function setUp() public {
        token = new MockUSDT0();
        rail = new JuiceRail(IERC20(address(token)));
        handler = new Handler(rail, token);
        targetContract(address(handler));
    }

    function _sumBalances() internal view returns (uint256 total) {
        for (uint256 i = 0; i < handler.actorCount(); i++) {
            total += rail.balanceOf(handler.actors(i));
        }
    }

    /// held USDT0 >= sum of balances
    function invariant_heldCoversBalances() public view {
        assertGe(token.balanceOf(address(rail)), _sumBalances());
    }

    /// settle conserves the sum; deposit and withdraw change it by exactly the
    /// amount moved. Together: the sum is deposits minus withdrawals, always.
    function invariant_sumIsDepositsMinusWithdrawals() public view {
        assertEq(_sumBalances(), handler.deposited() - handler.withdrawn());
    }

    /// A bound identifier never changes its terms.
    function invariant_bindingIsWriteOnce() public view {
        for (uint256 i = 0; i < handler.idCount(); i++) {
            bytes32 id = handler.ids(i);
            bytes32 first = handler.firstTerms(id);
            if (first != bytes32(0)) assertEq(rail.operations(id), first);
        }
    }
}
