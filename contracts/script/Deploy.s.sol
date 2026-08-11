// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Script, console} from "forge-std/Script.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {JuiceRail} from "../src/JuiceRail.sol";

/// @notice Deploys one domain: the rail, and nothing else.
///
/// The token is configuration, so the same script serves a testnet and a
/// production network. There is no operator contract and no privileged party:
/// once this returns, the deployer has no power over the rail.
///
/// Required environment:
///   TOKEN   the stablecoin on this network; it must implement EIP-3009
///           receiveWithAuthorization
contract Deploy is Script {
    function run() external {
        address token = vm.envAddress("TOKEN");

        vm.startBroadcast();
        JuiceRail rail = new JuiceRail(IERC20(token));
        vm.stopBroadcast();

        console.log("chainId", block.chainid);
        console.log("rail   ", address(rail));
        console.log("token  ", token);
    }
}
