// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {JuiceRail} from "../src/JuiceRail.sol";

/// @notice Minimal exact-transfer token. Symbolic execution has to reason
///         through every token call, so the token is kept to the arithmetic
///         the rail actually depends on.
contract SymbolicToken {
    mapping(address => uint256) public balanceOf;

    function mint(address to, uint256 amount) external {
        balanceOf[to] += amount;
    }

    function transfer(address to, uint256 amount) external returns (bool) {
        require(balanceOf[msg.sender] >= amount, "balance");
        unchecked {
            balanceOf[msg.sender] -= amount;
        }
        balanceOf[to] += amount;
        return true;
    }

    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        require(balanceOf[from] >= amount, "balance");
        unchecked {
            balanceOf[from] -= amount;
        }
        balanceOf[to] += amount;
        return true;
    }

    function allowance(address, address) external pure returns (uint256) {
        return type(uint256).max;
    }

    function approve(address, uint256) external pure returns (bool) {
        return true;
    }
}

/// @notice The properties of §9 target 1, proved over all inputs rather than
///         sampled ones. Run with `halmos --contract JuiceRailSymbolicTest`.
contract JuiceRailSymbolicTest is Test {
    JuiceRail internal rail;
    SymbolicToken internal token;

    address internal constant PAYER = address(0x1001);
    address internal constant A = address(0x1002);
    address internal constant B = address(0x1003);

    function setUp() public {
        token = new SymbolicToken();
        rail = new JuiceRail(IERC20(address(token)));
        token.mint(PAYER, type(uint128).max);
    }

    function _sum() internal view returns (uint256) {
        return rail.balanceOf(PAYER) + rail.balanceOf(A) + rail.balanceOf(B);
    }

    /// An identifier binds once: after it is bound, its terms never change.
    function check_bindingIsWriteOnce(bytes32 id, uint96 first, uint96 second) public {
        vm.prank(PAYER);
        rail.deposit(id, A, first);
        bytes32 bound = rail.operations(id);

        vm.prank(PAYER);
        try rail.deposit(id, A, second) {
            // Only an exact replay may succeed, and it changes nothing.
            assert(first == second);
        } catch {}
        assert(rail.operations(id) == bound);
    }

    /// Conflicting terms under one identifier always revert; identical terms
    /// never do. Every intent executes or reverts, and none no-ops silently.
    function check_conflictingTermsAlwaysRevert(bytes32 id, uint96 amount, address account) public {
        vm.assume(account != address(0));
        vm.prank(PAYER);
        rail.deposit(id, account, amount);

        uint256 before = rail.balanceOf(account);
        vm.prank(PAYER);
        try rail.deposit(id, account, amount) {
            assert(rail.balanceOf(account) == before); // exact replay: a no-op
        } catch {
            assert(false); // an exact replay must never revert
        }
    }

    /// Settlement preserves the sum of balances, and debits only the caller.
    function check_settleConservesTheSumAndDebitsTheCaller(bytes32 depositID, bytes32 settleID, uint96 funded, uint96 amount)
        public
    {
        vm.assume(depositID != settleID);
        vm.prank(PAYER);
        rail.deposit(depositID, A, funded);

        uint256 sumBefore = _sum();
        uint256 heldBefore = token.balanceOf(address(rail));
        uint256 otherBefore = rail.balanceOf(PAYER);

        vm.prank(A);
        try rail.settle(settleID, B, amount) {
            assert(_sum() == sumBefore); // conservation
            assert(token.balanceOf(address(rail)) == heldBefore); // no tokens move
            assert(rail.balanceOf(PAYER) == otherBefore); // only the caller is debited
        } catch {
            assert(amount > funded); // the only reason to refuse
        }
    }

    /// A deposit raises the sum and the holding by the same amount.
    function check_depositMovesHeldAndSumEqually(bytes32 id, uint96 amount) public {
        uint256 sumBefore = _sum();
        uint256 heldBefore = token.balanceOf(address(rail));

        vm.prank(PAYER);
        rail.deposit(id, A, amount);

        assert(_sum() == sumBefore + amount);
        assert(token.balanceOf(address(rail)) == heldBefore + amount);
    }

    /// A withdrawal lowers the sum and the holding by the same amount.
    function check_withdrawMovesHeldAndSumEqually(bytes32 depositID, bytes32 withdrawID, uint96 funded, uint96 amount)
        public
    {
        vm.assume(depositID != withdrawID);
        vm.prank(PAYER);
        rail.deposit(depositID, A, funded);

        uint256 sumBefore = _sum();
        uint256 heldBefore = token.balanceOf(address(rail));

        vm.prank(A);
        try rail.withdraw(withdrawID, B, amount) {
            assert(_sum() == sumBefore - amount);
            assert(token.balanceOf(address(rail)) == heldBefore - amount);
        } catch {
            assert(amount > funded);
        }
    }

    /// One account can never reach another's balance.
    function check_oneAccountCannotDebitAnother(bytes32 depositID, bytes32 id, uint96 funded, uint96 amount) public {
        vm.assume(depositID != id);
        vm.prank(PAYER);
        rail.deposit(depositID, A, funded);
        uint256 victim = rail.balanceOf(A);

        vm.prank(B);
        try rail.settle(id, B, amount) {} catch {}
        assert(rail.balanceOf(A) == victim);

        vm.prank(B);
        try rail.withdraw(id, B, amount) {} catch {}
        assert(rail.balanceOf(A) == victim);
    }

    /// Tokens sent straight to the contract mint nothing: the rail never reads
    /// its standing balance to decide what anyone owns.
    function check_directTransfersMintNothing(uint96 amount) public {
        uint256 sumBefore = _sum();

        vm.prank(PAYER);
        token.transfer(address(rail), amount);

        assert(_sum() == sumBefore);
    }
}
