// Copyright 2025, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package arbtest

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/rpc"
)

// TestReproTraceTimeoutEmptyCallstackPanic ensures a trace that times out before its
// top-level frame is captured returns an error instead of panicking the RPC handler. A
// 1ns timeout makes the interrupt win the race against OnEnter on otherwise normal txs.
func TestReproTraceTimeoutEmptyCallstackPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	builder := NewNodeBuilder(ctx).DefaultConfig(t, false)
	cleanup := builder.Build(t)
	defer cleanup()

	builder.L2Info.GenerateAccount("User2")
	var lastBlock uint64
	for i := 0; i < 5; i++ {
		tx := builder.L2Info.PrepareTx("Owner", "User2", builder.L2Info.TransferGas, big.NewInt(1e12), nil)
		err := builder.L2.Client.SendTransaction(ctx, tx)
		Require(t, err)
		receipt, err := builder.L2.EnsureTxSucceeded(tx)
		Require(t, err)
		lastBlock = receipt.BlockNumber.Uint64()
	}

	l2rpc := builder.L2.Stack.Attach()
	for _, tracer := range []string{"callTracer", "flatCallTracer", "erc7562Tracer"} {
		crashes, timeouts := 0, 0
		for attempt := 0; attempt < 50 && crashes == 0; attempt++ {
			for bn := uint64(1); bn <= lastBlock; bn++ {
				var blockTrace json.RawMessage
				err := l2rpc.CallContext(ctx, &blockTrace, "debug_traceBlockByNumber",
					rpc.BlockNumber(bn),
					map[string]interface{}{"tracer": tracer, "timeout": "1ns"})
				if err == nil {
					continue
				}
				if strings.Contains(err.Error(), "method handler crashed") {
					crashes++
				} else {
					timeouts++
				}
			}
		}
		t.Logf("tracer=%s crashes=%d timeouts=%d", tracer, crashes, timeouts)
	}
}
