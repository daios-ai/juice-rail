// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Script, console} from "forge-std/Script.sol";
import {MockUSDT0} from "../src/MockUSDT0.sol";

interface IWETH9 {
    function deposit() external payable;
    function approve(address spender, uint256 amount) external returns (bool);
}

interface IPositionManager {
    struct MintParams {
        address token0;
        address token1;
        uint24 fee;
        int24 tickLower;
        int24 tickUpper;
        uint256 amount0Desired;
        uint256 amount1Desired;
        uint256 amount0Min;
        uint256 amount1Min;
        address recipient;
        uint256 deadline;
    }

    function createAndInitializePoolIfNecessary(address token0, address token1, uint24 fee, uint160 sqrtPriceX96)
        external
        payable
        returns (address pool);

    function mint(MintParams calldata params)
        external
        payable
        returns (uint256 tokenId, uint128 liquidity, uint256 amount0, uint256 amount1);
}

/// @notice One-time staging setup for Arbitrum Sepolia, which has no USDT0 and
/// no pool to buy gas from. It deploys a mock token and seeds a real Uniswap V3
/// pool against the canonical deployment, so the rail exercises the real
/// router. Run it once, then copy the printed addresses into the domain file.
///
///     forge script script/SepoliaBootstrap.s.sol --rpc-url $RPC --broadcast \
///       --private-key $KEY
contract SepoliaBootstrap is Script {
    address constant WETH = 0x980B62Da83eFf3D4576C647993b0c1D7faf17c73;
    address constant POSITION_MANAGER = 0x6b2937Bde17889EDCf8fbD8dE31C3C2a70Bc4d65;
    uint24 constant FEE = 500;
    // Full range at a tick spacing of 10.
    int24 constant TICK_LOWER = -887270;
    int24 constant TICK_UPPER = 887270;

    /// @dev The reference price: one unit of native currency is worth this many
    /// display units of the token.
    uint256 constant PRICE = 3000;

    function run() external {
        uint256 wethAmount = vm.envOr("WETH_AMOUNT", uint256(0.004 ether));
        uint256 tokenAmount = wethAmount * PRICE * 1e6 / 1e18;

        vm.startBroadcast();

        MockUSDT0 token = new MockUSDT0();
        token.mint(msg.sender, tokenAmount * 1000); // plenty left over to fund accounts

        IWETH9(WETH).deposit{value: wethAmount}();
        IWETH9(WETH).approve(POSITION_MANAGER, type(uint256).max);
        token.approve(POSITION_MANAGER, type(uint256).max);

        (address token0, address token1) = address(token) < WETH ? (address(token), WETH) : (WETH, address(token));
        (uint256 amount0, uint256 amount1) =
            token0 == address(token) ? (tokenAmount, wethAmount) : (wethAmount, tokenAmount);

        // A Uniswap V3 pool is initialised with the square root of the price of
        // token1 in token0, scaled by 2**96.
        uint160 sqrtPriceX96 = uint160(sqrt((amount1 << 192) / amount0));
        address pool = IPositionManager(POSITION_MANAGER).createAndInitializePoolIfNecessary(
            token0, token1, FEE, sqrtPriceX96
        );

        IPositionManager(POSITION_MANAGER).mint(
            IPositionManager.MintParams({
                token0: token0,
                token1: token1,
                fee: FEE,
                tickLower: TICK_LOWER,
                tickUpper: TICK_UPPER,
                amount0Desired: amount0,
                amount1Desired: amount1,
                amount0Min: 0,
                amount1Min: 0,
                recipient: msg.sender,
                deadline: block.timestamp + 3600
            })
        );

        vm.stopBroadcast();

        console.log("token", address(token));
        console.log("pool ", pool);
        console.log("weth ", WETH);
    }

    /// @dev Babylonian square root, enough for one initialisation.
    function sqrt(uint256 x) internal pure returns (uint256 y) {
        if (x == 0) return 0;
        uint256 z = (x + 1) / 2;
        y = x;
        while (z < y) {
            y = z;
            z = (x / z + z) / 2;
        }
    }
}
