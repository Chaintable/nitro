// Copyright 2025-2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package arbtest

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"

	"github.com/offchainlabs/nitro/solgen/go/precompilesgen"
)

// TestMultiGasRefundWithoutConstraints reproduces a bug where EndTxHook issues
// a multi-dim gas refund even when no multi-gas constraints are configured.
// EndTxHook's totalCost uses header.BaseFee (pre-UpdatePricingModel), while
// MultiDimensionalPriceForRefund reads the post-update BaseFeeWei(). When base
// fee falls between blocks, the difference is wrongly credited to the sender.
// The test pumps the backlog, raises the speed limit to force a drain, then
// asserts the sender's balance delta equals Σ(EffectiveGasPrice * GasUsed).
func TestMultiGasRefundWithoutConstraints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder := NewNodeBuilder(ctx).
		DefaultConfig(t, false).
		WithArbOSVersion(params.ArbosVersion_61)
	cleanup := builder.Build(t)
	defer cleanup()

	// Raise the fee cap so the pump burst isn't rejected when base fee spikes.
	builder.L2Info.GasPrice = big.NewInt(100 * params.GWei)

	// Set speed limit.
	arbOwner, err := precompilesgen.NewArbOwner(types.ArbOwnerAddress, builder.L2.Client)
	Require(t, err)
	ownerAuth := builder.L2Info.GetDefaultTransactOpts("Owner", ctx)
	tx, err := arbOwner.SetSpeedLimit(&ownerAuth, 5_000)
	Require(t, err)
	_, err = builder.L2.EnsureTxSucceeded(tx)
	Require(t, err)

	// Fund user account.
	builder.L2Info.GenerateAccount("User")
	userAddr := builder.L2Info.GetAddress("User")
	hundredEther := new(big.Int).Mul(big.NewInt(params.Ether), big.NewInt(100))
	TransferBalance(t, "Owner", "User", hundredEther, builder.L2Info, builder.L2.Client, ctx)

	startBalance, err := builder.L2.Client.BalanceAt(ctx, userAddr, nil)
	Require(t, err)

	sendUserTx := func(gasTipCap *big.Int) *types.Receipt {
		base := builder.L2Info.PrepareTxTo("User", &userAddr, builder.L2Info.TransferGas, common.Big0, nil)
		signed := builder.L2Info.SignTxAs("User", &types.DynamicFeeTx{
			To:        base.To(),
			Gas:       base.Gas(),
			GasTipCap: gasTipCap,
			GasFeeCap: base.GasFeeCap(),
			Value:     base.Value(),
			Nonce:     base.Nonce(),
		})
		Require(t, builder.L2.Client.SendTransaction(ctx, signed))
		receipt, err := builder.L2.EnsureTxSucceeded(signed)
		Require(t, err)
		return receipt
	}

	const pumpTxs = 100
	const measurementTxs = 10
	receipts := make([]*types.Receipt, 0, pumpTxs+measurementTxs)

	// Pump the backlog. No refund fires here.
	for range pumpTxs {
		receipts = append(receipts, sendUserTx(common.Big0))
	}

	// Raise the speed limit. The next block's internal tx drains the backlog
	// and BaseFeeWei() crashes, while header.BaseFee still holds the pumped
	// value — opening the drain window where the bug fires.
	tx, err = arbOwner.SetSpeedLimit(&ownerAuth, 7_000_000)
	Require(t, err)
	_, err = builder.L2.EnsureTxSucceeded(tx)
	Require(t, err)

	for range measurementTxs {
		receipts = append(receipts, sendUserTx(common.Big0))
	}

	endBalance, err := builder.L2.Client.BalanceAt(ctx, userAddr, nil)
	Require(t, err)

	// Confirm a drain actually happened; otherwise the bug can't reproduce.
	peak := receipts[0].EffectiveGasPrice
	for _, r := range receipts {
		if r.EffectiveGasPrice.Cmp(peak) > 0 {
			peak = r.EffectiveGasPrice
		}
	}
	final := receipts[len(receipts)-1].EffectiveGasPrice
	if peak.Cmp(final) <= 0 {
		Fatal(t, "base fee did not fall during the run; bug cannot reproduce",
			"peak", peak, "final", final)
	}

	expectedPaid := new(big.Int)
	for _, r := range receipts {
		expectedPaid.Add(expectedPaid,
			new(big.Int).Mul(r.EffectiveGasPrice, new(big.Int).SetUint64(r.GasUsed)))
	}
	actualPaid := new(big.Int).Sub(startBalance, endBalance)

	if actualPaid.Cmp(expectedPaid) != 0 {
		overRefund := new(big.Int).Sub(expectedPaid, actualPaid)
		Fatal(t, "balance delta does not match sum(effectiveGasPrice * gasUsed)",
			"expectedPaid", expectedPaid,
			"actualPaid", actualPaid,
			"overRefund", overRefund)
	}
}
