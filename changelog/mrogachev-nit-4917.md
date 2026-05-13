### Changed
- Add `TransactionFiltering.Enable` master switch (default false), replacing `AddressFilter.Enable`.
- Scope `EnableETHCallFilter` to eth_estimateGas only; no longer gates prechecker filtering.
- Skip prechecker transaction-filter dry-run on sequencer nodes.
