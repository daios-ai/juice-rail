// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test, Vm} from "forge-std/Test.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {ECDSA} from "@openzeppelin/contracts/utils/cryptography/ECDSA.sol";
import {JuiceRail} from "../src/JuiceRail.sol";
import {MockUSDT0} from "./MockUSDT0.sol";

contract JuiceRailTest is Test {
    JuiceRail internal rail;
    MockUSDT0 internal token;

    uint256 internal constant ALICE_KEY = 0xA11CE;
    uint256 internal constant BOB_KEY = 0xB0B;
    uint256 internal constant RELAYER_KEY = 0xEEE;
    uint256 internal constant MALLORY_KEY = 0xBAD;

    address internal alice;
    address internal bob;
    address internal relayer;
    address internal mallory;
    address internal outside = address(0xDEAD);

    uint256 internal deadline;

    function setUp() public {
        token = new MockUSDT0();
        rail = new JuiceRail(IERC20(address(token)));

        alice = vm.addr(ALICE_KEY);
        bob = vm.addr(BOB_KEY);
        relayer = vm.addr(RELAYER_KEY);
        mallory = vm.addr(MALLORY_KEY);

        vm.warp(1_000_000);
        deadline = block.timestamp + 1 hours;
    }

    // --- signing helpers ---

    function _sign(uint256 pk, bytes32 d) internal pure returns (bytes memory) {
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(pk, d);
        return abi.encodePacked(r, s, v);
    }

    /// @dev The token authorisation whose nonce is the deposit's terms hash.
    function _auth(uint256 pk, address from, uint256 value, uint256 validBefore, bytes32 nonce)
        internal
        view
        returns (bytes memory)
    {
        bytes32 structHash = keccak256(
            abi.encode(
                token.RECEIVE_WITH_AUTHORIZATION_TYPEHASH(),
                from,
                address(rail),
                value,
                uint256(0),
                validBefore,
                nonce
            )
        );
        return _sign(pk, keccak256(abi.encodePacked("\x19\x01", token.DOMAIN_SEPARATOR(), structHash)));
    }

    function _depositTerms(bytes32 id, address payer, address account, uint256 amount, uint256 fee)
        internal
        view
        returns (JuiceRail.DepositTerms memory)
    {
        return JuiceRail.DepositTerms({
            id: id,
            payer: payer,
            account: account,
            amount: amount,
            fee: fee,
            relayer: relayer,
            validBefore: deadline
        });
    }

    function _transferTerms(bytes32 id, address sender, address recipient, uint256 amount, uint256 fee)
        internal
        view
        returns (JuiceRail.TransferTerms memory)
    {
        return JuiceRail.TransferTerms({
            id: id,
            sender: sender,
            recipient: recipient,
            amount: amount,
            fee: fee,
            relayer: relayer,
            validBefore: deadline
        });
    }

    function _withdrawTerms(bytes32 id, address account, address destination, uint256 amount, uint256 fee)
        internal
        view
        returns (JuiceRail.WithdrawTerms memory)
    {
        return JuiceRail.WithdrawTerms({
            id: id,
            account: account,
            destination: destination,
            amount: amount,
            fee: fee,
            relayer: relayer,
            validBefore: deadline
        });
    }

    function _submitDeposit(uint256 pk, JuiceRail.DepositTerms memory t) internal {
        bytes32 h = rail.depositHash(t);
        bytes memory termsSig = _sign(pk, rail.digest(h));
        bytes memory authSig = _auth(pk, t.payer, t.amount + t.fee, t.validBefore, h);
        vm.prank(t.relayer);
        rail.deposit(t, termsSig, authSig);
    }

    // Signatures are always computed before the prank: reading a hash from the
    // rail is itself a call, and would consume it.

    function _submitTransfer(uint256 pk, JuiceRail.TransferTerms memory t) internal {
        bytes memory sig = _sign(pk, rail.digest(rail.transferHash(t)));
        vm.prank(t.relayer);
        rail.transfer(t, sig);
    }

    function _submitWithdraw(uint256 pk, JuiceRail.WithdrawTerms memory t) internal {
        bytes memory sig = _sign(pk, rail.digest(rail.withdrawHash(t)));
        vm.prank(t.relayer);
        rail.withdraw(t, sig);
    }

    /// @dev Gives an account a rail balance through a real deposit.
    function _credit(uint256 pk, address account, uint256 amount) internal {
        address payer = vm.addr(pk);
        token.mint(payer, amount);
        _submitDeposit(pk, _depositTerms(keccak256(abi.encode("credit", account, amount)), payer, account, amount, 0));
    }

    function _sumOfBalances() internal view returns (uint256) {
        return rail.balanceOf(alice) + rail.balanceOf(bob) + rail.balanceOf(relayer) + rail.balanceOf(mallory)
            + rail.balanceOf(outside) + rail.balanceOf(address(rail));
    }

    // --- the three transitions ---

    function test_depositCreditsTheAccountAndPaysTheRelayer() public {
        token.mint(alice, 100e6);
        JuiceRail.DepositTerms memory t = _depositTerms(bytes32("d1"), alice, alice, 60e6, 2e4);

        vm.recordLogs();
        _submitDeposit(ALICE_KEY, t);

        assertEq(rail.balanceOf(alice), 60e6);
        assertEq(rail.balanceOf(relayer), 2e4);
        assertEq(token.balanceOf(address(rail)), 60e6 + 2e4);
        assertEq(token.balanceOf(alice), 100e6 - 60e6 - 2e4);
        assertEq(vm.getRecordedLogs().length, 2); // token Transfer, rail Deposited
    }

    /// The payer's address never determines the credited account.
    function test_depositCreditsAThirdParty() public {
        token.mint(alice, 100e6);
        _submitDeposit(ALICE_KEY, _depositTerms(bytes32("d2"), alice, bob, 50e6, 1e4));

        assertEq(rail.balanceOf(bob), 50e6);
        assertEq(rail.balanceOf(alice), 0);
        assertEq(rail.balanceOf(relayer), 1e4);
    }

    function test_transferMovesAmountAndFee() public {
        _credit(ALICE_KEY, alice, 100e6);
        uint256 sum = _sumOfBalances();

        _submitTransfer(ALICE_KEY, _transferTerms(bytes32("t1"), alice, bob, 10e6, 1e4));

        assertEq(rail.balanceOf(alice), 100e6 - 10e6 - 1e4);
        assertEq(rail.balanceOf(bob), 10e6);
        assertEq(rail.balanceOf(relayer), 1e4);
        assertEq(_sumOfBalances(), sum, "transfer preserves the sum of balances");
    }

    function test_withdrawSendsToTheSignedDestination() public {
        _credit(ALICE_KEY, alice, 50e6);
        uint256 held = token.balanceOf(address(rail));
        uint256 sum = _sumOfBalances();

        _submitWithdraw(ALICE_KEY, _withdrawTerms(bytes32("w1"), alice, outside, 20e6, 1e4));

        assertEq(token.balanceOf(outside), 20e6, "the destination receives the amount");
        assertEq(rail.balanceOf(alice), 50e6 - 20e6 - 1e4);
        assertEq(rail.balanceOf(relayer), 1e4);
        assertEq(token.balanceOf(address(rail)), held - 20e6, "held drops by the amount");
        assertEq(_sumOfBalances(), sum - 20e6, "the sum drops by the amount; the fee stays inside");
    }

    // --- replay ---

    function test_transferReplayIsASilentNoOp() public {
        _credit(ALICE_KEY, alice, 100e6);
        JuiceRail.TransferTerms memory t = _transferTerms(bytes32("t2"), alice, bob, 10e6, 1e4);
        _submitTransfer(ALICE_KEY, t);

        uint256 aliceBalance = rail.balanceOf(alice);
        vm.recordLogs();
        _submitTransfer(ALICE_KEY, t);

        assertEq(vm.getRecordedLogs().length, 0, "a replay emits nothing");
        assertEq(rail.balanceOf(alice), aliceBalance, "a replay moves nothing");
        assertEq(rail.balanceOf(bob), 10e6);
    }

    /// The binding is checked before the deadline, so an executed operation
    /// still replays harmlessly once it has expired.
    function test_replayAfterExpiryIsASilentNoOp() public {
        _credit(ALICE_KEY, alice, 100e6);
        JuiceRail.TransferTerms memory t = _transferTerms(bytes32("t3"), alice, bob, 10e6, 1e4);
        _submitTransfer(ALICE_KEY, t);

        vm.warp(deadline + 1);
        vm.recordLogs();
        _submitTransfer(ALICE_KEY, t);
        assertEq(vm.getRecordedLogs().length, 0);
        assertEq(rail.balanceOf(bob), 10e6);
    }

    /// A deposit replay must not reach the token, whose authorisation nonce is
    /// spent: the binding check comes first.
    function test_depositReplayAfterTheAuthorisationIsSpentIsASilentNoOp() public {
        token.mint(alice, 100e6);
        JuiceRail.DepositTerms memory t = _depositTerms(bytes32("d3"), alice, alice, 60e6, 2e4);
        _submitDeposit(ALICE_KEY, t);

        assertTrue(token.authorizationState(alice, rail.depositHash(t)), "the nonce is spent");
        vm.recordLogs();
        _submitDeposit(ALICE_KEY, t);
        assertEq(vm.getRecordedLogs().length, 0);
        assertEq(rail.balanceOf(alice), 60e6);
    }

    /// A replay needs no authority: it moves nothing, so anyone may present it.
    function test_anyoneMayReplayAnExecutedOperation() public {
        _credit(ALICE_KEY, alice, 100e6);
        JuiceRail.TransferTerms memory t = _transferTerms(bytes32("t4"), alice, bob, 10e6, 1e4);
        _submitTransfer(ALICE_KEY, t);

        bytes memory sig = _sign(ALICE_KEY, rail.digest(rail.transferHash(t)));
        vm.prank(mallory);
        rail.transfer(t, sig);
        assertEq(rail.balanceOf(bob), 10e6);
    }

    // --- conflicting reuse ---

    function test_differentTermsUnderOneIdentifierRevert() public {
        _credit(ALICE_KEY, alice, 100e6);
        JuiceRail.TransferTerms memory t = _transferTerms(bytes32("t5"), alice, bob, 10e6, 1e4);
        _submitTransfer(ALICE_KEY, t);

        // Every field is part of the terms, so any change is a conflict.
        JuiceRail.TransferTerms[5] memory variants = [t, t, t, t, t];
        variants[0].amount = 11e6;
        variants[1].fee = 2e4;
        variants[2].recipient = mallory;
        variants[3].relayer = mallory;
        variants[4].validBefore = deadline + 1;

        for (uint256 i = 0; i < variants.length; i++) {
            bytes memory sig = _sign(ALICE_KEY, rail.digest(rail.transferHash(variants[i])));
            vm.prank(variants[i].relayer);
            vm.expectRevert(abi.encodeWithSelector(JuiceRail.TermsConflict.selector, alice, t.id));
            rail.transfer(variants[i], sig);
        }
    }

    function test_identifierReuseAcrossKindsReverts() public {
        _credit(ALICE_KEY, alice, 100e6);
        bytes32 id = bytes32("shared");
        _submitTransfer(ALICE_KEY, _transferTerms(id, alice, bob, 10e6, 0));

        JuiceRail.WithdrawTerms memory w = _withdrawTerms(id, alice, outside, 10e6, 0);
        bytes memory sig = _sign(ALICE_KEY, rail.digest(rail.withdrawHash(w)));
        vm.prank(relayer);
        vm.expectRevert(abi.encodeWithSelector(JuiceRail.TermsConflict.selector, alice, id));
        rail.withdraw(w, sig);
    }

    /// Identifiers are scoped to the authorising account, so no one can burn
    /// another account's identifier.
    function test_oneIdentifierUnderTwoAccountsBothExecute() public {
        _credit(ALICE_KEY, alice, 100e6);
        _credit(BOB_KEY, bob, 100e6);
        bytes32 id = bytes32("mine");

        _submitTransfer(ALICE_KEY, _transferTerms(id, alice, bob, 10e6, 0));
        _submitTransfer(BOB_KEY, _transferTerms(id, bob, alice, 7e6, 0));

        assertEq(rail.balanceOf(alice), 100e6 - 10e6 + 7e6);
        assertEq(rail.balanceOf(bob), 100e6 + 10e6 - 7e6);
    }

    // --- authorisation ---

    function test_expiredOperationReverts() public {
        _credit(ALICE_KEY, alice, 100e6);
        JuiceRail.TransferTerms memory t = _transferTerms(bytes32("t6"), alice, bob, 10e6, 0);
        bytes memory sig = _sign(ALICE_KEY, rail.digest(rail.transferHash(t)));

        vm.warp(deadline);
        vm.prank(relayer);
        vm.expectRevert(JuiceRail.Expired.selector);
        rail.transfer(t, sig);
    }

    function test_onlyTheNamedRelayerMaySubmit() public {
        _credit(ALICE_KEY, alice, 100e6);
        JuiceRail.TransferTerms memory t = _transferTerms(bytes32("t7"), alice, bob, 10e6, 1e4);
        bytes memory sig = _sign(ALICE_KEY, rail.digest(rail.transferHash(t)));

        vm.prank(mallory);
        vm.expectRevert(JuiceRail.WrongRelayer.selector);
        rail.transfer(t, sig);

        // The identifier is untouched: the account may still have it carried.
        assertEq(rail.operations(alice, t.id), bytes32(0));
        _submitTransfer(ALICE_KEY, t);
        assertEq(rail.balanceOf(bob), 10e6);
    }

    /// Naming oneself is how an account with gas submits its own operation.
    function test_anAccountMayRelayForItself() public {
        _credit(ALICE_KEY, alice, 100e6);
        JuiceRail.TransferTerms memory t = _transferTerms(bytes32("t8"), alice, bob, 10e6, 1e4);
        t.relayer = alice;
        _submitTransfer(ALICE_KEY, t);

        assertEq(rail.balanceOf(alice), 100e6 - 10e6, "the fee returns to itself");
        assertEq(rail.balanceOf(bob), 10e6);
    }

    function test_anotherAccountsSignatureIsRejected() public {
        _credit(ALICE_KEY, alice, 100e6);
        JuiceRail.TransferTerms memory t = _transferTerms(bytes32("t9"), alice, bob, 10e6, 0);
        bytes memory sig = _sign(MALLORY_KEY, rail.digest(rail.transferHash(t)));

        vm.prank(relayer);
        vm.expectRevert(JuiceRail.BadSignature.selector);
        rail.transfer(t, sig);
    }

    function test_malformedSignaturesAreRejected() public {
        _credit(ALICE_KEY, alice, 100e6);
        JuiceRail.TransferTerms memory t = _transferTerms(bytes32("t10"), alice, bob, 10e6, 0);

        // A signature that recovers to some unrelated address is not Alice's.
        bytes memory stray = abi.encodePacked(bytes32(uint256(1)), bytes32(uint256(1)), uint8(27));
        // One that recovers to nothing must never pass, not even as address(0).
        bytes memory nothing = abi.encodePacked(bytes32(0), bytes32(0), uint8(27));
        JuiceRail.TransferTerms memory zero = _transferTerms(bytes32("t10b"), address(0), bob, 1, 0);

        vm.startPrank(relayer);
        vm.expectRevert(abi.encodeWithSelector(ECDSA.ECDSAInvalidSignatureLength.selector, uint256(64)));
        rail.transfer(t, new bytes(64));

        vm.expectRevert(JuiceRail.BadSignature.selector);
        rail.transfer(t, stray);

        vm.expectRevert(abi.encodeWithSelector(ECDSA.ECDSAInvalidSignature.selector));
        rail.transfer(zero, nothing);
        vm.stopPrank();
    }

    /// The terms signer and the token authorisation's signer must be one.
    function test_depositRejectsAnAuthorisationFromAnotherPayer() public {
        token.mint(alice, 100e6);
        token.mint(mallory, 100e6);
        JuiceRail.DepositTerms memory t = _depositTerms(bytes32("d4"), alice, alice, 60e6, 0);
        bytes32 h = rail.depositHash(t);
        bytes memory termsSig = _sign(ALICE_KEY, rail.digest(h));
        bytes memory authSig = _auth(MALLORY_KEY, alice, 60e6, t.validBefore, h);

        vm.prank(relayer);
        vm.expectRevert(MockUSDT0.InvalidSignature.selector);
        rail.deposit(t, termsSig, authSig);
    }

    function test_depositRejectsAMalformedAuthorisation() public {
        token.mint(alice, 100e6);
        JuiceRail.DepositTerms memory t = _depositTerms(bytes32("d5"), alice, alice, 60e6, 0);
        bytes memory termsSig = _sign(ALICE_KEY, rail.digest(rail.depositHash(t)));

        vm.prank(relayer);
        vm.expectRevert(JuiceRail.MalformedSignature.selector);
        rail.deposit(t, termsSig, new bytes(64));
    }

    // --- funds ---

    function test_insufficientBalanceCountsTheFee() public {
        _credit(ALICE_KEY, alice, 10e6);
        JuiceRail.TransferTerms memory t = _transferTerms(bytes32("t11"), alice, bob, 10e6, 1);
        bytes memory sig = _sign(ALICE_KEY, rail.digest(rail.transferHash(t)));

        vm.prank(relayer);
        vm.expectRevert(JuiceRail.InsufficientBalance.selector);
        rail.transfer(t, sig);
    }

    function test_aFeeTakingTokenCannotFundADeposit() public {
        token.mint(alice, 100e6);
        token.setFeeBps(50);
        JuiceRail.DepositTerms memory t = _depositTerms(bytes32("d6"), alice, alice, 60e6, 0);
        bytes32 h = rail.depositHash(t);
        bytes memory termsSig = _sign(ALICE_KEY, rail.digest(h));
        bytes memory authSig = _auth(ALICE_KEY, alice, 60e6, t.validBefore, h);

        vm.prank(relayer);
        vm.expectRevert(JuiceRail.InexactTransfer.selector);
        rail.deposit(t, termsSig, authSig);
    }

    function test_aBareTransferCreditsNothing() public {
        token.mint(alice, 100e6);
        vm.prank(alice);
        token.transfer(address(rail), 100e6);

        assertEq(rail.balanceOf(alice), 0);
        assertEq(rail.balanceOf(address(rail)), 0);
        assertEq(token.balanceOf(address(rail)), 100e6, "held may exceed the sum of balances");
    }

    // --- aliasing ---

    function test_aliasedPartiesNet() public {
        _credit(ALICE_KEY, alice, 100e6);

        // sender == recipient: only the fee leaves.
        _submitTransfer(ALICE_KEY, _transferTerms(bytes32("a1"), alice, alice, 10e6, 1e4));
        assertEq(rail.balanceOf(alice), 100e6 - 1e4);

        // sender == relayer: only the amount leaves.
        JuiceRail.TransferTerms memory t2 = _transferTerms(bytes32("a2"), alice, bob, 10e6, 1e4);
        t2.relayer = alice;
        _submitTransfer(ALICE_KEY, t2);
        assertEq(rail.balanceOf(alice), 100e6 - 1e4 - 10e6);
        assertEq(rail.balanceOf(bob), 10e6);

        // recipient == relayer: the recipient takes both legs.
        JuiceRail.TransferTerms memory t3 = _transferTerms(bytes32("a3"), alice, relayer, 5e6, 1e4);
        uint256 relayerBefore = rail.balanceOf(relayer);
        _submitTransfer(ALICE_KEY, t3);
        assertEq(rail.balanceOf(relayer), relayerBefore + 5e6 + 1e4);

        // all three aliased: nothing moves at all.
        uint256 before = rail.balanceOf(alice);
        JuiceRail.TransferTerms memory t4 = _transferTerms(bytes32("a4"), alice, alice, 5e6, 1e4);
        t4.relayer = alice;
        _submitTransfer(ALICE_KEY, t4);
        assertEq(rail.balanceOf(alice), before);
    }

    // --- the specification's preimages ---

    function test_typeHashesMatchTheSpecification() public view {
        assertEq(
            rail.DEPOSIT_TYPEHASH(),
            keccak256(
                "Deposit(bytes32 id,address payer,address account,uint256 amount,uint256 fee,address relayer,uint256 validBefore)"
            )
        );
        assertEq(
            rail.TRANSFER_TYPEHASH(),
            keccak256(
                "Transfer(bytes32 id,address sender,address recipient,uint256 amount,uint256 fee,address relayer,uint256 validBefore)"
            )
        );
        assertEq(
            rail.WITHDRAW_TYPEHASH(),
            keccak256(
                "Withdraw(bytes32 id,address account,address destination,uint256 amount,uint256 fee,address relayer,uint256 validBefore)"
            )
        );
    }

    /// The digest binds the domain: (chain id, contract address). The same
    /// terms signed for another deployment cannot execute here.
    function test_theDigestBindsTheDomain() public view {
        bytes32 domainSeparator = keccak256(
            abi.encode(
                keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"),
                keccak256("JuiceRail"),
                keccak256("2"),
                block.chainid,
                address(rail)
            )
        );
        bytes32 h = rail.transferHash(_transferTerms(bytes32("x"), alice, bob, 1, 2));
        assertEq(rail.digest(h), keccak256(abi.encodePacked("\x19\x01", domainSeparator, h)));
    }

    function test_theTermsHashIsTheEip712StructHash() public view {
        JuiceRail.WithdrawTerms memory t = _withdrawTerms(bytes32("h"), alice, outside, 3, 4);
        assertEq(
            rail.withdrawHash(t),
            keccak256(
                abi.encode(
                    rail.WITHDRAW_TYPEHASH(), t.id, t.account, t.destination, t.amount, t.fee, t.relayer, t.validBefore
                )
            )
        );
    }

    /// These constants are pinned identically in go/rail/sign_test.go. If the
    /// encoding drifts on either side, both suites fail rather than one signing
    /// what the other will not accept.
    function test_crossLanguageGoldenVectors() public view {
        address payer = 0x1111111111111111111111111111111111111111;
        address party = 0x2222222222222222222222222222222222222222;
        address carrier = 0x3333333333333333333333333333333333333333;
        bytes32 id = bytes32(uint256(1));
        uint256 value = 1000000;
        uint256 charge = 2500;
        uint256 until_ = 1893456000;

        assertEq(
            rail.depositHash(
                JuiceRail.DepositTerms(id, payer, party, value, charge, carrier, until_)
            ),
            0xa5287ef541fb6292d106a3f21b644a59f8ec509ad3732353a65f16a3a9f96ba7
        );
        assertEq(
            rail.transferHash(
                JuiceRail.TransferTerms(id, payer, party, value, charge, carrier, until_)
            ),
            0xf80f55d5187157c5cfe78049bb8b41af257966d1fa196f4dd848633ee0dd91f5
        );
        assertEq(
            rail.withdrawHash(
                JuiceRail.WithdrawTerms(id, payer, party, value, charge, carrier, until_)
            ),
            0xaf98a560cc4df4da8606057478c0753cc1727069e9a028d0fb4326d47e575b01
        );

        // The domain of a deployment at a fixed address on a fixed chain.
        assertEq(
            keccak256(
                abi.encode(
                    keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"),
                    keccak256("JuiceRail"),
                    keccak256("2"),
                    uint256(31337),
                    address(0x00000000000000000000000000000000000000A1)
                )
            ),
            0xade9308a35ed9af153baebcacd628f248895fc8f1867fc9fd97ce0eae27c8b75
        );
    }

    // --- fuzz ---

    function testFuzz_transferConservesTheSum(uint96 funded, uint96 amount, uint96 fee) public {
        vm.assume(uint256(amount) + fee <= funded && funded > 0);
        _credit(ALICE_KEY, alice, funded);
        uint256 sum = _sumOfBalances();

        _submitTransfer(ALICE_KEY, _transferTerms(bytes32("fz1"), alice, bob, amount, fee));

        assertEq(_sumOfBalances(), sum);
        assertEq(rail.balanceOf(bob), amount);
        assertEq(rail.balanceOf(relayer), fee);
        assertGe(token.balanceOf(address(rail)), _sumOfBalances());
    }

    function testFuzz_depositThenWithdrawRoundTrips(uint96 amount, uint96 fee) public {
        vm.assume(amount > 0 && uint256(amount) + fee < type(uint96).max);
        token.mint(alice, uint256(amount) + fee);
        _submitDeposit(ALICE_KEY, _depositTerms(bytes32("fz2"), alice, alice, amount, fee));
        assertEq(rail.balanceOf(alice), amount);

        _submitWithdraw(ALICE_KEY, _withdrawTerms(bytes32("fz3"), alice, outside, amount, 0));
        assertEq(token.balanceOf(outside), amount);
        assertEq(rail.balanceOf(alice), 0);
        assertGe(token.balanceOf(address(rail)), _sumOfBalances());
    }

    function testFuzz_conflictingTermsAlwaysRevert(uint96 amount, address recipient, uint96 otherAmount) public {
        vm.assume(amount > 0 && amount != otherAmount && recipient != address(0));
        _credit(ALICE_KEY, alice, uint256(amount) + 1);

        bytes32 id = bytes32("fz4");
        _submitTransfer(ALICE_KEY, _transferTerms(id, alice, recipient, amount, 0));

        JuiceRail.TransferTerms memory other = _transferTerms(id, alice, recipient, otherAmount, 0);
        bytes memory sig = _sign(ALICE_KEY, rail.digest(rail.transferHash(other)));
        vm.prank(relayer);
        vm.expectRevert(abi.encodeWithSelector(JuiceRail.TermsConflict.selector, alice, id));
        rail.transfer(other, sig);
    }

    function testFuzz_onlyTheSignerIsDebited(uint96 amount, uint256 submitterKey) public {
        vm.assume(amount > 0);
        submitterKey = bound(submitterKey, 1, type(uint128).max);
        _credit(ALICE_KEY, alice, amount);
        _credit(BOB_KEY, bob, amount);

        uint256 bobBefore = rail.balanceOf(bob);
        JuiceRail.TransferTerms memory t = _transferTerms(bytes32("fz5"), alice, mallory, amount, 0);
        t.relayer = vm.addr(submitterKey);
        _submitTransfer(ALICE_KEY, t);

        assertEq(rail.balanceOf(alice), 0, "the signer is debited");
        assertEq(rail.balanceOf(bob), bobBefore, "no one else is");
    }
}
