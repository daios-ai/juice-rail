// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {ERC20Permit} from "@openzeppelin/contracts/token/ERC20/extensions/ERC20Permit.sol";

/// @notice A stand-in for USDT0 on a chain that has none: an ordinary ERC-20
/// with six decimals and EIP-2612 permits, which is the whole surface the rail
/// uses. Anyone may mint, so it is only ever a test and staging fixture.
contract MockUSDT0 is ERC20, ERC20Permit {
    constructor() ERC20("Mock USDT0", "USDT0") ERC20Permit("Mock USDT0") {}

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }
}
