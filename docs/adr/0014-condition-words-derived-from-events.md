# 14. Condition storage words derived from pool events

## Status

Accepted

## Context

A condition (ADR 0013) must state the exact 32-byte storage word the sequencer will compare. Our pool state holds decoded fields (sqrt price, tick, liquidity, reserves), but the word that packs the price also packs fields our state does not otherwise track: the oracle's observation index and cardinality, protocol fee bits, Algebra's fee and timepoint fields.

Reading each word over RPC on the send path costs a round trip per pool, which the opportunity rarely survives. Reading in the background after every change costs a read per pool per change, and the pools that change most often are the ones most worth holding. A word read a few blocks behind the head is also wrong for a pool that changed since.

## Decision

- For V3-family, Slipstream, Camelot V3, V2 and hookless V4 pools, the engine keeps a word model per pool and advances it from the same events that update pool state. For the V3 shape:
  - a swap that changes the tick, or an in-range mint or burn, writes an oracle observation;
  - a second write in the same block timestamp changes nothing;
  - otherwise the index advances by one modulo the cardinality, which first grows to `cardinalityNext` when the index sits on its last entry.
  Other shapes follow their own packing rules. Bits the model does not follow are kept as read.
- A model starts from one storage read taken a few blocks behind the head, when the read matches our state, and then replays the blocks since that read from a small ring of applied blocks.
- Each model is compared with a fresh chain read every ten minutes. A mismatch drops the model until the next matching read.
- A gap in applied block numbers drops every model, since their events were not seen.
- Pools named by the current pre-confirmation frame's pending logs are treated as changing and carry no condition.
- Models are saved with the last applied block at shutdown and restored only if the next applied block is that block's child. After a start without saved models, sends wait a bounded time for a first read pass, so routes are not sent without conditions they could have had.
- Kinds whose word carries data no event reports (Solidly V3's fee in `slot0`) are read only, never derived.

## Consequences

- Most held pools need one read in their lifetime instead of one per change, and frequently traded pools can be held as soon as they go quiet.
- The derivation mirrors on-chain bookkeeping of each protocol. A fork with different oracle rules produces wrong words. Because a wrong word only drops the transaction free, such errors show as condition rejections without a rival and are caught by the periodic comparison.
- Adding a protocol to conditional sends means writing its word model and checking it against chain reads before enabling it.
