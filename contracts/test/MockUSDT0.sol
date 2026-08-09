// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/// @notice Test token standing in for USDT0: six decimals, freely mintable,
///         and able to take a transfer fee so the deposit balance-delta check
///         can be exercised.
contract MockUSDT0 is ERC20 {
    uint256 public feeBps;

    constructor() ERC20("Mock USDT0", "USDT0") {}

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }

    function setFeeBps(uint256 bps) external {
        feeBps = bps;
    }

    function _update(address from, address to, uint256 value) internal override {
        uint256 fee = (from == address(0) || to == address(0)) ? 0 : (value * feeBps) / 10_000;
        if (fee != 0) {
            super._update(from, address(0), fee);
            value -= fee;
        }
        super._update(from, to, value);
    }
}
