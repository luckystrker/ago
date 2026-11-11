# Wave 4 — formal model of the invariants (A2A reconnect, hub persistence, remote hub, offline delivery)

Status: models written and checked **before any wave-4 code**. Models live in `spec/tla/` and are run by the commit gate
(`node ~/.agent-toolkit/bin/tlc.mjs`). Python reference: `ag2/network/hub/core.py` (`_handle_receipt`, `_advance_cursor`,
`_replay_for_recipient`, `find_envelope_by_causation`, hydrate), `network/client/hub_client.py` (`_pending`),
`a2a/client.py` (resubscribe loop).

Step 0 (gap is real): no cursor / receipt / replay / causation-dedup / resubscribe code exists in Go
(`grep` for `inbox_cursor`, `since_envelope`, resubscribe finds only comments in `contrib/wslink`; `network/hub.go` indexes
causation but never rejects a repeat). Nothing is "already covered".

## Models and results

| Model | Covers | Distinct states | TLC |
|---|---|---|---|
| `HubCore.tla` | generic protocol (no `.cfg`, instantiated by the three scenario models) | — | — |
| `HubDelivery.tla` + cfg | 1, 2 (cross-channel), 3, index half of 5 — ONE addressee, 2 channels, 2 posts, link drops, client restart, nacks, lost receipts, hub crash/restart, retried posts; `MaxFaults=3` | 2 178 | pass (safety + liveness) |
| `HubMulti.tla` + cfg | 1 for **two addressees**, 2 (same-channel order, out-of-order receipts) — 2 agents with own links, 2 broadcasts in one channel, drops, lost receipts, hub crash/restart; `MaxFaults=2` | 9 563 | pass (safety + liveness) |
| `HubAccess.tla` + cfg | 4 (and safety of 2–3) — 3 agents, audience-restricted post, non-participant, forged receipts; `MaxFaults=1`; safety only (liveness is in the two models above) | 15 460 | pass |
| `HubRestart.tla` + cfg | 5 — registry, cursors, WAL durable vs volatile, two-step writes, crash inside hydrate, hydrate re-run; `MaxFaults=3` | 5 260 | pass |
| `LinkRPC.tla` + cfg | 6 — request/response correlation across link drops, late frames of the old connection, duplicated frames | 5 956 | pass |
| `A2AResubscribe.tla` + cfg | 7 — task lifecycle + progress, stream drops, resubscribe with snapshot | 564 | pass (safety + liveness) |

**Gate time.** The commit gate runs ALL models in one `spawnSync` with a 120 s timeout, so the budget is for the sum. Measured with the
gate's exact command (`time node ~/.agent-toolkit/bin/tlc.mjs`, all six at once) while the host had load average ≈ 15 (6 cores):

```
✓ A2AResubscribe 3.1 s   ✓ HubAccess 6.1 s   ✓ HubDelivery 4.9 s   ✓ HubMulti 20.3 s   ✓ HubRestart 2.7 s   ✓ LinkRPC 2.9 s
[tlc] все модели прошли: 6        real 0m40.2s (a second run: 47.6 s, load ≈ 14)
```
(Before the split the single-addressee model plus liveness on `HubAccess` took 112 s in the same conditions. The gate prints the state count
of TLC's first "Progress" line, not the final one; the table has the final counts.)

### Why two addressees are modelled separately (invariant 1, "every addressee")

`HubMulti` runs the full liveness property `NothingLost` and the safety invariants with two agents addressed by the same broadcasts, each with
its own link, bound-state, send pointer, cursor and client high-water; a hub crash and any drop/loss hit one or both. Independence of
addressees is checked two ways: `CursorBacked` (a cursor only ever holds an id that *that* agent received) and per-agent `NothingLost`
under every interleaving of the other agent's faults. Two mutants break exactly this and go red: **X1** (one cursor shared by all agents:
one agent's ack suppresses the other's replay) and **X2** (one agent's link drop discards the other's in-flight envelopes). A third
addressee adds nothing: every rule is per (agent, channel) and the model has no agent-to-agent interaction other than the shared WAL.

## Invariants → names in the model

| # | Invariant in words | Name(s) in the model | Kind |
|---|---|---|---|
| 1 | Every accepted envelope is eventually received and acked by every addressee, across offline / link drop / hub restart, if the addressee reconnects and stays | `HubCore!NothingLost` (`Eligible(a,k) ~> received ∧ cursor ≥ id`), weak fairness on connect / send / receive / ack processing / restart | liveness |
| 2a | Persisted cursor per (agent, channel) only moves forward | `CursorMonotonic`, `PersistedCursorMonotone` (HubRestart) | action property |
| 2b | Replay starts strictly after max(persisted cursor, client `since`), in WAL order | `ReplayStrictlyAfter` (every send names an id `>` both), `OrderedDelivery` | action property / invariant |
| 2c | An ack in one channel never suppresses replay in another | `CursorBacked` (cursor of channel X only ever holds an id of channel X that the agent received) + `NothingLost` | invariant + liveness |
| 3a | Duplicates allowed, never after the ack is persisted | `ReplayStrictlyAfter` (a send happens only above the persisted cursor); duplicates are reachable: witness `NoDupWitness` (defined in `HubCore.tla`, deliberately not listed in any `.cfg`) goes red in 8 steps | action property + witness |
| 3b | A post retried under the same causation key is not accepted twice | `NoDoubleAccept` (WAL has no repeated key), `IndexFaithful` (index = keys of the WAL after restart) | invariant |
| 4a | Never delivered to a non-audience member, non-participant or the sender | `NoLeak` (everything received or in flight is `Eligible`) | invariant |
| 4b | An ack from a connection that does not hold the agent does not move the cursor | `CursorBacked` (a forged ack names an id the victim never received) | invariant |
| 5a | After crash + restart registry and cursors equal the last persisted state | `RestartFaithful`, `MemoryEqualsPersisted` (HubRestart) | action prop. / invariant |
| 5b | Nothing deleted resurrects (passport, cursor) | `NoResurrection`, `NoOrphanCursor` | invariant |
| 5c | WAL replay (hydrate) is idempotent and never invents index entries | `MemoryEqualsPersisted` after `Rehydrate` (index not cleared), `IndexNeverInvented`, `WalAppendOnly` | invariant / action prop. |
| 6a | Each request gets at most one response | `AtMostOneResponse` | invariant |
| 6b | That response is its own | `ResponseIsOwn` | invariant |
| 6c | After reconnect no response of the old connection satisfies a new request | `NoStaleSatisfy` (+ `ResponseIsOwn`) | invariant |
| 7a | After reconnect the client's view converges to the server's task | `Converges` (`<>[](view = srv)`) | liveness |
| 7b | The terminal state is always observed | `TerminalObserved` | liveness |
| 7c | No events after terminal | `TerminalIsLast`, `StreamClosedAtTerminal`, `TerminalIsFinal`, `ViewMonotone` | invariant / action prop. |

## Mutations: every one went red (none survived)

Each mutation edits the MODEL (a copy in a scratch directory; the committed files are unmutated), removes one protection and is
checked against the property it should break. D2 and D15 need two envelopes in one channel and two receipts in flight, so they are run on `HubMulti`; D5, D9–D11 on `HubAccess`; D1–D4, D6–D8, D12–D14, D16–D17 on `HubDelivery`; X1, X2 on `HubMulti`.
34 mutations + 2 assumption mutants = 36. "Trace" = number of states in TLC's counter-example (≥ 3 means a real interleaving).

| ID | Mutation (protection removed) | Violated property | Trace |
|---|---|---|---|
| D1 | replay starts at `>=` (one envelope back from the high-water mark) | ReplayStrictlyAfter | 8 |
| D14 | replay ignores the client's `since` (cursor only) | ReplayStrictlyAfter | 8 |
| D12 | receipts of the replaced connection are still honoured after the rebind (replay pointer was computed before the late ack landed) | ReplayStrictlyAfter | 9 |
| D2 | `id > cursor` guard dropped: cursor can move backwards (receipts handled concurrently, out of order) — HubMulti | CursorMonotonic | 10 |
| D17 | cursor kept in memory only: a hub crash resets it | CursorMonotonic | 7 |
| D3 | a nack advances the cursor like an ack | CursorBacked (6) · NothingLost (10) | 6 / 10 |
| D6 | cursor persisted when the hub sends, before the client received | CursorBacked (4) · NothingLost (10) | 4 / 10 |
| D16 | one cursor per agent instead of per (agent, channel) | CursorBacked | 6 |
| D8 | `since` is one value for all channels (as the single `since_envelope_id` of the Python Hello frame): a newer id in channel B hides an envelope lost in flight in channel A | NothingLost | 12 |
| D15 | sender picks the newest envelope first (live dispatch overtakes replay) — HubMulti | NothingLost | 15 |
| D13 | no re-ack of the client's high-water on reconnect: a lost receipt leaves the cursor behind forever while `since` hides the redelivery | NothingLost | 10 |
| D4 | causation check skipped on post | NoDoubleAccept | 3 |
| D7 | restart does not rebuild the causation index from the WAL | IndexFaithful (4) · NoDoubleAccept (5) | 4 / 5 |
| D5 | receipt accepted from a connection that does not hold the named agent (3 agents) | CursorBacked | 5 |
| D9 | audience filter dropped from the send filter | NoLeak | 5 |
| D10 | "not the sender" check dropped | NoLeak | 4 |
| D11 | "channel participant" check dropped | NoLeak | 4 |
| X1 | one cursor per **channel shared by all agents** (one addressee's ack suppresses the other's replay) — HubMulti | CursorBacked (6) · NothingLost (14) | 6 / 14 |
| X2 | one agent's link drop discards the other agent's in-flight envelopes — HubMulti | NothingLost | 18 |
| R1 | restart loads cursors as empty | RestartFaithful (7) · MemoryEqualsPersisted (8) | 7 / 8 |
| R2 | deregister is not persisted (deleted agent resurrects at restart) | NoResurrection | 4 |
| R3 | deregister leaves the agent's cursor in the store | NoOrphanCursor | 6 |
| R4 | hydrate fold is not idempotent (toggles instead of adds) | MemoryEqualsPersisted | 5 |
| R5 | hydrate declares itself finished before folding the WAL | MemoryEqualsPersisted | 5 |
| R6 | a crash wipes the persisted cursors | PersistedCursorMonotone | 6 |
| R7 | a crash truncates the WAL | WalAppendOnly | 3 |
| L1 | response matched to a waiter by position, not by id | ResponseIsOwn | 7 |
| L2 | request ids are a per-connection counter (reset on reconnect) | ResponseIsOwn (7) · NoStaleSatisfy (7) | 7 / 7 |
| L3 | the waiter is not removed when a response arrives (duplicated frame answers twice) | AtMostOneResponse | 5 |
| A1 | resubscribe opens with no snapshot of the current task | Converges · TerminalObserved | 4 / 6 |
| A3 | resubscribe snapshot taken from the client's cached view, not the server | Converges (12) · TerminalObserved (11) | 12 / 11 |
| A2 | server keeps the stream open after the terminal event | StreamClosedAtTerminal | 4 |
| A5 | resubscribe at a terminal task leaves the stream open | StreamClosedAtTerminal | 4 |
| A4 | server emits one more event after the terminal one | TerminalIsLast (4) · ViewMonotone (7) | 4 / 7 |

Assumption mutants (the model needs these two environment guarantees; without them it breaks):

| ID | Assumption removed | Violated | Trace |
|---|---|---|---|
| D19 | a client that nacks keeps the link open and keeps acking later envelopes (nack(e1), ack(e2) moves the cursor past e1) | NothingLost | 9 |
| D20 | a receipt can be lost on a live link (not only with the connection) | NothingLost | 9 |

## Design requirements the models impose on the Go code (found while modelling)

1. **One ordered sender per (connection, channel).** Live dispatch and reconnect replay must be the same loop advancing one send
   pointer in WAL order (D15). Python runs replay (`_replay_for_recipient`) and live fan-out as separate paths; whether an interleaving
   in which a live envelope overtakes a pending replay can occur there was NOT verified; the model only shows that if it could, the cursor would
   jump past the replayed envelope, so the Go design must exclude it.
2. **`since` per channel.** Python's Hello frame carries ONE `since_envelope_id`, compared against every channel's cursor. This is a trap in
   Python's API, not a bug on its default path: the default `""` is safe, and an envelope can be skipped only when a client passes a *high*
   single `since` while an older envelope of another channel is still undelivered (D8). Python's own client does not do this by default. In Go
   the high-water is therefore per channel (the safe shape for the wire), so a caller cannot construct the trap. The wire shape differs from
   Python → needs an `AGO-DEVIATION` + DECISIONS entry (see Docs).
3. **Re-ack on reconnect.** The client sends an ack for its per-channel high-water right after it reconnects (D13). Python uses
   `since` only to bound replay and never advances the cursor from it, so a lost receipt leaves the cursor behind and "acked by
   every addressee" never becomes true (the default path is safe against *loss*; what is missing is the acknowledgement). Alternative with the same effect: the hub adopts `since` as an implicit ack. Deviation either way.
4. **Rebinding drops the replaced connection's receipts, or the replay re-reads the cursor at every send** (D12). The Python replay
   snapshots the cursor once per channel before looping.
5. **Idempotent post is new hub behaviour.** Python's `find_envelope_by_causation` is a lookup for handlers; the hub does not itself
   reject a repeated post. The invariant ("not accepted twice") needs the hub to consult the index on accept and to rebuild the index
   from the WAL on hydrate (Python hydrate already rebuilds it, `core.py:3355`). Terminal-channel pruning of the index stays as in Python.
6. **After a nack the connection stops delivering that channel until reconnect** (D19): otherwise a later ack moves the cursor past
   the nacked envelope and it is never replayed, contradicting "a nack leaves the cursor in place".
7. **A receipt may be lost only together with its connection** (D20): true for WebSocket/TCP and the in-process link; a transport
   that can drop a single frame needs an explicit ack timeout + resend.
8. **Request ids must be unique for the lifetime of the process**, not per connection (L2). Python `make_id()` already is.
9. **Resubscribe must open with the current task** (A1/A3) and the server closes the stream right after the terminal event (A2/A4/A5).

## Property-based tests the code will need (for the executor tasks)

Use a fake link (in-memory, with scriptable drop / delay / duplicate), a fake store (with a "crash now" hook that discards
unflushed state), a fake clock; no network. Random schedules of: post, retry post, connect, drop, client restart, crash/restart,
nack, delayed/lost/forged receipt; assertions after every step:

- **1** after the schedule is run to quiescence with the addressee connected, every accepted envelope was received and `cursor ≥ id` for each addressee (also after posts made while offline, and after a crash in the middle of replay).
- **2** (a) the persisted cursor of each (agent, channel) never decreases across all steps incl. restarts; (b) every send names an id `>` max(cursor, since) of its channel and is in WAL order within the channel; (c) acking in channel A leaves replay of channel B untouched (two-channel schedules, drop an envelope of B in flight, ack in A).
- **3** (a) redelivery after a client restart / lost receipt happens, and never for an id `≤` the persisted cursor at send time; (b) N retries of one `(channel, sender, causation_id)`, with a hub restart in between, leave exactly one WAL entry.
- **4** with random audiences, participants and senders: every delivered envelope satisfies `participant ∧ in audience ∧ ≠ sender`; a receipt naming agent X from a connection that does not hold X, or from a replaced connection, never changes any cursor.
- **5** crash at every write boundary (between the in-memory update and the store write, inside hydrate): after restart registry, cursors and index equal the store; deregistered agents and their cursors stay gone; hydrate twice == hydrate once; WAL is append-only.
- **6** concurrent requests over a link with random drops, reordering, duplicated and late response frames: each caller gets ≤ 1 response and it is the response to its own request; no response produced before a reconnect completes a request issued after it.
- **7** against a fake A2A server with scripted task progress: for random drop points (including during and after the terminal event) the client ends with the server's final task, observes the terminal state exactly once, sees nothing after it, never moves backwards; the first event after a resubscribe is the current task.

## What stays outside the models

- Real framing, auth, TLS, WebSocket ping/pause, backpressure and inbox capacity (the in-flight queue is unbounded); timeouts, backoff and `max_reconnects`
  (the A2A model assumes the client keeps resubscribing until it observes the terminal state; the Python give-up path raises `A2AReconnectError` and polls — that explicit failure is not modelled).
- WAL durability details: the model assumes an append is atomic and durable once acknowledged (no torn tail, no fsync gap) and that the cursor map is one atomic write.
- Envelope id generation: the models assume ids are totally ordered and grow in WAL order (Python compares string ids); the Go id format must keep that across restarts and clock skew.
- A connection holding several agents (model: one agent per connection); federation / remote owners; channel lifecycle (terminal pruning of cursors and index); protocol adapters (what an envelope means).
- Link RPC: request timeouts, hub-side deduplication of side-effecting requests (covered by the causation invariant), request kinds; A2A: input-required / cancel states, artifacts' content, polling fallback, SSE parsing.
- HubRestart models one in-flight write at a time and one channel; concurrent writers are covered only by the delivery model (receipts processed in any order).
- Liveness is checked under weak fairness and a finite fault budget (`MaxFaults`); a perpetually failing link is, by assumption, not a case where delivery is promised.
