// Copyright 2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package arbnode

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"

	"github.com/offchainlabs/nitro/arbos/arbostypes"
	"github.com/offchainlabs/nitro/arbutil"
)

// stubBatchDataProvider returns getErr from every batch lookup. Used to simulate
// the inbox tracker not yet having metadata for a batch referenced by a
// BatchPostingReport.
type stubBatchDataProvider struct {
	getErr error
}

func (p *stubBatchDataProvider) GetBatchCount() (uint64, error) { return 0, nil }
func (p *stubBatchDataProvider) GetBatchMessageCount(seqNum uint64) (arbutil.MessageIndex, error) {
	return 0, nil
}
func (p *stubBatchDataProvider) GetDelayedAcc(seqNum uint64) (common.Hash, error) {
	return common.Hash{}, nil
}
func (p *stubBatchDataProvider) GetSequencerMessageBytes(ctx context.Context, seqNum uint64) ([]byte, common.Hash, error) {
	return nil, common.Hash{}, p.getErr
}
func (p *stubBatchDataProvider) GetSequencerMessageBytesForParentBlock(ctx context.Context, seqNum uint64, parentChainBlock uint64) ([]byte, common.Hash, error) {
	return nil, common.Hash{}, p.getErr
}
func (p *stubBatchDataProvider) FindParentChainBlockContainingDelayed(ctx context.Context, index uint64) (uint64, error) {
	return 0, nil
}

// buildBatchPostingReportL2msg constructs a minimal L2msg payload that
// ParseBatchPostingReportMessageFields can decode. Layout:
//
//	batchTimestamp(32) | batchPosterAddr(20) | dataHash(32) | batchNum(32) | l1BaseFee(32)
func buildBatchPostingReportL2msg(batchNum uint64) []byte {
	out := make([]byte, 32+20+32+32+32)
	// batchNum is read as a 32-byte hash whose low 8 bytes hold the uint64.
	// The 4th 32-byte slot starts at offset 32+20+32 = 84; write big-endian
	// uint64 into bytes [108:116].
	for i := range 8 {
		out[108+i] = byte(batchNum >> (8 * (7 - i)))
	}
	return out
}

// TestAccumulatorNotFoundErrSubstring guards the substring match in
// EphemeralErrorHandler against drift in AccumulatorNotFoundErr's text. The
// handler in TransactionStreamer matches on AccumulatorNotFoundErr.Error() (i.e.
// "accumulator not found"); changing that string without updating call sites
// across the codebase would silently break the throttle.
func TestAccumulatorNotFoundErrSubstring(t *testing.T) {
	const want = "accumulator not found"
	if !strings.Contains(AccumulatorNotFoundErr.Error(), want) {
		t.Fatalf("AccumulatorNotFoundErr.Error() must contain %q for the EphemeralErrorHandler substring match in ExecuteNextMsg to engage; got %q",
			want, AccumulatorNotFoundErr.Error())
	}
}

// TestExecuteNextMsgEphemeralAccumulatorNotFound verifies that when the batch
// data provider returns AccumulatorNotFoundErr while reading a BatchPostingReport
// message, the ephemeral error handler is engaged so the error isn't logged at
// ERROR on every retry.
func TestExecuteNextMsgEphemeralAccumulatorNotFound(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	exec, streamer, _, _ := NewTransactionStreamerForTest(t, ctx, common.Address{})

	// Sanity: the handler is wired up by NewTransactionStreamer.
	if streamer.accNotFoundErrHandler == nil {
		t.Fatal("accNotFoundErrHandler should be initialized by NewTransactionStreamer")
	}
	handler := streamer.accNotFoundErrHandler

	stubProvider := &stubBatchDataProvider{
		getErr: fmt.Errorf("%w: no metadata for batch 1225668", AccumulatorNotFoundErr),
	}
	if err := streamer.SetBatchDataProvider(stubProvider, nil); err != nil {
		Fail(t, err)
	}

	// Set up the streamer's StopWaiter context (needed by GetContextSafe in
	// GetMessage) without launching the executeMessages loop. Driving
	// ExecuteNextMsg synchronously below means there's no concurrent writer to
	// handler.FirstOccurrence — the test goroutine has exclusive access.
	streamer.StopWaiter.Start(ctx, streamer)
	defer streamer.StopAndWait()
	if err := exec.Start(ctx); err != nil {
		Fail(t, err)
	}
	defer exec.StopAndWait()

	// Add a BatchPostingReport at idx 1 (init message is auto-added at idx 0).
	// IsBatchGasFieldsMissing() is true because BatchDataStats and
	// LegacyBatchGasCost are both nil. Because DelayedMessagesRead != 0 and
	// FindParentChainBlockContainingDelayed succeeds, the FillInBatchGasFields
	// callback dispatches to GetSequencerMessageBytesForParentBlock; which the
	// stub fails with AccumulatorNotFoundErr.
	bpr := arbostypes.MessageWithMetadata{
		Message: &arbostypes.L1IncomingMessage{
			Header: &arbostypes.L1IncomingMessageHeader{
				Kind:      arbostypes.L1MessageType_BatchPostingReport,
				RequestId: &common.Hash{},
				L1BaseFee: common.Big0,
			},
			L2msg: buildBatchPostingReportL2msg(1225668),
		},
		DelayedMessagesRead: 1,
	}
	if err := streamer.AddMessages(1, false, []arbostypes.MessageWithMetadata{bpr}, nil); err != nil {
		Fail(t, err)
	}

	// Confirm the error chain still produces AccumulatorNotFoundErr; important
	// because the handler matches on substring "accumulator not found".
	_, err := streamer.GetMessage(1)
	if err == nil {
		t.Fatal("expected GetMessage to fail with AccumulatorNotFoundErr")
	}
	if !errors.Is(err, AccumulatorNotFoundErr) {
		t.Fatalf("expected error chain to contain AccumulatorNotFoundErr, got: %v", err)
	}

	// First call advances exec past the init message at idx 0. Second call hits
	// the BPR at idx 1, fails the read, and engages the handler.
	streamer.ExecuteNextMsg(ctx)
	streamer.ExecuteNextMsg(ctx)
	if handler.FirstOccurrence.Equal(time.Time{}) {
		t.Fatal("expected handler to engage after AccumulatorNotFoundErr from ExecuteNextMsg, but FirstOccurrence is still zero")
	}

	// Reset clears state; mirrors the call on ExecuteNextMsg's success path.
	handler.Reset()
	if !handler.FirstOccurrence.Equal(time.Time{}) {
		t.Fatal("Reset should clear FirstOccurrence")
	}

	// Unrelated errors do not engage the handler.
	unrelated := errors.New("some unrelated db error")
	handler.LogLevel(unrelated, log.Error)
	if !handler.FirstOccurrence.Equal(time.Time{}) {
		t.Fatal("handler should not engage on errors that don't contain 'accumulator not found'")
	}
}
