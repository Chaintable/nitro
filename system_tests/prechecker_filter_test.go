// Copyright 2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package arbtest

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/arbitrum/filter"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"

	"github.com/offchainlabs/nitro/arbnode"
	"github.com/offchainlabs/nitro/arbos"
	"github.com/offchainlabs/nitro/arbos/arbostypes"
	"github.com/offchainlabs/nitro/arbos/l2pricing"
	"github.com/offchainlabs/nitro/cmd/chaininfo"
	filteringreportapi "github.com/offchainlabs/nitro/cmd/filtering-report/api"
	"github.com/offchainlabs/nitro/cmd/filtering-report/forwarder"
	"github.com/offchainlabs/nitro/execution/gethexec"
	"github.com/offchainlabs/nitro/execution/gethexec/addressfilter"
	"github.com/offchainlabs/nitro/execution/gethexec/eventfilter"
	"github.com/offchainlabs/nitro/solgen/go/bridgegen"
	"github.com/offchainlabs/nitro/solgen/go/localgen"
	"github.com/offchainlabs/nitro/solgen/go/precompilesgen"
	"github.com/offchainlabs/nitro/util/arbmath"
	"github.com/offchainlabs/nitro/util/sqsclient"
	"github.com/offchainlabs/nitro/util/testhelpers"
	"github.com/offchainlabs/nitro/util/testhelpers/env"
)

// These tests use a two-node setup: a sequencer (node A) and a forwarder
// (node B). The forwarder's TxPreChecker has address filtering enabled, but
// the sequencer has NO filtering configured. This proves filtering structurally:
// rejections can only come from the forwarder's prechecker dry-run. Clean txs
// forwarded through B reach A and are sequenced normally.

// waitForForwarderSync polls the forwarder until its latest block number
// reaches targetBlock. Unlike WaitForTx, this doesn't depend on the tx
// indexer, which can be slow on freshly-synced nodes.
func waitForForwarderSync(t *testing.T, ctx context.Context, forwarder *TestClient, targetBlock uint64) {
	t.Helper()
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		header, err := forwarder.Client.HeaderByNumber(timeoutCtx, nil)
		if err == nil && header.Number.Uint64() >= targetBlock {
			return
		}
		select {
		case <-timeoutCtx.Done():
			require.NoError(t, timeoutCtx.Err(), "forwarder did not reach block %d within timeout", targetBlock)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// buildPrecheckerFilterNodes creates a sequencer node A and a forwarder node B
// for prechecker filter testing. Node B forwards to A via IPC. If reportURL is
// non-empty, the forwarder's TxPreChecker is wired to send filtered tx reports
// to that URL.
func buildPrecheckerFilterNodes(t *testing.T, ctx context.Context, withDelayedSeq bool, reportURL string, eventRules ...eventfilter.EventRule) (builder *NodeBuilder, forwarder *TestClient, cleanup func()) {
	t.Helper()
	ipcPath := tmpPath(t, "test.ipc")

	builder = NewNodeBuilder(ctx).DefaultConfig(t, true)
	builder.execConfig.TransactionFiltering.EnableETHCallFilter = false
	builder.nodeConfig.Feed.Output = *newBroadcasterConfigTest()
	builder.l2StackConfig.IPCPath = ipcPath
	if withDelayedSeq {
		builder.nodeConfig.DelayedSequencer.Enable = true
		builder.nodeConfig.DelayedSequencer.FinalizeDistance = 1
	} else {
		builder.nodeConfig.BatchPoster.Enable = false
	}
	cleanupA := builder.Build(t)

	port := testhelpers.AddrTCPPort(builder.L2.ConsensusNode.BroadcastServer.ListenerAddr(), t)

	nodeConfigB := arbnode.ConfigDefaultL1Test()
	execConfigB := ExecConfigDefaultTest(t, env.GetTestStateScheme())
	execConfigB.TxPreChecker.Strictness = gethexec.TxPreCheckerStrictnessAlwaysCompatible
	execConfigB.TransactionFiltering.EnableETHCallFilter = true
	execConfigB.Sequencer.Enable = false
	nodeConfigB.Sequencer = false
	nodeConfigB.DelayedSequencer.Enable = false
	execConfigB.Forwarder.RedisUrl = ""
	execConfigB.ForwardingTarget = ipcPath
	nodeConfigB.BatchPoster.Enable = false
	nodeConfigB.Feed.Input = *newBroadcastClientConfigTest(port)
	if len(eventRules) > 0 {
		execConfigB.TransactionFiltering.EventFilter.Rules = eventRules
	}
	if reportURL != "" {
		execConfigB.TransactionFiltering.FilteringReportRPCClient.URL = reportURL
	}

	forwarder, cleanupB := builder.Build2ndNode(t, &SecondNodeParams{
		nodeConfig: nodeConfigB,
		execConfig: execConfigB,
	})

	cleanup = func() {
		cleanupB()
		cleanupA()
	}
	return builder, forwarder, cleanup
}

// syncForwarderToHead waits until the forwarder catches up to the sequencer's
// current head, so prechecker dry-runs see the latest filter / contract state.
func syncForwarderToHead(t *testing.T, ctx context.Context, builder *NodeBuilder, forwarder *TestClient) {
	t.Helper()
	seqLatest, err := builder.L2.Client.BlockNumber(ctx)
	Require(t, err)
	waitForForwarderSync(t, ctx, forwarder, seqLatest)
}

// buildForwarderRedeemTx builds a signed (un-sent) Redeem tx targeting the
// forwarder's RPC, so the caller can both submit it and reference tx.Hash().
func buildForwarderRedeemTx(
	t *testing.T, ctx context.Context, builder *NodeBuilder, forwarder *TestClient,
	account string, ticketId common.Hash, gasLimit uint64,
) *types.Transaction {
	t.Helper()
	arbRetryable, err := precompilesgen.NewArbRetryableTx(types.ArbRetryableTxAddress, forwarder.Client)
	Require(t, err)
	auth := builder.L2Info.GetDefaultTransactOpts(account, ctx)
	auth.GasLimit = gasLimit
	auth.NoSend = true
	tx, err := arbRetryable.Redeem(&auth, ticketId)
	Require(t, err, "building redeem tx")
	return tx
}

// precheckerSubmitRetryable creates a retryable ticket via the L1 delayed inbox,
// advances L1, waits for the sequencer to process it, and verifies the ticket
// exists. Returns the ticket ID (L2 submission tx hash).
func precheckerSubmitRetryable(
	t *testing.T, ctx context.Context, builder *NodeBuilder,
	destAddr common.Address, calldata []byte, gasLimit *big.Int,
) common.Hash {
	t.Helper()
	delayedInbox, err := bridgegen.NewInbox(builder.L1Info.GetAddress("Inbox"), builder.L1.Client)
	require.NoError(t, err)

	deposit := arbmath.BigMul(big.NewInt(1e12), big.NewInt(1e12))
	maxSubmissionCost := big.NewInt(1e16)
	maxFeePerGas := big.NewInt(l2pricing.InitialBaseFeeWei * 2)

	l1opts := builder.L1Info.GetDefaultTransactOpts("Faucet", ctx)
	l1opts.Value = deposit
	l1tx, err := delayedInbox.CreateRetryableTicket(
		&l1opts,
		destAddr,
		common.Big0,
		maxSubmissionCost,
		common.Address{},
		common.Address{},
		gasLimit,
		maxFeePerGas,
		calldata,
	)
	require.NoError(t, err)
	l1Receipt, err := builder.L1.EnsureTxSucceeded(l1tx)
	require.NoError(t, err)

	ticketId := lookupSubmissionTxHash(t, ctx, builder, l1Receipt)

	AdvanceL1(t, ctx, builder.L1.Client, builder.L1Info, 30)

	_, err = WaitForTx(ctx, builder.L2.Client, ticketId, 30*time.Second)
	require.NoError(t, err)

	arbRetryable, err := precompilesgen.NewArbRetryableTx(types.ArbRetryableTxAddress, builder.L2.Client)
	require.NoError(t, err)
	_, err = arbRetryable.GetTimeout(&bind.CallOpts{}, ticketId)
	require.NoError(t, err, "retryable ticket %s should exist", ticketId.Hex())

	return ticketId
}

// lookupSubmissionTxHash finds the ArbitrumSubmitRetryableTx hash from an L1 receipt
// by parsing the delayed message.
func lookupSubmissionTxHash(t *testing.T, ctx context.Context, builder *NodeBuilder, l1Receipt *types.Receipt) common.Hash {
	t.Helper()

	delayedBridge, err := arbnode.NewDelayedBridge(builder.L1.Client, builder.L1Info.GetAddress("Bridge"), 0)
	require.NoError(t, err)

	messages, err := delayedBridge.LookupMessagesInRange(ctx, l1Receipt.BlockNumber, l1Receipt.BlockNumber, nil)
	require.NoError(t, err)
	require.NotEmpty(t, messages, "no delayed messages found")

	for _, message := range messages {
		if message.Message.Header.Kind != arbostypes.L1MessageType_SubmitRetryable {
			continue
		}
		txs, err := arbos.ParseL2Transactions(message.Message, chaininfo.ArbitrumDevTestChainConfig().ChainID, params.MaxDebugArbosVersionSupported)
		require.NoError(t, err)
		for _, tx := range txs {
			if tx.Type() == types.ArbitrumSubmitRetryableTxType {
				return tx.Hash()
			}
		}
	}
	t.Fatal("no retryable submission tx found in delayed messages")
	return common.Hash{}
}

// setupPrecheckerFilteringReport wires report stack + mock SQS forwarder + external endpoint; returns the URL prechecker reports to and the endpoint that receives the result.
func setupPrecheckerFilteringReport(t *testing.T) (string, *forwarder.MockExternalEndpoint) {
	t.Helper()
	queueClient := &sqsclient.MockQueueClient{}
	externalEndpoint := forwarder.NewMockExternalEndpoint(t)
	stack := filteringreportapi.NewTestStack(t, queueClient)
	fwd := forwarder.NewTestForwarder(t, queueClient, externalEndpoint.URL())
	fwd.Start(t.Context())
	t.Cleanup(func() { fwd.StopAndWait() })
	return stack.HTTPEndpoint(), externalEndpoint
}

// requireFilteredAddress finds a record for addr and returns it.
func requireFilteredAddress(t *testing.T, report *addressfilter.FilteredTxReport, addr common.Address) filter.FilteredAddressRecord {
	t.Helper()
	for _, rec := range report.FilteredAddresses {
		if rec.Address == addr {
			return rec
		}
	}
	t.Fatalf("report should contain filtered address %s, got %+v", addr.Hex(), report.FilteredAddresses)
	return filter.FilteredAddressRecord{}
}

// requireBaseReportFields asserts invariants common to every prechecker report.
func requireBaseReportFields(t *testing.T, ctx context.Context, builder *NodeBuilder, report *addressfilter.FilteredTxReport, tx *types.Transaction) {
	t.Helper()
	require.Equal(t, tx.Hash(), report.TxHash, "txHash")
	require.NotEmpty(t, report.ID, "report ID must be set")
	require.NotEmpty(t, report.TxRLP, "txRLP must be set")
	require.Equal(t, builder.chainConfig.ChainID.Uint64(), report.ChainID, "chainID")
	require.False(t, report.IsDelayed, "prechecker must not flag tx as delayed")
	require.Nil(t, report.DelayedReportData, "prechecker must not populate delayed payload")
	require.Equal(t, uint64(0), report.PositionInBlock, "prechecker has no in-block position")
	require.False(t, report.FilteredAt.IsZero(), "filteredAt must be populated")
	require.WithinDuration(t, time.Now().UTC(), report.FilteredAt, 5*time.Minute, "filteredAt must be recent")

	var decoded types.Transaction
	Require(t, decoded.UnmarshalBinary(report.TxRLP), "txRLP should decode")
	require.Equal(t, tx.Hash(), decoded.Hash(), "decoded txRLP hash should match")

	CheckReportBlockNumberAndParentBlockHash(t, ctx, builder, report)
}

// TestPrecheckerFilterDirectAddress verifies that the forwarder's prechecker
// dry-run filtering catches transactions sent to/from a filtered address.
func TestPrecheckerFilterDirectAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder, forwarder, cleanup := buildPrecheckerFilterNodes(t, ctx, false, "")
	defer cleanup()

	builder.L2Info.GenerateAccount("FilteredUser")
	builder.L2Info.GenerateAccount("NormalUser")
	builder.L2.TransferBalance(t, "Owner", "NormalUser", big.NewInt(1e18), builder.L2Info)
	_, fundReceipt := builder.L2.TransferBalance(t, "Owner", "FilteredUser", big.NewInt(1e18), builder.L2Info)
	waitForForwarderSync(t, ctx, forwarder, fundReceipt.BlockNumber.Uint64())

	filteredAddr := builder.L2Info.GetAddress("FilteredUser")
	filter := newHashedChecker([]common.Address{filteredAddr})
	forwarder.ExecNode.ExecEngine.SetAddressChecker(t, filter)

	// tx TO filtered address via forwarder should be rejected
	tx := builder.L2Info.PrepareTx("NormalUser", "FilteredUser", builder.L2Info.TransferGas, big.NewInt(1e12), nil)
	err := forwarder.Client.SendTransaction(ctx, tx)
	if !isFilteredError(err) {
		t.Fatalf("expected filtered error for tx TO filtered address, got: %v", err)
	}
	builder.L2Info.GetInfoWithPrivKey("NormalUser").Nonce.Store(0)

	// tx FROM filtered address via forwarder should be rejected
	tx = builder.L2Info.PrepareTx("FilteredUser", "NormalUser", builder.L2Info.TransferGas, big.NewInt(1e12), nil)
	err = forwarder.Client.SendTransaction(ctx, tx)
	if !isFilteredError(err) {
		t.Fatalf("expected filtered error for tx FROM filtered address, got: %v", err)
	}
	builder.L2Info.GetInfoWithPrivKey("FilteredUser").Nonce.Store(0)

	// tx between non-filtered addresses via forwarder should forward and succeed
	builder.L2Info.GenerateAccount("AnotherUser")
	_, fundReceipt = builder.L2.TransferBalance(t, "Owner", "AnotherUser", big.NewInt(1e18), builder.L2Info)
	waitForForwarderSync(t, ctx, forwarder, fundReceipt.BlockNumber.Uint64())
	tx = builder.L2Info.PrepareTx("NormalUser", "AnotherUser", builder.L2Info.TransferGas, big.NewInt(1e12), nil)
	err = forwarder.Client.SendTransaction(ctx, tx)
	Require(t, err)
	_, err = builder.L2.EnsureTxSucceeded(tx)
	Require(t, err)
}

// TestPrecheckerFilterCleanTxPasses verifies that non-filtered transactions
// pass through the forwarder's prechecker and are forwarded to the sequencer.
func TestPrecheckerFilterCleanTxPasses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder, forwarder, cleanup := buildPrecheckerFilterNodes(t, ctx, false, "")
	defer cleanup()

	builder.L2Info.GenerateAccount("User1")
	builder.L2Info.GenerateAccount("User2")
	builder.L2Info.GenerateAccount("FilteredUser")
	_, fundReceipt := builder.L2.TransferBalance(t, "Owner", "User1", big.NewInt(1e18), builder.L2Info)
	waitForForwarderSync(t, ctx, forwarder, fundReceipt.BlockNumber.Uint64())

	filteredAddr := builder.L2Info.GetAddress("FilteredUser")
	filter := newHashedChecker([]common.Address{filteredAddr})
	forwarder.ExecNode.ExecEngine.SetAddressChecker(t, filter)

	tx := builder.L2Info.PrepareTx("User1", "User2", builder.L2Info.TransferGas, big.NewInt(1e12), nil)
	err := forwarder.Client.SendTransaction(ctx, tx)
	Require(t, err)
	_, err = builder.L2.EnsureTxSucceeded(tx)
	Require(t, err)
}

// TestPrecheckerFilterDisabled verifies that all transactions pass when no
// address checker is set on the forwarder's prechecker.
func TestPrecheckerFilterDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder, forwarder, cleanup := buildPrecheckerFilterNodes(t, ctx, false, "")
	defer cleanup()

	builder.L2Info.GenerateAccount("User1")
	builder.L2Info.GenerateAccount("User2")
	_, fundReceipt := builder.L2.TransferBalance(t, "Owner", "User1", big.NewInt(1e18), builder.L2Info)
	waitForForwarderSync(t, ctx, forwarder, fundReceipt.BlockNumber.Uint64())

	// No address checker set on forwarder -- all txs should pass
	tx := builder.L2Info.PrepareTx("User1", "User2", builder.L2Info.TransferGas, big.NewInt(1e12), nil)
	err := forwarder.Client.SendTransaction(ctx, tx)
	Require(t, err)
	_, err = builder.L2.EnsureTxSucceeded(tx)
	Require(t, err)
}

// TestPrecheckerFilterEvents verifies that the forwarder's prechecker catches
// transactions whose execution emits events referencing filtered addresses.
func TestPrecheckerFilterEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	selector, _, err := eventfilter.CanonicalSelectorFromEvent("Transfer(address,address,uint256)")
	Require(t, err)

	rules := []eventfilter.EventRule{
		{
			Event:          "Transfer(address,address,uint256)",
			Selector:       selector,
			TopicAddresses: []int{1, 2},
		},
	}

	builder, forwarder, cleanup := buildPrecheckerFilterNodes(t, ctx, false, "", rules...)
	defer cleanup()

	// Deploy contract through sequencer and wait for forwarder to sync
	auth := builder.L2Info.GetDefaultTransactOpts("Owner", ctx)
	contractAddr, deployTx, _, err := localgen.DeployAddressFilterTest(&auth, builder.L2.Client)
	Require(t, err)
	deployReceipt, err := builder.L2.EnsureTxSucceeded(deployTx)
	Require(t, err)
	waitForForwarderSync(t, ctx, forwarder, deployReceipt.BlockNumber.Uint64())

	// Bind contract to forwarder client
	contractOnForwarder, err := localgen.NewAddressFilterTest(contractAddr, forwarder.Client)
	Require(t, err)

	builder.L2Info.GenerateAccount("FilteredAddr")
	builder.L2Info.GenerateAccount("CleanAddr")
	filteredAddr := builder.L2Info.GetAddress("FilteredAddr")
	cleanAddr := builder.L2Info.GetAddress("CleanAddr")

	filter := newHashedChecker([]common.Address{filteredAddr})
	forwarder.ExecNode.ExecEngine.SetAddressChecker(t, filter)

	// Transfer to filtered address via forwarder should be rejected
	auth = builder.L2Info.GetDefaultTransactOpts("Owner", ctx)
	_, err = contractOnForwarder.EmitTransfer(&auth, auth.From, filteredAddr)
	if !isFilteredError(err) {
		t.Fatalf("expected filtered error for Transfer to filtered address, got: %v", err)
	}

	// Transfer between clean addresses via forwarder should succeed
	auth = builder.L2Info.GetDefaultTransactOpts("Owner", ctx)
	tx, err := contractOnForwarder.EmitTransfer(&auth, auth.From, cleanAddr)
	Require(t, err)
	_, err = builder.L2.EnsureTxSucceeded(tx)
	Require(t, err)
}

// TestPrecheckerFilterManualRedeem verifies that the forwarder's prechecker
// catches a manual redeem of a retryable whose inner call touches a filtered
// address. The retryable is created via L1 and processed by the sequencer.
// The forwarder syncs the state, then the redeem is sent through the forwarder
// where the prechecker dry-run detects the filtered address.
func TestPrecheckerFilterManualRedeem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder, forwarder, cleanup := buildPrecheckerFilterNodes(t, ctx, true, "")
	defer cleanup()

	// Deploy contract through sequencer as retryable destination
	contractAddr, _ := deployAddressFilterTestContract(t, ctx, builder)

	builder.L2Info.GenerateAccount("Redeemer")
	builder.L2.TransferBalance(t, "Owner", "Redeemer", big.NewInt(1e18), builder.L2Info)

	// Submit retryable with invalid calldata so auto-redeem fails
	invalidCalldata := []byte{0xde, 0xad, 0xbe, 0xef}
	ticketId := precheckerSubmitRetryable(t, ctx, builder, contractAddr, invalidCalldata, big.NewInt(100000))

	// Wait for forwarder to sync to the sequencer's latest block
	syncForwarderToHead(t, ctx, builder, forwarder)

	// Set filter on forwarder's prechecker targeting the contract
	filter := newHashedChecker([]common.Address{contractAddr})
	forwarder.ExecNode.ExecEngine.SetAddressChecker(t, filter)

	// Build redeem tx and send through forwarder -- prechecker should reject
	redeemTx := buildForwarderRedeemTx(t, ctx, builder, forwarder, "Redeemer", ticketId, 1_000_000)

	err := forwarder.Client.SendTransaction(ctx, redeemTx)
	if !isFilteredError(err) {
		t.Fatalf("expected prechecker to reject manual redeem touching filtered address, got: %v", err)
	}
}

// TestPrecheckerFilterContractTriggeredRedeem verifies that the forwarder's
// prechecker catches a redeem triggered by an intermediary contract. The user's
// outer tx targets a wrapper contract (not filtered), which internally calls
// ArbRetryableTx.redeem(). The redeem's inner execution touches the filtered
// destination contract.
func TestPrecheckerFilterContractTriggeredRedeem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder, forwarder, cleanup := buildPrecheckerFilterNodes(t, ctx, true, "")
	defer cleanup()

	// Contract A: the retryable destination (will be filtered)
	destAddr, _ := deployAddressFilterTestContract(t, ctx, builder)

	// Contract B: the wrapper that will call redeemTicket()
	wrapperAddr, _ := deployAddressFilterTestContract(t, ctx, builder)

	builder.L2Info.GenerateAccount("Caller")
	builder.L2.TransferBalance(t, "Owner", "Caller", big.NewInt(1e18), builder.L2Info)

	// Submit retryable with invalid calldata so auto-redeem fails
	invalidCalldata := []byte{0xde, 0xad, 0xbe, 0xef}
	ticketId := precheckerSubmitRetryable(t, ctx, builder, destAddr, invalidCalldata, big.NewInt(100000))

	// Wait for forwarder to sync to the sequencer's latest block
	syncForwarderToHead(t, ctx, builder, forwarder)

	// Set filter on forwarder's prechecker targeting contract A
	filter := newHashedChecker([]common.Address{destAddr})
	forwarder.ExecNode.ExecEngine.SetAddressChecker(t, filter)

	// Bind wrapper contract to forwarder client and send through forwarder
	wrapperOnForwarder, err := localgen.NewAddressFilterTest(wrapperAddr, forwarder.Client)
	require.NoError(t, err)
	auth := builder.L2Info.GetDefaultTransactOpts("Caller", ctx)
	auth.GasLimit = 1_000_000
	_, err = wrapperOnForwarder.RedeemTicket(&auth, ticketId)
	if !isFilteredError(err) {
		t.Fatalf("expected prechecker to reject contract-triggered redeem touching filtered address, got: %v", err)
	}
}

// testPrecheckerFilterCascadingRedeem tests that the prechecker's FIFO redeem
// loop catches filtered addresses at arbitrary cascade depth. The deepest ticket
// targets a neutral wrapper contract that internally CALLs filteredTarget, so the
// filtered address is only discovered during actual execution (not via the To field).
func testPrecheckerFilterCascadingRedeem(t *testing.T, depth int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder, forwarder, cleanup := buildPrecheckerFilterNodes(t, ctx, true, "")
	defer cleanup()

	// Deploy wrapper (neutral) and filteredTarget contracts
	wrapperAddr, _ := deployAddressFilterTestContract(t, ctx, builder)
	filteredTarget, _ := deployAddressFilterTestContract(t, ctx, builder)

	// Fund redeemer early so balance syncs when we sync the forwarder later
	builder.L2Info.GenerateAccount("Redeemer")
	builder.L2.TransferBalance(t, "Owner", "Redeemer", big.NewInt(1e18), builder.L2Info)

	wrapperABI, err := localgen.AddressFilterTestMetaData.GetAbi()
	require.NoError(t, err)
	arbRetryableABI, err := precompilesgen.ArbRetryableTxMetaData.GetAbi()
	require.NoError(t, err)

	// Build retryable chain bottom-up.
	// ticket[0] is the deepest: targets wrapper with callTarget(filteredTarget).
	// ticket[i>0]: dest=ArbRetryableTx, data=redeem(ticket[i-1]).
	ticketIds := make([]common.Hash, depth)

	// Deepest ticket targets wrapper.callTarget(filteredTarget) — the filtered
	// address is only touched during execution, not in the To field.
	callTargetData, err := wrapperABI.Pack("callTarget", filteredTarget)
	require.NoError(t, err)
	ticketIds[0] = precheckerSubmitRetryable(
		t, ctx, builder, wrapperAddr, callTargetData, common.Big0,
	)

	// Each subsequent ticket redeems the previous one.
	for i := 1; i < depth; i++ {
		redeemData, err := arbRetryableABI.Pack("redeem", ticketIds[i-1])
		require.NoError(t, err)
		ticketIds[i] = precheckerSubmitRetryable(
			t, ctx, builder, types.ArbRetryableTxAddress, redeemData, common.Big0,
		)
	}

	topTicketId := ticketIds[depth-1]

	// Sync forwarder to sequencer's latest block
	syncForwarderToHead(t, ctx, builder, forwarder)

	// Set filter on forwarder's prechecker targeting filteredTarget
	filter := newHashedChecker([]common.Address{filteredTarget})
	forwarder.ExecNode.ExecEngine.SetAddressChecker(t, filter)

	// Manual redeem of the top ticket through forwarder — prechecker must
	// execute the full cascade including the deepest redeem to discover the
	// filtered address touched via wrapper.callTarget().
	redeemTx := buildForwarderRedeemTx(t, ctx, builder, forwarder, "Redeemer", topTicketId, 2_000_000)

	err = forwarder.Client.SendTransaction(ctx, redeemTx)
	if !isFilteredError(err) {
		t.Fatalf("expected prechecker to reject cascading redeem at depth %d, got: %v", depth, err)
	}
}

// TestPrecheckerFilterCascadingRedeemDepth2 tests A -> B -> wrapper.callTarget(filtered).
func TestPrecheckerFilterCascadingRedeemDepth2(t *testing.T) {
	testPrecheckerFilterCascadingRedeem(t, 2)
}

// TestPrecheckerFilterCascadingRedeemDepth3 tests A -> B -> C -> wrapper.callTarget(filtered).
func TestPrecheckerFilterCascadingRedeemDepth3(t *testing.T) {
	testPrecheckerFilterCascadingRedeem(t, 3)
}

// TestPrecheckerFilterCascadingRedeemDepth4 tests A -> B -> C -> D -> wrapper.callTarget(filtered).
func TestPrecheckerFilterCascadingRedeemDepth4(t *testing.T) {
	testPrecheckerFilterCascadingRedeem(t, 4)
}

// TestPrecheckerFilterReport (Scenario 1: preTxFilter from/to) verifies that
// the prechecker sends a FilteredTxReport when a tx is filtered by its To
// address before any execution.
func TestPrecheckerFilterReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reportURL, externalEndpoint := setupPrecheckerFilteringReport(t)

	builder, forwarder, cleanup := buildPrecheckerFilterNodes(t, ctx, false, reportURL)
	defer cleanup()

	builder.L2Info.GenerateAccount("FilteredUser")
	builder.L2Info.GenerateAccount("NormalUser")
	builder.L2.TransferBalance(t, "Owner", "NormalUser", big.NewInt(1e18), builder.L2Info)
	_, fundReceipt := builder.L2.TransferBalance(t, "Owner", "FilteredUser", big.NewInt(1e18), builder.L2Info)
	waitForForwarderSync(t, ctx, forwarder, fundReceipt.BlockNumber.Uint64())

	filteredAddr := builder.L2Info.GetAddress("FilteredUser")
	forwarder.ExecNode.ExecEngine.SetAddressChecker(t, newHashedChecker([]common.Address{filteredAddr}))

	tx := builder.L2Info.PrepareTx("NormalUser", "FilteredUser", builder.L2Info.TransferGas, big.NewInt(1e12), nil)
	err := forwarder.Client.SendTransaction(ctx, tx)
	if !isFilteredError(err) {
		t.Fatalf("expected filtered error, got: %v", err)
	}

	report := externalEndpoint.NextReport(t)
	requireBaseReportFields(t, ctx, builder, report, tx)
	rec := requireFilteredAddress(t, report, filteredAddr)
	require.Equal(t, filter.ReasonTo, rec.Reason)
	require.Nil(t, rec.EventRuleMatch, "preTxFilter must not carry event-rule payload")
}

// TestPrecheckerFilterReportEventTriggered: postTxFilter via EventFilter rule on emitted log topic.
func TestPrecheckerFilterReportEventTriggered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	selector, _, err := eventfilter.CanonicalSelectorFromEvent("Transfer(address,address,uint256)")
	Require(t, err)
	rules := []eventfilter.EventRule{{
		Event:          "Transfer(address,address,uint256)",
		Selector:       selector,
		TopicAddresses: []int{1, 2},
	}}

	reportURL, externalEndpoint := setupPrecheckerFilteringReport(t)
	builder, forwarder, cleanup := buildPrecheckerFilterNodes(t, ctx, false, reportURL, rules...)
	defer cleanup()

	auth := builder.L2Info.GetDefaultTransactOpts("Owner", ctx)
	contractAddr, deployTx, _, err := localgen.DeployAddressFilterTest(&auth, builder.L2.Client)
	Require(t, err)
	deployReceipt, err := builder.L2.EnsureTxSucceeded(deployTx)
	Require(t, err)
	waitForForwarderSync(t, ctx, forwarder, deployReceipt.BlockNumber.Uint64())

	contractOnForwarder, err := localgen.NewAddressFilterTest(contractAddr, forwarder.Client)
	Require(t, err)

	builder.L2Info.GenerateAccount("FilteredAddr")
	filteredAddr := builder.L2Info.GetAddress("FilteredAddr")
	forwarder.ExecNode.ExecEngine.SetAddressChecker(t, newHashedChecker([]common.Address{filteredAddr}))

	auth = builder.L2Info.GetDefaultTransactOpts("Owner", ctx)
	auth.GasLimit = 500_000 // skip EstimateGas, which would surface the filter via the forwarder's prechecker before we get a tx to assert against
	auth.NoSend = true
	tx, err := contractOnForwarder.EmitTransfer(&auth, auth.From, filteredAddr)
	Require(t, err, "building EmitTransfer tx")
	err = forwarder.Client.SendTransaction(ctx, tx)
	if !isFilteredError(err) {
		t.Fatalf("expected event-rule filtered error, got: %v", err)
	}

	report := externalEndpoint.NextReport(t)
	requireBaseReportFields(t, ctx, builder, report, tx)
	rec := requireFilteredAddress(t, report, filteredAddr)
	require.Equal(t, filter.ReasonEventRule, rec.Reason)
	require.NotNil(t, rec.EventRuleMatch, "event-rule reason must carry EventRuleMatch")
	require.Equal(t, "Transfer(address,address,uint256)", rec.EventRuleMatch.MatchedEvent)
	require.Equal(t, 2, rec.EventRuleMatch.MatchedTopicIndex, "filteredAddr is the `to` arg, indexed at topic[2]")
	require.NotNil(t, rec.EventRuleMatch.RawLog, "event-rule reason must carry raw log")
	require.Equal(t, contractAddr, rec.EventRuleMatch.RawLog.Address, "event must originate from emitter contract")
	require.NotEmpty(t, rec.EventRuleMatch.RawLog.Topics, "raw log topics must be set")
	require.Equal(t, selector[:], rec.EventRuleMatch.RawLog.Topics[0].Bytes()[:len(selector)], "topic[0] must be the event selector")
}

// TestPrecheckerFilterReportContractCall: postTxFilter via PushContract on inner CALL, no EventFilter.
func TestPrecheckerFilterReportContractCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reportURL, externalEndpoint := setupPrecheckerFilteringReport(t)
	builder, forwarder, cleanup := buildPrecheckerFilterNodes(t, ctx, false, reportURL)
	defer cleanup()

	wrapperAddr, _ := deployAddressFilterTestContract(t, ctx, builder)
	filteredTargetAddr, _ := deployAddressFilterTestContract(t, ctx, builder)

	syncForwarderToHead(t, ctx, builder, forwarder)

	forwarder.ExecNode.ExecEngine.SetAddressChecker(t, newHashedChecker([]common.Address{filteredTargetAddr}))

	wrapperOnForwarder, err := localgen.NewAddressFilterTest(wrapperAddr, forwarder.Client)
	Require(t, err)
	auth := builder.L2Info.GetDefaultTransactOpts("Owner", ctx)
	auth.GasLimit = 500_000 // skip EstimateGas, which would surface the filter via the forwarder's prechecker before we get a tx to assert against
	auth.NoSend = true
	tx, err := wrapperOnForwarder.CallTarget(&auth, filteredTargetAddr)
	Require(t, err, "building CallTarget tx")
	err = forwarder.Client.SendTransaction(ctx, tx)
	if !isFilteredError(err) {
		t.Fatalf("expected post-execution filtered error, got: %v", err)
	}

	report := externalEndpoint.NextReport(t)
	requireBaseReportFields(t, ctx, builder, report, tx)
	rec := requireFilteredAddress(t, report, filteredTargetAddr)
	require.Equal(t, filter.ReasonContractAddress, rec.Reason, "filtered contract should be flagged via PushContract bookkeeping")
	require.Nil(t, rec.EventRuleMatch, "contract-address reason must not carry EventRuleMatch")
}

// TestPrecheckerFilterReportRedeem: scheduled retryable redeem; filtered contract surfaces via RunScheduledTxes.
func TestPrecheckerFilterReportRedeem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reportURL, externalEndpoint := setupPrecheckerFilteringReport(t)
	builder, forwarder, cleanup := buildPrecheckerFilterNodes(t, ctx, true, reportURL)
	defer cleanup()

	contractAddr, _ := deployAddressFilterTestContract(t, ctx, builder)

	builder.L2Info.GenerateAccount("Redeemer")
	builder.L2.TransferBalance(t, "Owner", "Redeemer", big.NewInt(1e18), builder.L2Info)

	invalidCalldata := []byte{0xde, 0xad, 0xbe, 0xef}
	ticketId := precheckerSubmitRetryable(t, ctx, builder, contractAddr, invalidCalldata, big.NewInt(100000))

	syncForwarderToHead(t, ctx, builder, forwarder)

	forwarder.ExecNode.ExecEngine.SetAddressChecker(t, newHashedChecker([]common.Address{contractAddr}))

	redeemTx := buildForwarderRedeemTx(t, ctx, builder, forwarder, "Redeemer", ticketId, 1_000_000)

	err := forwarder.Client.SendTransaction(ctx, redeemTx)
	if !isFilteredError(err) {
		t.Fatalf("expected redeem prechecker rejection, got: %v", err)
	}

	report := externalEndpoint.NextReport(t)
	requireBaseReportFields(t, ctx, builder, report, redeemTx)
	rec := requireFilteredAddress(t, report, contractAddr)
	// Outer tx targets ArbRetryableTx, not contractAddr — contractAddr can only surface via the
	// scheduled inner retry, where it appears as either the inner tx's To or as a pushed contract
	// frame. Async workers determine which lands in the report first.
	require.Contains(t,
		[]filter.FilterReasonType{filter.ReasonTo, filter.ReasonContractAddress, filter.ReasonRetryableTo},
		rec.Reason,
		"scheduled redeem must surface filtered contract via the cascade path")
	require.Nil(t, rec.EventRuleMatch)
}
