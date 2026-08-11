// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {JuiceRail} from "../src/JuiceRail.sol";
import {MockUSDT0} from "./MockUSDT0.sol";

/// @notice Drives the rail the way the world does: signed operations carried by
///         the relayer they name, with a small identifier space so replays and
///         conflicts actually happen, and forged signatures that must never
///         move money.
contract Handler is Test {
    JuiceRail public immutable rail;
    MockUSDT0 public immutable token;

    uint256 public constant ACTORS = 4;
    address public constant OUTSIDE = address(0xBEEF);

    /// Ghost totals: what entered the rail and what left it.
    uint256 public deposited;
    uint256 public withdrawn;

    /// The terms each identifier bound, to prove the binding is write-once.
    mapping(address => mapping(bytes32 => bytes32)) public firstTerms;

    constructor(JuiceRail r, MockUSDT0 t) {
        rail = r;
        token = t;
    }

    function actorKey(uint256 seed) public pure returns (uint256) {
        return (seed % ACTORS) + 1;
    }

    function actor(uint256 seed) public pure returns (address) {
        return vm.addr(actorKey(seed));
    }

    function _id(uint256 seed) internal pure returns (bytes32) {
        return bytes32(seed % 8);
    }

    function _sign(uint256 pk, bytes32 d) internal pure returns (bytes memory) {
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(pk, d);
        return abi.encodePacked(r, s, v);
    }

    /// @dev Records the binding and reports whether this call would be the one
    ///      that executes it.
    function _fresh(address account, bytes32 id) internal view returns (bool) {
        return rail.operations(account, id) == bytes32(0);
    }

    function _remember(address account, bytes32 id) internal {
        bytes32 bound = rail.operations(account, id);
        if (bound != bytes32(0) && firstTerms[account][id] == bytes32(0)) {
            firstTerms[account][id] = bound;
        }
    }

    function deposit(uint256 payerSeed, uint256 accountSeed, uint256 relayerSeed, uint96 amount, uint96 fee, uint256 idSeed)
        external
    {
        uint256 pk = actorKey(payerSeed);
        address payer = vm.addr(pk);
        JuiceRail.DepositTerms memory t = JuiceRail.DepositTerms({
            id: _id(idSeed),
            payer: payer,
            account: actor(accountSeed),
            amount: amount,
            fee: fee,
            relayer: actor(relayerSeed),
            validBefore: block.timestamp + 1 days
        });
        bytes32 h = rail.depositHash(t);
        uint256 total = uint256(amount) + fee;
        token.mint(payer, total);

        bytes32 authStruct = keccak256(
            abi.encode(
                token.RECEIVE_WITH_AUTHORIZATION_TYPEHASH(),
                payer,
                address(rail),
                total,
                uint256(0),
                t.validBefore,
                h
            )
        );
        bytes memory authSig =
            _sign(pk, keccak256(abi.encodePacked("\x19\x01", token.DOMAIN_SEPARATOR(), authStruct)));
        // Signatures are computed before the prank: reading a hash from the
        // rail is itself a call, and would consume it.
        bytes memory termsSig = _sign(pk, rail.digest(h));

        bool fresh = _fresh(payer, t.id);
        vm.prank(t.relayer);
        try rail.deposit(t, termsSig, authSig) {
            if (fresh && !_fresh(payer, t.id)) deposited += total;
        } catch {}
        _remember(payer, t.id);
    }

    function transfer(uint256 senderSeed, uint256 recipientSeed, uint256 relayerSeed, uint96 amount, uint96 fee, uint256 idSeed)
        external
    {
        uint256 pk = actorKey(senderSeed);
        address sender = vm.addr(pk);
        JuiceRail.TransferTerms memory t = JuiceRail.TransferTerms({
            id: _id(idSeed),
            sender: sender,
            recipient: actor(recipientSeed),
            amount: amount,
            fee: fee,
            relayer: actor(relayerSeed),
            validBefore: block.timestamp + 1 days
        });
        bytes memory sig = _sign(pk, rail.digest(rail.transferHash(t)));
        vm.prank(t.relayer);
        try rail.transfer(t, sig) {} catch {}
        _remember(sender, t.id);
    }

    function withdraw(uint256 accountSeed, uint256 relayerSeed, uint96 amount, uint96 fee, uint256 idSeed) external {
        uint256 pk = actorKey(accountSeed);
        address account = vm.addr(pk);
        JuiceRail.WithdrawTerms memory t = JuiceRail.WithdrawTerms({
            id: _id(idSeed),
            account: account,
            destination: OUTSIDE,
            amount: amount,
            fee: fee,
            relayer: actor(relayerSeed),
            validBefore: block.timestamp + 1 days
        });
        bytes memory sig = _sign(pk, rail.digest(rail.withdrawHash(t)));
        bool fresh = _fresh(account, t.id);
        vm.prank(t.relayer);
        try rail.withdraw(t, sig) {
            if (fresh && !_fresh(account, t.id)) withdrawn += amount;
        } catch {}
        _remember(account, t.id);
    }

    /// A signature from the wrong key must never move money.
    function forge_(uint256 senderSeed, uint256 forgerSeed, uint96 amount, uint256 idSeed) external {
        address sender = actor(senderSeed);
        uint256 forger = actorKey(forgerSeed);
        if (vm.addr(forger) == sender) return;

        JuiceRail.TransferTerms memory t = JuiceRail.TransferTerms({
            id: _id(idSeed),
            sender: sender,
            recipient: OUTSIDE,
            amount: amount,
            fee: 0,
            relayer: actor(forgerSeed),
            validBefore: block.timestamp + 1 days
        });
        bytes memory sig = _sign(forger, rail.digest(rail.transferHash(t)));
        vm.prank(t.relayer);
        try rail.transfer(t, sig) {
            revert("a forged signature moved money");
        } catch {}
    }

    /// Tokens sent to the rail by hand must never become anyone's balance.
    function donate(uint96 amount) external {
        token.mint(address(rail), amount);
    }

    /// Time passing is what expires an operation.
    function passTime(uint16 step) external {
        vm.warp(block.timestamp + step);
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

    /// Every address the handler can ever credit, each counted once.
    function _sumOfBalances() internal view returns (uint256 sum) {
        for (uint256 i = 0; i < handler.ACTORS(); i++) {
            sum += rail.balanceOf(handler.actor(i));
        }
        sum += rail.balanceOf(handler.OUTSIDE());
        sum += rail.balanceOf(address(rail));
    }

    /// The invariants above are worthless if the handler never moves money, so
    /// this asserts the actions actually execute.
    function test_theHandlerMovesMoney() public {
        handler.deposit(0, 0, 1, 100e6, 1e4, 0);
        assertEq(handler.deposited(), 100e6 + 1e4, "a deposit executed");
        assertEq(rail.balanceOf(handler.actor(0)), 100e6);

        // actor(1) already holds the deposit's relay fee.
        handler.transfer(0, 1, 2, 10e6, 1e4, 1);
        assertEq(rail.balanceOf(handler.actor(1)), 10e6 + 1e4, "a transfer executed");
        assertEq(rail.balanceOf(handler.actor(2)), 1e4, "the relayer was paid");

        handler.withdraw(0, 1, 5e6, 0, 2);
        assertEq(handler.withdrawn(), 5e6, "a withdrawal executed");
        assertEq(token.balanceOf(handler.OUTSIDE()), 5e6);

        assertEq(_sumOfBalances(), handler.deposited() - handler.withdrawn());
    }

    /// Every balance is backed by stablecoin the contract actually holds.
    function invariant_heldCoversBalances() public view {
        assertGe(token.balanceOf(address(rail)), _sumOfBalances());
    }

    /// Only deposits and withdrawals change the sum; transfers and fees do not.
    function invariant_sumIsDepositsMinusWithdrawals() public view {
        assertEq(_sumOfBalances(), handler.deposited() - handler.withdrawn());
    }

    /// An identifier binds once, under its own account, and never rebinds.
    function invariant_bindingIsWriteOnce() public view {
        for (uint256 a = 0; a < handler.ACTORS(); a++) {
            address account = handler.actor(a);
            for (uint256 i = 0; i < 8; i++) {
                bytes32 first = handler.firstTerms(account, bytes32(i));
                if (first != bytes32(0)) {
                    assertEq(rail.operations(account, bytes32(i)), first);
                }
            }
        }
    }
}
