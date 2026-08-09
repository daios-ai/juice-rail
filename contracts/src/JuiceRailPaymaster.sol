// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {BasePaymaster} from "account-abstraction/core/BasePaymaster.sol";
import {IEntryPoint} from "account-abstraction/interfaces/IEntryPoint.sol";
import {PackedUserOperation} from "account-abstraction/interfaces/PackedUserOperation.sol";
import {_packValidationData} from "account-abstraction/core/Helpers.sol";
import {ECDSA} from "@openzeppelin/contracts/utils/cryptography/ECDSA.sol";
import {MessageHashUtils} from "@openzeppelin/contracts/utils/cryptography/MessageHashUtils.sol";

/// @title JuiceRailPaymaster
/// @notice Verifying paymaster that sponsors gas for exactly three rail
///         operations and nothing else.
/// @dev The sponsorship predicate never parses hostile input into a decision:
///      it reads the parameters at the canonical offsets, rebuilds the only
///      call data those parameters could legitimately produce, and compares the
///      two byte for byte. A non-canonical encoding, an extra sub-call, a
///      trailing byte, or any other target therefore cannot match.
///
///      A bypass would risk the operator's EntryPoint stake and rail
///      availability; it can never reach rail balances or USDT0.
contract JuiceRailPaymaster is BasePaymaster {
    /// Safe4337Module v0.3.0
    bytes4 internal constant EXECUTE_USER_OP = 0x7bb37428; // executeUserOp(address,uint256,bytes,uint8)
    /// MultiSendCallOnly v1.4.1
    bytes4 internal constant MULTI_SEND = 0x8d80ff0a; // multiSend(bytes)
    bytes4 internal constant APPROVE = 0x095ea7b3; // approve(address,uint256)
    bytes4 internal constant DEPOSIT = 0xd954863c; // deposit(bytes32,address,uint256)
    bytes4 internal constant SETTLE = 0xacd28cc5; // settle(bytes32,address,uint256)
    bytes4 internal constant WITHDRAW = 0xa6fb97d1; // withdraw(bytes32,address,uint256)

    /// @dev Canonical encoded lengths. A conforming operation has exactly one
    ///      of these two lengths; every field below sits at a fixed offset.
    uint256 internal constant DIRECT_LEN = 292;
    uint256 internal constant BATCH_LEN = 612;

    /// Fixed offsets, direct call (settle / withdraw).
    uint256 internal constant D_INNER_SELECTOR = 164;
    uint256 internal constant D_ID = 168;
    uint256 internal constant D_PARTY = 200;
    uint256 internal constant D_AMOUNT = 232;

    /// Fixed offsets, batched approve + deposit.
    uint256 internal constant B_ID = 474;
    uint256 internal constant B_ACCOUNT = 506;
    uint256 internal constant B_AMOUNT = 538;

    /// paymasterAndData: [0:20] paymaster, [20:52] gas limits, then our data.
    uint256 internal constant DATA_OFFSET = 52;
    uint256 internal constant PAYMASTER_AND_DATA_LEN = 155; // 52 + 6 + 32 + 65

    address public immutable rail;
    address public immutable token;
    address public immutable multiSend;

    /// @notice Key whose signature authorises sponsorship. Rotatable by the owner.
    address public signer;

    event SignerRotated(address indexed previous, address indexed current);

    error UnsupportedOperation();
    error GasCostTooHigh();
    error MalformedPaymasterData();
    error ZeroSigner();

    constructor(IEntryPoint entryPoint_, address rail_, address token_, address multiSend_, address signer_)
        BasePaymaster(entryPoint_)
    {
        if (signer_ == address(0)) revert ZeroSigner();
        rail = rail_;
        token = token_;
        multiSend = multiSend_;
        signer = signer_;
        emit SignerRotated(address(0), signer_);
    }

    /// @notice Rotate the verifying signer. One of the owner's two powers; the
    ///         other is EntryPoint stake management, inherited from BasePaymaster.
    function setSigner(address signer_) external onlyOwner {
        if (signer_ == address(0)) revert ZeroSigner();
        emit SignerRotated(signer, signer_);
        signer = signer_;
    }

    /// @notice True when this account call data is one of the three sponsored
    ///         shapes. Exposed for testing and symbolic checking.
    function isAllowed(bytes calldata callData) public view returns (bool) {
        if (callData.length == DIRECT_LEN) return _directAllowed(callData);
        if (callData.length == BATCH_LEN) return _batchAllowed(callData);
        return false;
    }

    /// @dev settle / withdraw: a plain call to the rail.
    function _directAllowed(bytes calldata callData) private view returns (bool) {
        bytes4 inner = bytes4(callData[D_INNER_SELECTOR:D_INNER_SELECTOR + 4]);
        if (inner != SETTLE && inner != WITHDRAW) return false;

        bytes memory inner_ = abi.encodeWithSelector(
            inner, _word(callData, D_ID), _address(callData, D_PARTY), uint256(_word(callData, D_AMOUNT))
        );
        return keccak256(callData)
            == keccak256(abi.encodeWithSelector(EXECUTE_USER_OP, rail, uint256(0), inner_, uint8(0)));
    }

    /// @dev approve + deposit, delegatecalled into MultiSendCallOnly so the
    ///      approval originates from the Safe.
    function _batchAllowed(bytes calldata callData) private view returns (bool) {
        uint256 amount = uint256(_word(callData, B_AMOUNT));
        bytes memory transactions = abi.encodePacked(
            _subCall(token, abi.encodeWithSelector(APPROVE, rail, amount)),
            _subCall(
                rail, abi.encodeWithSelector(DEPOSIT, _word(callData, B_ID), _address(callData, B_ACCOUNT), amount)
            )
        );
        bytes memory inner_ = abi.encodeWithSelector(MULTI_SEND, transactions);
        return keccak256(callData)
            == keccak256(abi.encodeWithSelector(EXECUTE_USER_OP, multiSend, uint256(0), inner_, uint8(1)));
    }

    /// @dev One MultiSend record: plain call, no value.
    function _subCall(address to, bytes memory data) private pure returns (bytes memory) {
        return abi.encodePacked(uint8(0), to, uint256(0), data.length, data);
    }

    /// @notice Digest the signer authorises: the operation with both signature
    ///         fields excluded, bound to this domain and this authorisation.
    function getHash(PackedUserOperation calldata userOp, uint48 validUntil, uint256 maxGasCost)
        public
        view
        returns (bytes32)
    {
        return keccak256(
            abi.encode(
                userOp.sender,
                userOp.nonce,
                keccak256(userOp.initCode),
                keccak256(userOp.callData),
                userOp.accountGasLimits,
                uint256(bytes32(userOp.paymasterAndData[20:DATA_OFFSET])),
                userOp.preVerificationGas,
                userOp.gasFees,
                block.chainid,
                address(this),
                address(entryPoint),
                validUntil,
                maxGasCost
            )
        );
    }

    function _validatePaymasterUserOp(PackedUserOperation calldata userOp, bytes32, uint256 maxCost)
        internal
        view
        override
        returns (bytes memory context, uint256 validationData)
    {
        if (!isAllowed(userOp.callData)) revert UnsupportedOperation();

        bytes calldata pmd = userOp.paymasterAndData;
        if (pmd.length != PAYMASTER_AND_DATA_LEN) revert MalformedPaymasterData();

        uint48 validUntil = uint48(bytes6(pmd[DATA_OFFSET:DATA_OFFSET + 6]));
        uint256 maxGasCost = uint256(bytes32(pmd[DATA_OFFSET + 6:DATA_OFFSET + 38]));
        if (maxCost > maxGasCost) revert GasCostTooHigh();

        bytes32 digest = MessageHashUtils.toEthSignedMessageHash(getHash(userOp, validUntil, maxGasCost));
        (address recovered, ECDSA.RecoverError err,) = ECDSA.tryRecover(digest, pmd[DATA_OFFSET + 38:]);
        bool badSignature = err != ECDSA.RecoverError.NoError || recovered != signer;

        // A signature failure is reported, never reverted (ERC-4337 rule).
        return ("", _packValidationData(badSignature, validUntil, 0));
    }

    function _word(bytes calldata data, uint256 offset) private pure returns (bytes32) {
        return bytes32(data[offset:offset + 32]);
    }

    function _address(bytes calldata data, uint256 offset) private pure returns (address) {
        return address(uint160(uint256(_word(data, offset))));
    }
}
