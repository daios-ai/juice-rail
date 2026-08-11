// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {JuiceRail} from "../src/JuiceRail.sol";

/// @notice Minimal exact-transfer token with the payee authorisation the rail
///         depends on. Symbolic execution has to reason through every token
///         call, so this is the arithmetic and nothing else. The authorisation
///         signature is not checked here: that the token verifies it is a trust
///         base assumption (§14.3), not a property of the rail.
contract SymbolicToken {
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(bytes32 => bool)) public authorizationState;

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

    function receiveWithAuthorization(
        address from,
        address to,
        uint256 value,
        uint256 validAfter,
        uint256 validBefore,
        bytes32 nonce,
        uint8,
        bytes32,
        bytes32
    ) external {
        require(to == msg.sender, "payee");
        require(block.timestamp > validAfter && block.timestamp < validBefore, "window");
        require(!authorizationState[from][nonce], "used");
        require(balanceOf[from] >= value, "balance");
        authorizationState[from][nonce] = true;
        unchecked {
            balanceOf[from] -= value;
        }
        balanceOf[to] += value;
    }
}

/// @notice The properties of §14 target 1, proved over all inputs rather than
///         sampled ones. Run with `halmos --contract JuiceRailSymbolicTest`.
///
/// @dev Operations are signed with `vm.sign` over symbolic terms. Nothing is
///      ever concluded from what an arbitrary signature recovers to: that no
///      signature but the account's own recovers to it is the `ecrecover` line
///      of the trust base, which no symbolic checker can discharge. What is
///      proved here is that the rail moves money only as some account signed.
contract JuiceRailSymbolicTest is Test {
    JuiceRail internal rail;
    SymbolicToken internal token;

    uint256 internal constant PK_A = 0xA;
    uint256 internal constant PK_B = 0xB;
    uint256 internal constant PK_V = 0xC;

    address internal A;
    address internal B;
    address internal V; // an uninvolved third account
    address internal constant R = address(0x9E1A4);

    /// The identifier the funding deposits use, kept out of the symbolic space.
    bytes32 internal constant FUND_ID = bytes32(uint256(0xF00D));

    function setUp() public {
        token = new SymbolicToken();
        rail = new JuiceRail(IERC20(address(token)));
        A = vm.addr(PK_A);
        B = vm.addr(PK_B);
        V = vm.addr(PK_V);

        // The properties below are about distinct parties, so the parties are
        // distinct. What happens when they alias is a separate question, and
        // is settled concretely by test_aliasedPartiesNet.
        address[5] memory parties = [A, B, V, R, address(rail)];
        for (uint256 i = 0; i < parties.length; i++) {
            vm.assume(parties[i] != address(token));
            for (uint256 j = i + 1; j < parties.length; j++) {
                vm.assume(parties[i] != parties[j]);
            }
        }

        token.mint(A, type(uint128).max);
        token.mint(B, type(uint128).max);
        token.mint(V, type(uint128).max);
    }

    function _sum() internal view returns (uint256) {
        return rail.balanceOf(A) + rail.balanceOf(B) + rail.balanceOf(V) + rail.balanceOf(R);
    }

    function _sign(uint256 pk, bytes32 d) internal pure returns (bytes memory) {
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(pk, d);
        return abi.encodePacked(r, s, v);
    }

    /// Funds an account through a real deposit, which is the only way in.
    function _fund(uint256 pk, uint96 amount) internal {
        JuiceRail.DepositTerms memory t = JuiceRail.DepositTerms({
            id: FUND_ID,
            payer: vm.addr(pk),
            account: vm.addr(pk),
            amount: amount,
            fee: 0,
            relayer: R,
            validBefore: block.timestamp + 1
        });
        bytes memory sig = _sign(pk, rail.digest(rail.depositHash(t)));
        vm.prank(R);
        rail.deposit(t, sig, new bytes(65));
    }

    function _transfer(bytes32 id, address sender, address recipient, uint96 amount, uint96 fee, address relayer)
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
            validBefore: block.timestamp + 1
        });
    }

    /// An identifier binds once, per account: after it is bound its terms never
    /// change, and only an exact replay may succeed.
    function check_bindingIsWriteOnce(bytes32 id, uint96 first, uint96 second) public {
        vm.assume(id != FUND_ID);
        _fund(PK_A, type(uint96).max);

        JuiceRail.TransferTerms memory t1 = _transfer(id, A, B, first, 0, R);
        bytes memory sig1 = _sign(PK_A, rail.digest(rail.transferHash(t1)));
        vm.prank(R);
        rail.transfer(t1, sig1);
        bytes32 bound = rail.operations(A, id);
        assert(bound == rail.transferHash(t1));

        JuiceRail.TransferTerms memory t2 = _transfer(id, A, B, second, 0, R);
        bytes memory sig2 = _sign(PK_A, rail.digest(rail.transferHash(t2)));
        vm.prank(R);
        try rail.transfer(t2, sig2) {
            assert(first == second); // only an exact replay may succeed
        } catch {}
        assert(rail.operations(A, id) == bound);
    }

    /// An exact replay of an executed operation never reverts and moves
    /// nothing, whatever the clock says and whoever presents it.
    function check_replayIsAlwaysASilentNoOp(bytes32 id, uint96 amount, uint256 later, address anyone) public {
        vm.assume(id != FUND_ID);
        _fund(PK_A, type(uint96).max);

        JuiceRail.TransferTerms memory t = _transfer(id, A, B, amount, 0, R);
        bytes memory sig = _sign(PK_A, rail.digest(rail.transferHash(t)));
        vm.prank(R);
        rail.transfer(t, sig);

        uint256 senderBefore = rail.balanceOf(A);
        uint256 recipientBefore = rail.balanceOf(B);
        vm.assume(later >= block.timestamp);
        vm.warp(later);

        vm.prank(anyone);
        rail.transfer(t, sig); // must not revert, for anyone, at any time
        assert(rail.balanceOf(A) == senderBefore);
        assert(rail.balanceOf(B) == recipientBefore);
    }

    /// A transfer preserves the sum of balances and moves no tokens. A funded,
    /// live, correctly signed transfer always executes.
    function check_transferConservesTheSum(bytes32 id, uint96 funded, uint96 amount, uint96 fee) public {
        vm.assume(id != FUND_ID);
        vm.assume(uint256(amount) + fee <= funded);
        _fund(PK_A, funded);

        uint256 sumBefore = _sum();
        uint256 heldBefore = token.balanceOf(address(rail));

        JuiceRail.TransferTerms memory t = _transfer(id, A, B, amount, fee, R);
        bytes memory sig = _sign(PK_A, rail.digest(rail.transferHash(t)));
        vm.prank(R);
        rail.transfer(t, sig);

        assert(_sum() == sumBefore);
        assert(token.balanceOf(address(rail)) == heldBefore);
        assert(rail.balanceOf(R) == fee);
    }

    /// A deposit raises the sum and the holding by the same amount.
    function check_depositMovesHeldAndSumEqually(bytes32 id, uint96 amount, uint96 fee) public {
        vm.assume(id != FUND_ID);
        uint256 sumBefore = _sum();
        uint256 heldBefore = token.balanceOf(address(rail));

        JuiceRail.DepositTerms memory t = JuiceRail.DepositTerms({
            id: id,
            payer: A,
            account: B,
            amount: amount,
            fee: fee,
            relayer: R,
            validBefore: block.timestamp + 1
        });
        bytes memory sig = _sign(PK_A, rail.digest(rail.depositHash(t)));
        vm.prank(R);
        rail.deposit(t, sig, new bytes(65));

        assert(_sum() == sumBefore + uint256(amount) + fee);
        assert(token.balanceOf(address(rail)) == heldBefore + uint256(amount) + fee);
    }

    /// A withdrawal lowers the sum and the holding by the same amount; the fee
    /// stays inside as the relayer's balance.
    function check_withdrawMovesHeldAndSumEqually(bytes32 id, uint96 funded, uint96 amount, uint96 fee) public {
        vm.assume(id != FUND_ID);
        vm.assume(uint256(amount) + fee <= funded);
        _fund(PK_A, funded);

        uint256 sumBefore = _sum();
        uint256 heldBefore = token.balanceOf(address(rail));

        JuiceRail.WithdrawTerms memory t = JuiceRail.WithdrawTerms({
            id: id,
            account: A,
            destination: V,
            amount: amount,
            fee: fee,
            relayer: R,
            validBefore: block.timestamp + 1
        });
        bytes memory sig = _sign(PK_A, rail.digest(rail.withdrawHash(t)));
        vm.prank(R);
        rail.withdraw(t, sig);

        assert(_sum() == sumBefore - amount);
        assert(token.balanceOf(address(rail)) == heldBefore - amount);
    }

    /// No operation reaches an account that is not a party to it. With the rail
    /// requiring the debited account's own signature, this is "an account
    /// cannot debit another".
    function check_oneAccountCannotDebitAnother(bytes32 id, uint96 funded, uint96 amount, uint96 fee) public {
        vm.assume(id != FUND_ID);
        _fund(PK_A, funded);
        _fund(PK_V, funded);
        uint256 victim = rail.balanceOf(V);

        JuiceRail.TransferTerms memory t = _transfer(id, A, B, amount, fee, R);
        bytes memory sig = _sign(PK_A, rail.digest(rail.transferHash(t)));
        vm.prank(R);
        try rail.transfer(t, sig) {} catch {}
        assert(rail.balanceOf(V) == victim);
    }

    /// The party that pays gas gains no authority: a submitter that is not the
    /// named relayer moves nothing, and is never credited.
    function check_submissionConfersNoAuthority(bytes32 id, uint96 funded, uint96 amount, uint96 fee, address submitter)
        public
    {
        vm.assume(id != FUND_ID);
        vm.assume(submitter != R);
        vm.assume(uint256(amount) + fee <= funded);
        _fund(PK_A, funded);

        uint256 senderBefore = rail.balanceOf(A);
        uint256 submitterBefore = rail.balanceOf(submitter);

        JuiceRail.TransferTerms memory t = _transfer(id, A, B, amount, fee, R);
        bytes memory sig = _sign(PK_A, rail.digest(rail.transferHash(t)));
        vm.prank(submitter);
        try rail.transfer(t, sig) {
            assert(false); // only the named relayer may carry it
        } catch {}

        assert(rail.balanceOf(A) == senderBefore);
        assert(rail.balanceOf(submitter) == submitterBefore);
        assert(rail.operations(A, id) == bytes32(0));
    }

    /// Several variants of one intent execute at most once: the sender pays one
    /// variant's total or nothing at all.
    function check_variantsExecuteAtMostOnce(bytes32 id, uint96 funded, uint96 amount, uint96 fee1, uint96 fee2)
        public
    {
        vm.assume(id != FUND_ID);
        _fund(PK_A, funded);
        uint256 before = rail.balanceOf(A);

        JuiceRail.TransferTerms memory t1 = _transfer(id, A, B, amount, fee1, R);
        JuiceRail.TransferTerms memory t2 = _transfer(id, A, B, amount, fee2, V);
        bytes memory sig1 = _sign(PK_A, rail.digest(rail.transferHash(t1)));
        bytes memory sig2 = _sign(PK_A, rail.digest(rail.transferHash(t2)));

        vm.prank(R);
        try rail.transfer(t1, sig1) {} catch {}
        vm.prank(V);
        try rail.transfer(t2, sig2) {} catch {}

        uint256 spent = before - rail.balanceOf(A);
        assert(spent == 0 || spent == uint256(amount) + fee1 || spent == uint256(amount) + fee2);
    }

    /// Tokens sent straight to the contract mint nothing: the rail never reads
    /// its standing balance to decide what anyone owns.
    function check_directTransfersMintNothing(uint96 amount) public {
        uint256 sumBefore = _sum();
        vm.prank(A);
        token.transfer(address(rail), amount);
        assert(_sum() == sumBefore);
    }
}
