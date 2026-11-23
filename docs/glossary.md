# Glossary

Chain IDs, ordering rules and fee parameters per chain are in [chains.md](chains.md).

## Chains and sequencing

| Term | Meaning |
|---|---|
| Arbitrum One | Arbitrum's main L2, chain ID `42161`, Nitro family |
| Nitro | Offchain Labs' Arbitrum node software. Rosetta's `nitro` family covers chains that share its execution, feed and fee semantics |
| OP Stack | The Optimism rollup stack. Rosetta's `op-stack` family covers OP Mainnet, Base, Unichain and Ink |
| Chain family | The set of sync, confirmation and fee rules a process applies. Selected by configuration, never inferred from a chain ID |
| Parent chain | The chain an L2 settles to. Must be nonzero and different from the L2's own chain ID |
| Sequencer | The L2 component that accepts, orders and publishes transactions |
| Sequencer feed | The sequencer's WebSocket broadcast of sequenced messages. Rosetta uses it to wake the RPC follower |
| `sequenceNumber` | A feed message's monotonically increasing position |
| Fast Feed | Arbitrum's paid, authenticated per-transaction stream, published during the auction round before the block closes. It carries partial receipts with logs, no final block hash and no replay; inclusion is not guaranteed |
| First-index mapping | The offset between Fast Feed transaction positions and block transaction indexes, measured per chain with `cmd/fastprobe` |
| PGA | Priority Gas Auction: Arbitrum One's ordering by priority fee within short rounds |
| Flashblocks | OP Stack sub-block preconfirmations, published as `(payload, index)` frames. Rosetta requires frames with full receipts |
| Priority ordering | Transactions in a block are ordered by tip |
| Arrival ordering | Transactions are ordered first-come, first-served; the tip does not affect order |
| Batch poster | The Nitro address that posts L2 batches to L1 |
| L1 block number | On Nitro, the parent-chain block number seen by L2 transactions and by conditional-transaction bounds |

## State and sync

| Term | Meaning |
|---|---|
| Canonical | The chain view built only from complete blocks whose parent is the accepted head |
| Speculative branch | Pool state built from preconfirmation receipts (Fast Feed or Flashblocks) on top of canonical state. It never writes canonical state |
| Reconcile | Compare a speculative branch with a new canonical block, keep the matching prefix and replay the rest, or reset on conflict |
| Stream-sealed block | A canonical block assembled from a stream instead of RPC, applied when its parent matches and verified against RPC on a sample |
| Reseed | Reload every known pool at a fresh, hash-pinned tip after a reorg or verification failure |
| Pinned read | An RPC read at a specific block hash (EIP-1898 `blockHash` with `requireCanonical`) |
| Reorg | A change of the canonical chain below the follower's head |
| Receipt | A transaction's result: status, gas used and logs |
| Log | A contract event. Pool logs are the main input to pool state |
| Shallow state | Per-pool price or reserves, active liquidity and fee, kept for every tracked pool |
| Deep state | Tick bitmap and per-tick liquidity in a loaded window around the current tick |
| Tick window | The tick range whose deep state is loaded. A quote that leaves it fails instead of assuming no liquidity |
| Dirty pool | A pool whose state changed since the last search over it |
| Owed pool | A pool whose short or long routes still need a search at the current lineage |
| Pool snapshot | Deterministic JSON of the pool store, used to resume without a full bootstrap |
| Undo history | The store's bounded per-block rollback frames |

## Pools and math

| Term | Meaning |
|---|---|
| `PoolKey` | Pool identity: address, protocol kind, V4 pool ID and hooks |
| Adapter | Protocol code that loads, updates and quotes a pool kind |
| `slot0` | A V3-family pool's first storage slot, holding `sqrtPriceX96`, the current tick and more |
| `sqrtPriceX96` | The square root of the price token1/token0 in Q64.96 fixed point |
| Tick | A discrete price point; liquidity changes only at initialized ticks, aligned to `tickSpacing` |
| `liquidityNet` | The change in active liquidity when the price crosses a tick |
| Virtual reserves | The constant-product reserves implied by in-range liquidity at the current price: `x = L·Q96/sqrtP`, `y = L·sqrtP/Q96` |
| V4 pool ID | `keccak256(abi.encode(currency0, currency1, fee, tickSpacing, hooks))`; V4 pools live in one PoolManager |
| Hook | A V4 pool's callback contract. Pools with non-zero hooks are not traded |
| Dynamic fee | A pool fee that can change without a swap: Slipstream, Camelot V3, Velodrome V2 and V4 protocol fees |
| Shallow model | The ranking approximation `F(x) = A·x / (B + C·x)` composed from per-leg constant-product terms |
| Möbius composition | Composing two `a·x/(b + c·x)` legs gives the same form, so a route reduces to three coefficients |
| Closed-form input | The profit-maximizing input of the shallow model, `(sqrt(A·B) − B) / C` |
| Exact quote | A quote computed with the protocol's own integer math and rounding |

## Search

| Term | Meaning |
|---|---|
| Route, cycle | A closed sequence of swaps from a funding asset back to itself |
| Route catalog | The precomputed table of cycles up to `max_depth` hops, with pool postings |
| Postings | The index from each pool to the routes that pass through it |
| Background exploration | A bounded walk of the graph that finds cycles outside the catalog |
| Observed route | A cycle extracted from a committed transaction and remembered for later searches |
| Coarse ranking | Ordering affected routes by the shallow model before any exact quote |
| K | The number of routes per funding asset that receive exact sizing |
| Phantom pool | A pool that ranks well but repeatedly quotes non-positive, temporarily excluded from ranking |
| Sizing | Searching for the input that maximizes exact gross profit |
| `initial` quote | The first positive exact quote of a route, published before sizing finishes |
| `optimized` quote | The result of complete sizing; the only quote that goes on to execution |
| Fast lane | The reactive runtime's per-frame quote of short routes through changed pools, ahead of the full search |
| Golden replay | A recorded block range replayed as a regression test that the search still finds an equal or better opportunity |
| Backtest | Running the state and search code over recorded data |

## Execution

| Term | Meaning |
|---|---|
| Executor | The contract that runs one route atomically and checks its profit |
| Operator | An address allowed to call the executor's standard entry points |
| Owner | The executor's immutable administrator, who pauses, sets operators and sweeps profit |
| `configVersion` | A counter in executor slot 0, raised on every pause or operator change |
| Funding mode | How the principal is supplied: wallet, inventory, V3 flash loan, V3 flash swap or V4 flash accounting |
| Flash loan | Borrowing and repaying within one transaction |
| Flash swap | A swap that delivers output before collecting input, so the route needs no principal |
| `minAmountOut` | A per-leg minimum output checked by the executor |
| `minProfit` | The executor's closing profit floor. Rosetta encodes the fee budget and minimum net profit into it |
| Price precheck | An executor check that bounded pools have not moved against the route before any funds move |
| Inventory check | The executor's requirement that intermediate token balances end exactly where they started |
| Hot route | The `executeHot` path, sent only as a conditional transaction |
| Conditional transaction | A Nitro `eth_sendRawTransactionConditional` with expected storage values (`knownAccounts`) and time bounds |
| `-32003` | The error for a failed condition. The transaction is dropped without gas or nonce use |
| Quiet pool | A pool unchanged for long enough that a condition can hold its storage |
| Ready plan | An `exec.ReadyPlan`: a route confirmed by a full executor call at a pinned state, with fees and versions checked. Not a guarantee of inclusion |
| Reactive intent | An `exec.ReactiveIntent`: a sendable route admitted on local quotes, cached context and contract guards, without EVM pre-confirmation |
| Shadow | Reactive mode that builds and records intents without signing or sending |
| Live | Reactive mode that signs and sends |
| Execution paper | Simulating the real executor call at a chosen state, off the send path |
| Lane | One sender address with its own key, nonce sequence and journal |
| Dispatch journal | A lane's persisted record of nonces, signed variants, reservations and settlements |
| Replacement | A same-nonce transaction with higher fees and a better route or amount |
| Cancellation | A same-nonce zero-value self-transfer sent when every pending variant has become unprofitable |
| Release | Freeing a nonce once any of its variants is seen in the feed or a block |
| Settlement | A nonce's booked outcome. Paid fees arrive from the canonical receipt after the first sighting. Not L1 finality |
| Quote reserve | A margin for quote error, taken as a share of the route's final output including principal |
| Stop file | A file whose presence stops all signing and rebroadcasting |
| Token risk | Persistent evidence that a token transferred less than requested. Affected tokens are excluded; absence of evidence is not a guarantee |
| Pool quarantine | A persistent exclusion of a pool implicated in failed executions |

## Fees and bidding

| Term | Meaning |
|---|---|
| Base fee | The block's EIP-1559 base fee per gas |
| Priority fee, tip | The per-gas amount above the base fee. Sets order on priority chains and can raise Slipstream fees |
| Tip cap | The signed maximum priority fee per gas; the effective tip is also limited by the fee cap minus the base fee |
| Paid priority fee | Effective tip × receipt `gasUsed`. Tip × gas limit is only the upper bound |
| Poster gas | Nitro's L1 cost, charged as extra gas inside the transaction's gas |
| L1 data fee | OP Stack's charge for posting the transaction's data to L1, based on compressed size |
| Operator fee | An additional OP Stack fee set by the chain operator |
| Fee budget | The conservative maximum network fee for an intent, reserved before sending |
| Worth | In the bid model, the native-currency surplus divided by expected gas: our valuation, not a rival's profit |
| Opportunity window | A short period in which related rival transactions are grouped as competing for one opportunity |
| Win probability | The calibrated chance that a tip lands the route, estimated from rival bids and our own outcomes |
