// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {JuiceRail} from "../src/JuiceRail.sol";
import {MockUSDT0} from "./MockUSDT0.sol";

contract JuiceRailTest is Test {
    JuiceRail internal rail;
    MockUSDT0 internal token;

    address internal payer = makeAddr("payer");
    address internal alice = makeAddr("alice");
    address internal bob = makeAddr("bob");
    address internal carol = makeAddr("carol");

    bytes32 internal constant ID = bytes32(uint256(1));
    bytes32 internal constant ID2 = bytes32(uint256(2));

    event Deposited(bytes32 indexed id, address indexed account, uint256 amount);
    event Settled(bytes32 indexed id, address indexed debtor, address indexed creditor, uint256 amount);
    event Withdrawn(bytes32 indexed id, address indexed account, address to, uint256 amount);

    function setUp() public {
        token = new MockUSDT0();
        rail = new JuiceRail(IERC20(address(token)));
        token.mint(payer, 1_000_000);
        vm.prank(payer);
        token.approve(address(rail), type(uint256).max);
    }

    function _deposit(bytes32 id, address account, uint256 amount) internal {
        vm.prank(payer);
        rail.deposit(id, account, amount);
    }

    // --- terms hashes (the preimages the Go library must reproduce) ---

    function test_termsHashesMatchSpecPreimages() public view {
        assertEq(
            rail.depositTerms(alice, 100),
            keccak256(abi.encode(uint8(1), block.chainid, address(rail), alice, uint256(100)))
        );
        assertEq(
            rail.settleTerms(alice, bob, 100),
            keccak256(abi.encode(uint8(2), block.chainid, address(rail), alice, bob, uint256(100)))
        );
        assertEq(
            rail.withdrawTerms(alice, bob, 100),
            keccak256(abi.encode(uint8(3), block.chainid, address(rail), alice, bob, uint256(100)))
        );
    }

    // --- deposit ---

    function test_depositCreditsAccountAndBinds() public {
        vm.expectEmit(true, true, false, true, address(rail));
        emit Deposited(ID, alice, 100);
        _deposit(ID, alice, 100);

        assertEq(rail.balanceOf(alice), 100);
        assertEq(token.balanceOf(address(rail)), 100);
        assertEq(rail.operations(ID), rail.depositTerms(alice, 100));
    }

    function test_depositPayerDoesNotDetermineCreditedAccount() public {
        _deposit(ID, alice, 100);
        assertEq(rail.balanceOf(alice), 100);
        assertEq(rail.balanceOf(payer), 0);
    }

    function test_depositExactReplayIsSilentNoOp() public {
        _deposit(ID, alice, 100);
        uint256 held = token.balanceOf(address(rail));

        vm.recordLogs();
        _deposit(ID, alice, 100);

        assertEq(vm.getRecordedLogs().length, 0, "replay must emit nothing");
        assertEq(rail.balanceOf(alice), 100, "replay must not credit twice");
        assertEq(token.balanceOf(address(rail)), held, "replay must not move tokens");
    }

    function test_depositDifferentTermsReverts() public {
        _deposit(ID, alice, 100);
        vm.expectRevert(abi.encodeWithSelector(JuiceRail.TermsConflict.selector, ID));
        _deposit(ID, alice, 101);

        vm.expectRevert(abi.encodeWithSelector(JuiceRail.TermsConflict.selector, ID));
        _deposit(ID, bob, 100);
    }

    function test_depositRejectsInexactTransfer() public {
        token.setFeeBps(100);
        vm.expectRevert(JuiceRail.InexactTransfer.selector);
        _deposit(ID, alice, 100);
        assertEq(rail.balanceOf(alice), 0);
    }

    function test_bareTransferMintsNothing() public {
        vm.prank(payer);
        token.transfer(address(rail), 500);

        assertEq(token.balanceOf(address(rail)), 500);
        assertEq(rail.balanceOf(payer), 0);
        assertEq(rail.balanceOf(alice), 0);

        // The stray balance neither blocks nor inflates a later deposit.
        _deposit(ID, alice, 100);
        assertEq(rail.balanceOf(alice), 100);
    }

    // --- settle ---

    function test_settleMovesBalanceAndConservesSum() public {
        _deposit(ID, alice, 100);

        vm.expectEmit(true, true, true, true, address(rail));
        emit Settled(ID2, alice, bob, 40);
        vm.prank(alice);
        rail.settle(ID2, bob, 40);

        assertEq(rail.balanceOf(alice), 60);
        assertEq(rail.balanceOf(bob), 40);
        assertEq(token.balanceOf(address(rail)), 100, "settle moves no tokens");
    }

    function test_settleDebitsOnlyTheCaller() public {
        _deposit(ID, alice, 100);

        vm.prank(bob); // bob has nothing; he cannot reach alice's balance
        vm.expectRevert(JuiceRail.InsufficientBalance.selector);
        rail.settle(ID2, carol, 40);

        assertEq(rail.balanceOf(alice), 100);
    }

    function test_settleToSelfConservesBalance() public {
        _deposit(ID, alice, 100);
        vm.prank(alice);
        rail.settle(ID2, alice, 40);
        assertEq(rail.balanceOf(alice), 100);
    }

    function test_settleExactReplayIsSilentNoOp() public {
        _deposit(ID, alice, 100);
        vm.startPrank(alice);
        rail.settle(ID2, bob, 40);

        vm.recordLogs();
        rail.settle(ID2, bob, 40);
        vm.stopPrank();

        assertEq(vm.getRecordedLogs().length, 0);
        assertEq(rail.balanceOf(alice), 60);
        assertEq(rail.balanceOf(bob), 40);
    }

    function test_settleByDifferentDebtorSameIdReverts() public {
        _deposit(ID, alice, 100);
        _deposit(ID2, carol, 100);

        bytes32 settleId = bytes32(uint256(3));
        vm.prank(alice);
        rail.settle(settleId, bob, 40);

        // The debtor is part of the terms, so carol's reuse is loud, not silent.
        vm.prank(carol);
        vm.expectRevert(abi.encodeWithSelector(JuiceRail.TermsConflict.selector, settleId));
        rail.settle(settleId, bob, 40);

        assertEq(rail.balanceOf(carol), 100);
    }

    // --- withdraw ---

    function test_withdrawDecreasesBalanceAndHeldEqually() public {
        _deposit(ID, alice, 100);

        vm.expectEmit(true, true, false, true, address(rail));
        emit Withdrawn(ID2, alice, bob, 25);
        vm.prank(alice);
        rail.withdraw(ID2, bob, 25);

        assertEq(rail.balanceOf(alice), 75);
        assertEq(token.balanceOf(address(rail)), 75);
        assertEq(token.balanceOf(bob), 25);
    }

    function test_withdrawInsufficientBalanceReverts() public {
        _deposit(ID, alice, 100);
        vm.prank(alice);
        vm.expectRevert(JuiceRail.InsufficientBalance.selector);
        rail.withdraw(ID2, bob, 101);
    }

    function test_withdrawExactReplayIsSilentNoOp() public {
        _deposit(ID, alice, 100);
        vm.startPrank(alice);
        rail.withdraw(ID2, bob, 25);

        vm.recordLogs();
        rail.withdraw(ID2, bob, 25);
        vm.stopPrank();

        assertEq(vm.getRecordedLogs().length, 0);
        assertEq(rail.balanceOf(alice), 75);
        assertEq(token.balanceOf(bob), 25, "replay must not pay twice");
    }

    // --- one binding rule across all kinds ---

    function test_idReuseAcrossKindsReverts() public {
        _deposit(ID, alice, 100);

        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(JuiceRail.TermsConflict.selector, ID));
        rail.settle(ID, bob, 10);

        vm.prank(alice);
        vm.expectRevert(abi.encodeWithSelector(JuiceRail.TermsConflict.selector, ID));
        rail.withdraw(ID, bob, 10);
    }

    function test_settleAndWithdrawWithIdenticalArgumentsDoNotShareTerms() public {
        _deposit(ID, alice, 100);
        vm.prank(alice);
        rail.settle(ID2, bob, 10);

        // Same id would conflict; a fresh id must still distinguish the kinds.
        assertTrue(rail.settleTerms(alice, bob, 10) != rail.withdrawTerms(alice, bob, 10));
    }

    // --- fuzz ---

    function testFuzz_depositThenWithdrawRoundTrips(uint96 amount, address account) public {
        vm.assume(account != address(0) && account != address(rail));
        token.mint(payer, amount);

        _deposit(ID, account, amount);
        assertEq(rail.balanceOf(account), amount);

        uint256 heldBefore = token.balanceOf(address(rail));
        vm.prank(account);
        rail.withdraw(ID2, account, amount);

        assertEq(rail.balanceOf(account), 0);
        assertEq(token.balanceOf(address(rail)), heldBefore - amount);
    }

    function testFuzz_conflictingTermsAlwaysRevert(bytes32 id, uint96 a, uint96 b) public {
        vm.assume(a != b);
        token.mint(payer, uint256(a) + uint256(b));

        _deposit(id, alice, a);
        vm.expectRevert(abi.encodeWithSelector(JuiceRail.TermsConflict.selector, id));
        _deposit(id, alice, b);
    }
}
