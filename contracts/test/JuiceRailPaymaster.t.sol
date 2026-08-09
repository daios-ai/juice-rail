// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {EntryPoint} from "account-abstraction/core/EntryPoint.sol";
import {IEntryPoint} from "account-abstraction/interfaces/IEntryPoint.sol";
import {PackedUserOperation} from "account-abstraction/interfaces/PackedUserOperation.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {MessageHashUtils} from "@openzeppelin/contracts/utils/cryptography/MessageHashUtils.sol";
import {Ownable} from "@openzeppelin/contracts/access/Ownable.sol";
import {JuiceRail} from "../src/JuiceRail.sol";
import {JuiceRailPaymaster} from "../src/JuiceRailPaymaster.sol";
import {MockUSDT0} from "./MockUSDT0.sol";

contract JuiceRailPaymasterTest is Test {
    bytes4 internal constant EXECUTE_USER_OP = 0x7bb37428;
    bytes4 internal constant MULTI_SEND = 0x8d80ff0a;
    bytes4 internal constant APPROVE = 0x095ea7b3;
    bytes4 internal constant DEPOSIT = 0xd954863c;
    bytes4 internal constant SETTLE = 0xacd28cc5;
    bytes4 internal constant WITHDRAW = 0xa6fb97d1;

    EntryPoint internal entryPoint;
    JuiceRail internal rail;
    MockUSDT0 internal token;
    JuiceRailPaymaster internal paymaster;

    address internal multiSend = makeAddr("multiSendCallOnly");
    address internal account = makeAddr("safe");
    address internal attacker = makeAddr("attacker");
    address internal signer;
    uint256 internal signerKey;

    bytes32 internal constant ID = bytes32(uint256(0xabc));
    uint256 internal constant AMOUNT = 1_000_000;
    uint48 internal validUntil;
    uint256 internal constant MAX_GAS_COST = 1 ether;

    function setUp() public {
        (signer, signerKey) = makeAddrAndKey("paymasterSigner");
        entryPoint = new EntryPoint();
        token = new MockUSDT0();
        rail = new JuiceRail(IERC20(address(token)));
        paymaster = new JuiceRailPaymaster(IEntryPoint(address(entryPoint)), address(rail), address(token), multiSend, signer);
        validUntil = uint48(block.timestamp + 5 minutes);
    }

    // --- canonical call data builders (the shapes the Go client produces) ---

    function _direct(bytes4 inner, bytes32 id, address party, uint256 amount) internal view returns (bytes memory) {
        return abi.encodeWithSelector(
            EXECUTE_USER_OP, address(rail), uint256(0), abi.encodeWithSelector(inner, id, party, amount), uint8(0)
        );
    }

    function _subCall(address to, bytes memory data) internal pure returns (bytes memory) {
        return abi.encodePacked(uint8(0), to, uint256(0), data.length, data);
    }

    function _batch(bytes32 id, address acct, uint256 amount) internal view returns (bytes memory) {
        bytes memory txs = abi.encodePacked(
            _subCall(address(token), abi.encodeWithSelector(APPROVE, address(rail), amount)),
            _subCall(address(rail), abi.encodeWithSelector(DEPOSIT, id, acct, amount))
        );
        return abi.encodeWithSelector(
            EXECUTE_USER_OP, multiSend, uint256(0), abi.encodeWithSelector(MULTI_SEND, txs), uint8(1)
        );
    }

    // --- accepted shapes ---

    function test_acceptsTheThreeSponsoredShapes() public view {
        assertTrue(paymaster.isAllowed(_batch(ID, account, AMOUNT)), "approve+deposit");
        assertTrue(paymaster.isAllowed(_direct(SETTLE, ID, account, AMOUNT)), "settle");
        assertTrue(paymaster.isAllowed(_direct(WITHDRAW, ID, account, AMOUNT)), "withdraw");
    }

    function test_canonicalLengthsAndOffsetsAreAsDocumented() public view {
        bytes memory direct = _direct(SETTLE, ID, account, AMOUNT);
        assertEq(direct.length, 292);

        bytes memory batch = _batch(ID, account, AMOUNT);
        assertEq(batch.length, 612);

        // The fixed offsets the predicate reads.
        assertEq(_wordAt(batch, 474), ID);
        assertEq(address(uint160(uint256(_wordAt(batch, 506)))), account);
        assertEq(uint256(_wordAt(batch, 538)), AMOUNT);
        assertEq(_wordAt(direct, 168), ID);
        assertEq(uint256(_wordAt(direct, 232)), AMOUNT);
    }

    function _wordAt(bytes memory data, uint256 offset) internal pure returns (bytes32 word) {
        assembly {
            word := mload(add(add(data, 32), offset))
        }
    }

    // --- rejected shapes ---

    function test_rejectsWrongOuterSelector() public view {
        bytes memory cd = _direct(SETTLE, ID, account, AMOUNT);
        cd[0] = 0xff;
        assertFalse(paymaster.isAllowed(cd));
    }

    function test_rejectsUnknownInnerSelector() public view {
        assertFalse(paymaster.isAllowed(_direct(0xdeadbeef, ID, account, AMOUNT)));
        // deposit is only sponsored inside the batch, never as a bare call
        assertFalse(paymaster.isAllowed(_direct(DEPOSIT, ID, account, AMOUNT)));
    }

    function test_rejectsForeignTarget() public view {
        bytes memory cd = abi.encodeWithSelector(
            EXECUTE_USER_OP,
            attacker,
            uint256(0),
            abi.encodeWithSelector(SETTLE, ID, account, AMOUNT),
            uint8(0)
        );
        assertFalse(paymaster.isAllowed(cd));
    }

    function test_rejectsDelegatecallToAnythingButMultiSend() public view {
        bytes memory cd = abi.encodeWithSelector(
            EXECUTE_USER_OP, address(rail), uint256(0), abi.encodeWithSelector(SETTLE, ID, account, AMOUNT), uint8(1)
        );
        assertFalse(paymaster.isAllowed(cd));
    }

    function test_rejectsPlainCallToMultiSend() public view {
        bytes memory txs = abi.encodePacked(
            _subCall(address(token), abi.encodeWithSelector(APPROVE, address(rail), AMOUNT)),
            _subCall(address(rail), abi.encodeWithSelector(DEPOSIT, ID, account, AMOUNT))
        );
        bytes memory cd = abi.encodeWithSelector(
            EXECUTE_USER_OP, multiSend, uint256(0), abi.encodeWithSelector(MULTI_SEND, txs), uint8(0)
        );
        assertFalse(paymaster.isAllowed(cd));
    }

    function test_rejectsNonZeroValue() public view {
        bytes memory cd = abi.encodeWithSelector(
            EXECUTE_USER_OP, address(rail), uint256(1), abi.encodeWithSelector(SETTLE, ID, account, AMOUNT), uint8(0)
        );
        assertFalse(paymaster.isAllowed(cd));
    }

    function test_rejectsHiddenThirdSubCall() public view {
        bytes memory txs = abi.encodePacked(
            _subCall(address(token), abi.encodeWithSelector(APPROVE, address(rail), AMOUNT)),
            _subCall(address(rail), abi.encodeWithSelector(DEPOSIT, ID, account, AMOUNT)),
            _subCall(address(token), abi.encodeWithSelector(APPROVE, attacker, type(uint256).max))
        );
        bytes memory cd = abi.encodeWithSelector(
            EXECUTE_USER_OP, multiSend, uint256(0), abi.encodeWithSelector(MULTI_SEND, txs), uint8(1)
        );
        assertFalse(paymaster.isAllowed(cd));
    }

    function test_rejectsSubCallReordering() public view {
        bytes memory txs = abi.encodePacked(
            _subCall(address(rail), abi.encodeWithSelector(DEPOSIT, ID, account, AMOUNT)),
            _subCall(address(token), abi.encodeWithSelector(APPROVE, address(rail), AMOUNT))
        );
        bytes memory cd = abi.encodeWithSelector(
            EXECUTE_USER_OP, multiSend, uint256(0), abi.encodeWithSelector(MULTI_SEND, txs), uint8(1)
        );
        assertFalse(paymaster.isAllowed(cd));
    }

    function test_rejectsApprovalToForeignSpender() public view {
        bytes memory txs = abi.encodePacked(
            _subCall(address(token), abi.encodeWithSelector(APPROVE, attacker, AMOUNT)),
            _subCall(address(rail), abi.encodeWithSelector(DEPOSIT, ID, account, AMOUNT))
        );
        bytes memory cd = abi.encodeWithSelector(
            EXECUTE_USER_OP, multiSend, uint256(0), abi.encodeWithSelector(MULTI_SEND, txs), uint8(1)
        );
        assertFalse(paymaster.isAllowed(cd));
    }

    function test_rejectsApprovalLargerThanDeposit() public view {
        bytes memory txs = abi.encodePacked(
            _subCall(address(token), abi.encodeWithSelector(APPROVE, address(rail), type(uint256).max)),
            _subCall(address(rail), abi.encodeWithSelector(DEPOSIT, ID, account, AMOUNT))
        );
        bytes memory cd = abi.encodeWithSelector(
            EXECUTE_USER_OP, multiSend, uint256(0), abi.encodeWithSelector(MULTI_SEND, txs), uint8(1)
        );
        assertFalse(paymaster.isAllowed(cd));
    }

    function test_rejectsDelegatecallInsideTheBatch() public view {
        bytes memory approveData = abi.encodeWithSelector(APPROVE, address(rail), AMOUNT);
        bytes memory txs = abi.encodePacked(
            uint8(1), // delegatecall sub-call
            address(token),
            uint256(0),
            approveData.length,
            approveData,
            _subCall(address(rail), abi.encodeWithSelector(DEPOSIT, ID, account, AMOUNT))
        );
        bytes memory cd = abi.encodeWithSelector(
            EXECUTE_USER_OP, multiSend, uint256(0), abi.encodeWithSelector(MULTI_SEND, txs), uint8(1)
        );
        assertFalse(paymaster.isAllowed(cd));
    }

    function test_rejectsTrailingBytes() public view {
        bytes memory cd = abi.encodePacked(_direct(SETTLE, ID, account, AMOUNT), bytes32(0));
        assertFalse(paymaster.isAllowed(cd));
    }

    function test_rejectsEmptyAndShortCallData() public view {
        assertFalse(paymaster.isAllowed(""));
        assertFalse(paymaster.isAllowed(hex"7bb37428"));
    }

    /// The encoding frame is rigid: only the parameter words may vary.
    function testFuzz_rejectsAnyMutationOutsideParameterFields(uint256 position, uint8 delta) public view {
        bytes memory cd = _direct(SETTLE, ID, account, AMOUNT);
        // Parameters live at [164,264): inner selector plus three words.
        position = bound(position, 0, cd.length - 1);
        vm.assume(position < 164 || position >= 264);
        vm.assume(delta != 0);

        cd[position] = bytes1(uint8(cd[position]) ^ delta);
        assertFalse(paymaster.isAllowed(cd));
    }

    function testFuzz_rejectsAnyMutationOutsideBatchParameterFields(uint256 position, uint8 delta) public view {
        bytes memory cd = _batch(ID, account, AMOUNT);
        // Only the deposit arguments at [474,570) are free; the approval amount
        // is rebuilt from them, so its bytes are fixed too.
        position = bound(position, 0, cd.length - 1);
        vm.assume(position < 474 || position >= 570);
        vm.assume(delta != 0);

        cd[position] = bytes1(uint8(cd[position]) ^ delta);
        assertFalse(paymaster.isAllowed(cd));
    }

    /// Any parameter values are sponsorable — the rail, not the paymaster,
    /// decides whether the operation itself is legitimate.
    function testFuzz_acceptsAnyParameters(bytes32 id, address party, uint256 amount) public view {
        assertTrue(paymaster.isAllowed(_batch(id, party, amount)));
        assertTrue(paymaster.isAllowed(_direct(SETTLE, id, party, amount)));
        assertTrue(paymaster.isAllowed(_direct(WITHDRAW, id, party, amount)));
    }

    // --- validation, signature, expiry ---

    function _op(bytes memory callData, bytes memory paymasterData)
        internal
        view
        returns (PackedUserOperation memory op)
    {
        op.sender = account;
        op.nonce = 1;
        op.initCode = "";
        op.callData = callData;
        op.accountGasLimits = bytes32(uint256(500_000) << 128 | uint256(500_000));
        op.preVerificationGas = 50_000;
        op.gasFees = bytes32(uint256(1 gwei) << 128 | uint256(1 gwei));
        op.paymasterAndData =
            abi.encodePacked(address(paymaster), uint128(300_000), uint128(100_000), paymasterData);
        op.signature = "";
    }

    function _sponsored(bytes memory callData, uint256 key) internal view returns (PackedUserOperation memory op) {
        op = _op(callData, abi.encodePacked(validUntil, MAX_GAS_COST, bytes(hex"")));
        bytes32 digest = MessageHashUtils.toEthSignedMessageHash(paymaster.getHash(op, validUntil, MAX_GAS_COST));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(key, digest);
        op.paymasterAndData =
            abi.encodePacked(address(paymaster), uint128(300_000), uint128(100_000), validUntil, MAX_GAS_COST, r, s, v);
    }

    function _validate(PackedUserOperation memory op, uint256 maxCost) internal returns (uint256 validationData) {
        vm.prank(address(entryPoint));
        (, validationData) = paymaster.validatePaymasterUserOp(op, bytes32(0), maxCost);
    }

    function test_validatesSponsoredOperation() public {
        uint256 validationData = _validate(_sponsored(_batch(ID, account, AMOUNT), signerKey), 0.5 ether);
        assertEq(uint160(validationData), 0, "signature must validate");
        assertEq(uint48(validationData >> 160), validUntil, "expiry must be reported");
    }

    function test_reportsRatherThanRevertsOnWrongSigner() public {
        (, uint256 wrongKey) = makeAddrAndKey("impostor");
        uint256 validationData = _validate(_sponsored(_batch(ID, account, AMOUNT), wrongKey), 0.5 ether);
        assertEq(uint160(validationData), 1, "SIG_VALIDATION_FAILED");
    }

    function test_rejectsTamperedSignature() public {
        PackedUserOperation memory op = _sponsored(_batch(ID, account, AMOUNT), signerKey);
        bytes memory pmd = op.paymasterAndData;
        pmd[pmd.length - 2] = bytes1(uint8(pmd[pmd.length - 2]) ^ 0xff);
        op.paymasterAndData = pmd;

        uint256 validationData = _validate(op, 0.5 ether);
        assertEq(uint160(validationData), 1);
    }

    function test_rejectsUnsupportedOperation() public {
        PackedUserOperation memory op = _sponsored(_direct(0xdeadbeef, ID, account, AMOUNT), signerKey);
        vm.prank(address(entryPoint));
        vm.expectRevert(JuiceRailPaymaster.UnsupportedOperation.selector);
        paymaster.validatePaymasterUserOp(op, bytes32(0), 0.5 ether);
    }

    function test_rejectsGasCostAboveAuthorisedCeiling() public {
        PackedUserOperation memory op = _sponsored(_batch(ID, account, AMOUNT), signerKey);
        vm.prank(address(entryPoint));
        vm.expectRevert(JuiceRailPaymaster.GasCostTooHigh.selector);
        paymaster.validatePaymasterUserOp(op, bytes32(0), MAX_GAS_COST + 1);
    }

    function test_rejectsMalformedPaymasterData() public {
        PackedUserOperation memory op = _op(_batch(ID, account, AMOUNT), hex"0011");
        vm.prank(address(entryPoint));
        vm.expectRevert(JuiceRailPaymaster.MalformedPaymasterData.selector);
        paymaster.validatePaymasterUserOp(op, bytes32(0), 0);
    }

    function test_onlyEntryPointMayValidate() public {
        PackedUserOperation memory op = _sponsored(_batch(ID, account, AMOUNT), signerKey);
        vm.expectRevert("Sender not EntryPoint");
        paymaster.validatePaymasterUserOp(op, bytes32(0), 0);
    }

    /// A signature for one operation must not sponsor another.
    function test_signatureIsBoundToTheOperation() public {
        PackedUserOperation memory signed = _sponsored(_batch(ID, account, AMOUNT), signerKey);
        PackedUserOperation memory other = _op(_batch(ID, account, AMOUNT + 1), "");
        other.paymasterAndData = signed.paymasterAndData;

        assertEq(uint160(_validate(other, 0.5 ether)), 1);
    }

    // --- owner powers ---

    function test_ownerRotatesSigner() public {
        address next = makeAddr("nextSigner");
        paymaster.setSigner(next);
        assertEq(paymaster.signer(), next);

        // Operations signed by the retired key stop validating.
        assertEq(uint160(_validate(_sponsored(_batch(ID, account, AMOUNT), signerKey), 0.5 ether)), 1);
    }

    function test_nonOwnerCannotRotateSigner() public {
        address stranger = makeAddr("stranger");
        vm.prank(stranger);
        vm.expectRevert(abi.encodeWithSelector(Ownable.OwnableUnauthorizedAccount.selector, stranger));
        paymaster.setSigner(stranger);
    }

    function test_signerIsNeverZero() public {
        vm.expectRevert(JuiceRailPaymaster.ZeroSigner.selector);
        paymaster.setSigner(address(0));

        vm.expectRevert(JuiceRailPaymaster.ZeroSigner.selector);
        new JuiceRailPaymaster(IEntryPoint(address(entryPoint)), address(rail), address(token), multiSend, address(0));
    }

    function test_ownerManagesEntryPointStake() public {
        vm.deal(address(this), 2 ether);
        paymaster.deposit{value: 1 ether}();
        assertEq(entryPoint.balanceOf(address(paymaster)), 1 ether);

        paymaster.withdrawTo(payable(address(this)), 0.4 ether);
        assertEq(entryPoint.balanceOf(address(paymaster)), 0.6 ether);
    }

    function test_paymasterHoldsNoRailPowers() public view {
        // The paymaster is not an account on the rail and cannot become one
        // through any function it exposes.
        assertEq(rail.balanceOf(address(paymaster)), 0);
        assertEq(token.balanceOf(address(paymaster)), 0);
    }

    receive() external payable {}
}
