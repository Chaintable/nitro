// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package precompiles

import (
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
	if err := c.Burn(params.BalanceGasEIP1884); err != nil {
		return nil, err
	}
	return evm.StateDB.GetBalance(account).ToBig(), nil
}

// GetCode retrieves a contract's deployed code
func (con ArbInfo) GetCode(c ctx, evm mech, account addr) ([]byte, error) {
	if err := c.Burn(params.ColdSloadCostEIP2929); err != nil {
		return nil, err
	}
	code := evm.StateDB.GetCode(account)
	if err := c.Burn(params.CopyGas * arbmath.WordsForBytes(uint64(len(code)))); err != nil {
		return nil, err
	}
	return code, nil
}

// TODO: these values can be adjusted via a hardfork
const BalanceValuesGas = params.BalanceGasEIP1884

// in general, touches up to two other addresses: the old and new delegate, both of which are generally cold.
// gas for unsetting the delegate is charged here instead of in the ConfigureAutomaticYield() or ConfigureVoidYield() functions.
const ConfigureDelegateYieldGas = 2 * params.ColdAccountAccessCostEIP2929

// fixed, shares, debt
func (con ArbInfo) GetBalanceValues(c ctx, evm mech, account addr) (huge, huge, huge, error) {
	if err := c.Burn(BalanceValuesGas); err != nil {
		return nil, nil, nil, err
	}
	values := evm.StateDB.GetBalanceValues(account)
	return values.Fixed.ToBig(), values.Shares.ToBig(), values.Debt.ToBig(), nil
}

// flags
func (con ArbInfo) GetYieldConfiguration(c ctx, evm mech, account addr) (uint8, error) {
	if err := c.Burn(BalanceValuesGas); err != nil {
		return 0, err
	}
	values := evm.StateDB.GetBalanceValues(account)
	return values.Flags, nil
}

// delegate
func (con ArbInfo) GetDelegate(c ctx, evm mech, account addr) (addr, error) {
	if err := c.Burn(BalanceValuesGas); err != nil {
		return addr{}, err
	}
	values := evm.StateDB.GetBalanceValues(account)
	return values.Delegate, nil
}

// TODO: emit events when these functions are called
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
	// prevent state bloat
	if evm.StateDB.Empty(account) {
		gas += params.CallNewAccountGas
	}
	if err := c.Burn(gas); err != nil {
		return err
	}
	evm.StateDB.SetFlags(c.caller, types.YieldDelegated, &account)
	return nil
}
