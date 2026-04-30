// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package precompiles

import (
	"github.com/ethereum/go-ethereum/arbitrum/multigas"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"

	"github.com/offchainlabs/nitro/util/arbmath"
)

// ArbInfo povides the ability to lookup basic info about accounts and contracts.
type ArbInfo struct {
	Address addr // 0x65
}

// GetBalance retrieves an account's balance
func (con ArbInfo) GetBalance(c ctx, evm mech, account addr) (huge, error) {
	if err := c.Burn(multigas.ResourceKindComputation, params.BalanceGasEIP1884); err != nil {
		return nil, err
	}
	return evm.StateDB.GetBalance(account).ToBig(), nil
}

// GetCode retrieves a contract's deployed code
func (con ArbInfo) GetCode(c ctx, evm mech, account addr) ([]byte, error) {
	if err := c.Burn(multigas.ResourceKindStorageAccess, params.ColdSloadCostEIP2929); err != nil {
		return nil, err
	}
	code := evm.StateDB.GetCode(account)
	words := arbmath.WordsForBytes(uint64(len(code)))
	if err := c.Burn(multigas.ResourceKindStorageAccess, params.CopyGas*words); err != nil {
		return nil, err
	}
	return code, nil
}

// ApeChain yield precompiles — these exist because on-chain contracts call them.

const BalanceValuesGas = params.BalanceGasEIP1884
const ConfigureDelegateYieldGas = 2 * params.ColdAccountAccessCostEIP2929

func (con ArbInfo) GetBalanceValues(c ctx, evm mech, account addr) (huge, huge, huge, error) {
	if err := c.Burn(multigas.ResourceKindComputation, BalanceValuesGas); err != nil {
		return nil, nil, nil, err
	}
	values := evm.StateDB.GetBalanceValues(account)
	return values.Fixed.ToBig(), values.Shares.ToBig(), values.Debt.ToBig(), nil
}

func (con ArbInfo) GetYieldConfiguration(c ctx, evm mech, account addr) (uint8, error) {
	if err := c.Burn(multigas.ResourceKindComputation, BalanceValuesGas); err != nil {
		return 0, err
	}
	values := evm.StateDB.GetBalanceValues(account)
	return values.Flags, nil
}

func (con ArbInfo) GetDelegate(c ctx, evm mech, account addr) (addr, error) {
	if err := c.Burn(multigas.ResourceKindComputation, BalanceValuesGas); err != nil {
		return addr{}, err
	}
	values := evm.StateDB.GetBalanceValues(account)
	return values.Delegate, nil
}

func (con ArbInfo) ConfigureAutomaticYield(c ctx, evm mech) error {
	evm.StateDB.SetFlags(c.caller, types.YieldAutomatic, nil)
	return nil
}

func (con ArbInfo) ConfigureVoidYield(c ctx, evm mech) error {
	evm.StateDB.SetFlags(c.caller, types.YieldDisabled, nil)
	return nil
}

func (con ArbInfo) ConfigureDelegateYield(c ctx, evm mech, account addr) error {
	gas := ConfigureDelegateYieldGas
	if evm.StateDB.Empty(account) {
		gas += params.CallNewAccountGas
	}
	if err := c.Burn(multigas.ResourceKindComputation, gas); err != nil {
		return err
	}
	evm.StateDB.SetFlags(c.caller, types.YieldDelegated, &account)
	return nil
}
