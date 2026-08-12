// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

interface IPermitToken {
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function permit(
        address owner,
        address spender,
        uint256 value,
        uint256 deadline,
        uint8 v,
        bytes32 r,
        bytes32 s
    ) external;
}

/// @notice A stand-in for a Uniswap V3 SwapRouter and QuoterV2 on a local
/// chain. It accepts exactly the calldata the rail builds for the real router
/// — multicall of selfPermit, exactOutputSingle and unwrapWETH9 — and settles
/// it at a fixed price, so the encoding is exercised for real without a pool.
///
/// The wrapped native token is a label here: the router holds native currency
/// directly and pays it out on unwrap. Fund it at deployment.
contract MockRouter {
    struct ExactOutputSingleParams {
        address tokenIn;
        address tokenOut;
        uint24 fee;
        address recipient;
        uint256 deadline;
        uint256 amountOut;
        uint256 amountInMaximum;
        uint160 sqrtPriceLimitX96;
    }

    struct QuoteExactOutputSingleParams {
        address tokenIn;
        address tokenOut;
        uint256 amount;
        uint24 fee;
        uint160 sqrtPriceLimitX96;
    }

    address public immutable token;
    address public immutable weth;
    uint24 public immutable feeTier;

    /// @notice token base units charged for 1e18 of native currency.
    uint256 public price;

    /// @notice native currency bought but not yet unwrapped, per caller.
    mapping(address => uint256) public owed;

    constructor(address token_, address weth_, uint24 feeTier_, uint256 price_) payable {
        token = token_;
        weth = weth_;
        feeTier = feeTier_;
        price = price_;
    }

    receive() external payable {}

    function setPrice(uint256 price_) external {
        price = price_;
    }

    /// @notice Batches calls the way the real router does: delegatecall to
    /// itself, so the caller stays the payer throughout.
    function multicall(bytes[] calldata data) external payable returns (bytes[] memory results) {
        results = new bytes[](data.length);
        for (uint256 i = 0; i < data.length; i++) {
            (bool ok, bytes memory result) = address(this).delegatecall(data[i]);
            if (!ok) {
                assembly {
                    revert(add(result, 32), mload(result))
                }
            }
            results[i] = result;
        }
    }

    function selfPermit(
        address token_,
        uint256 value,
        uint256 deadline,
        uint8 v,
        bytes32 r,
        bytes32 s
    ) external payable {
        IPermitToken(token_).permit(msg.sender, address(this), value, deadline, v, r, s);
    }

    function quote(uint256 amountOut) public view returns (uint256) {
        return (amountOut * price + 1e18 - 1) / 1e18;
    }

    function quoteExactOutputSingle(QuoteExactOutputSingleParams calldata params)
        external
        view
        returns (uint256 amountIn, uint160 sqrtPriceX96After, uint32 initializedTicksCrossed, uint256 gasEstimate)
    {
        require(params.tokenIn == token && params.tokenOut == weth && params.fee == feeTier, "pair");
        return (quote(params.amount), 0, 0, 0);
    }

    function exactOutputSingle(ExactOutputSingleParams calldata params)
        external
        payable
        returns (uint256 amountIn)
    {
        require(block.timestamp <= params.deadline, "deadline");
        require(params.tokenIn == token && params.tokenOut == weth && params.fee == feeTier, "pair");
        require(params.recipient == address(this), "recipient");
        amountIn = quote(params.amountOut);
        require(amountIn <= params.amountInMaximum, "slippage");
        require(IPermitToken(token).transferFrom(msg.sender, address(this), amountIn), "pull");
        owed[msg.sender] += params.amountOut;
    }

    function unwrapWETH9(uint256 amountMinimum, address recipient) external payable {
        uint256 amount = owed[msg.sender];
        require(amount >= amountMinimum, "insufficient");
        owed[msg.sender] = 0;
        (bool ok,) = recipient.call{value: amount}("");
        require(ok, "send");
    }
}
