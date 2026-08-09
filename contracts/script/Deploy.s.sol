// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Script, console} from "forge-std/Script.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {IEntryPoint} from "account-abstraction/interfaces/IEntryPoint.sol";
import {JuiceRail} from "../src/JuiceRail.sol";
import {JuiceRailPaymaster} from "../src/JuiceRailPaymaster.sol";

/// @notice Deploys one settlement domain: a rail and its paymaster.
///
/// Every surrounding address is configuration, so the same script serves a
/// testnet and a production network. The Safe suite and the EntryPoint are the
/// canonical deployments and are never deployed by this script.
///
/// Required environment:
///   TOKEN               USDT0 on this network
///   ENTRY_POINT         EntryPoint v0.7
///   MULTI_SEND          MultiSendCallOnly v1.4.1
///   PAYMASTER_SIGNER    key that authorises sponsorship
/// Optional:
///   PAYMASTER_DEPOSIT   wei to deposit at the EntryPoint (default 0)
contract Deploy is Script {
    function run() external {
        address token = vm.envAddress("TOKEN");
        address entryPoint = vm.envAddress("ENTRY_POINT");
        address multiSend = vm.envAddress("MULTI_SEND");
        address signer = vm.envAddress("PAYMASTER_SIGNER");
        uint256 deposit = vm.envOr("PAYMASTER_DEPOSIT", uint256(0));

        vm.startBroadcast();

        JuiceRail rail = new JuiceRail(IERC20(token));
        JuiceRailPaymaster paymaster =
            new JuiceRailPaymaster(IEntryPoint(entryPoint), address(rail), token, multiSend, signer);
        if (deposit != 0) {
            paymaster.deposit{value: deposit}();
        }

        vm.stopBroadcast();

        console.log("chainId          ", block.chainid);
        console.log("rail             ", address(rail));
        console.log("paymaster        ", address(paymaster));
        console.log("token            ", token);
        console.log("entryPoint       ", entryPoint);
        console.log("multiSendCallOnly", multiSend);
    }
}
