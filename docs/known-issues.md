# Known issues

Findings that were reviewed, scored and **deliberately left open**: risk = frequency (1–5) ×
harm (1–10) below 5, so they are catalogued here instead of being fixed on the spot. Risk ≥ 5
is fixed; harm 10 (money, data) goes to the owner. Each entry names where it came from; the
decision records are in [DECISIONS.md](DECISIONS.md), the per-package status in
[PORT_STATUS.md](PORT_STATUS.md).

Entries below come from the gap-audit wave 3 review (2026-11-10) unless stated otherwise;
the `(W4-...)` and `(D-117..D-123)` entries come from the wave-4 reviews (2026-11-11).

## Flaky tests

- **`mcpserver` resume end-to-end test, first 20-second timeout (D-109).** One run under load
  hung until the test's 20 s timeout; the cause was not found. The other flake seen in the same
  test (`stream ended`) was a real race and is fixed (a turn finishing between the replay and
  the attach). The test now asserts `hijacked == 1`, `failed == 0`, so a failed connection hijack
  can no longer pass for a hang. Afterwards the executor ran 200 and the reviewer 150 loaded
  iterations: 0 failures. If it reappears, the failing assertion names the class of failure.
- **`network/TestAuditLogRecordsListenerEvents`** failed once in a full run and passed 6 of 6
  reruns (found by a reviewer; the listener/audit code is from the D-104 wave). Cause unknown.
- **`network/TestWorkflowRoundRobinMaxTurns`** is flaky under load (found by a reviewer).
  Cause unknown.

## Flakes already fixed (kept for the record)

- **`acp/TestProviderStartupFailures/exits_during_the_handshake`** (was risking 2 × 3 = 6): the fake ACP agent
  exits during the handshake, the client's `initialize` write lands in the already-closed pipe and surfaces as
  `acp: write: write |1: broken pipe` instead of a transport error wrapping `ErrAgentExited` (found by the
  ticket-013 reviewer, 2 of 200 runs). That was a product-code race, not only a test flake: a caller received a
  raw write error where the contract promises `ErrAgentExited`. Fixed in the wave-G ticket 016 — a write that
  finds the agent's stdin gone waits (bounded, 500 ms) for the process to end and wraps the failure in
  `ErrAgentExited`; the write error itself stays reachable through `errors.Is`. 26 of 500 runs of
  `TestProviderStartupFailures` failed before the change, 0 of 500 after; the added deterministic test
  `acp/TestBridgeAgentGoneBeforeHandshake` failed 500 of 500 on the old code. Evidence:
  [acceptance/v1.3.4-016.md](acceptance/v1.3.4-016.md).
- `tools/TestExecutorHitlFailureAbandonsBatchAndPropagates` ("call events sent = 1, want 2"
  under full-run load): fixed in W1-E3, commit `0d0b11a` (D-100).
- `acp/TestServeAgentStreamsAndContinues` (`chunks=[Hey] want [He y]` under load): fixed in
  W2-ACP, commit `476ade8` (D-105).
- `agui/TestServerRetentionExpiresUnansweredQuestion` (fixed `40 ms` retention and a
  `time.Sleep(120 ms)`): fixed in the wave-E ticket 007. The test now drives the retention
  clock (`WithClock`) past the deadline instead of outlasting it, and the "the expired
  question is dropped" check moved to `agui/TestServedTurnsDropExpiredQuestion`; the HTTP test
  keeps the wire-level claim (a late answer does not resume the turn). 20/20 under `-p 1` in
  0.14 s, against 2.8 s before.

## Defects fixed (kept for the record)

- **`agui` held turns released under the registry lock (wave-E ticket 008, was risk 3 × 9 = 27;
  a turn releasing itself, was 2 × 4 = 8).** `servedTurn.release()` closed the run and waited
  for the turn to wind down, and the winding-down turn takes `servedTurns.mu` (`discard`). It was
  called under that lock when a held turn was superseded or evicted at `MaxHeld` (`hold`) and
  when a held turn expired (`evictExpiredLocked`, reached from `hold`, `take`, `holds`), so one
  POST that parked a question or met an expired one wedged the whole endpoint. When the
  eviction victim was the turn asking the question, the wait lasted until the question's own
  timeout and the POST answered with `human input timed out` instead of the interrupt. Fixed:
  `release()` is now what Python's `release()` is, a cancel that does not wait (the run closes on
  its own goroutine); `aclose()` is the waiting kind and is kept for `releaseAll` and the
  cancelled-resume path. Victims are removed from `held` under the lock and released after
  `Unlock`. Tests: `agui/release_locking_test.go`.

## Test coverage gaps in new code

- **`agui` `aclose` waits for the run scope, not for the driver (ticket 008, risk 1 × 2 = 2,
  `agui/interrupts.go:317`).** Python's `aclose` gathers the whole task; ours waits for
  `run.Close()`, and the driver's tail (`endTurn`, `discard`, closing `driverDone`) finishes just
  after `Close` returns. Nothing reads that tail after shutdown or a cancelled resume, which
  already take their closings from the stopped run; the tests wait for `driverDone` with a deadline.
- **`agui` the order of `t.out.hold()` before `s.turns.hold` is not pinned by a test (ticket 008,
  risk 2 × 2 = 4, `agui/interrupts.go:715`).** It keeps a turn that releases itself (registry full,
  its own deadline the nearest) from writing a cancelled `RUN_FINISHED` into the exchange before its
  pause. Reverting the order does not turn any test red in 500 runs: the window is a scheduling
  race that the self-release test only rarely enters.
- **Cron search horizon boundary (D-106).** No test pins the one-year horizon edge: with
  `now = 2026-11-10 10:00:30 UTC` and `1 16 10 11 WED` the code returns the `now+1h` fallback
  as Python does, but the mutant `i < N` → `i <= N` returns `2027-11-10 16:01` and survives.
  The code matches Python; the missing piece is one fake-clock test.
- **`mcp` client edges (D-108), risk 4 each:** a round request that is left unanswered (the
  `scope.failure` return); an empty `content` in a sampling answer; a `0x7f` byte in
  `encodeHeaderValue`; a 4xx answer with a non-JSON body.
- **`config/anthropic` stream (D-110):** an error returned by a subscriber on server-tool events
  is masked by the later send of the same call and is not covered in isolation; the comment
  above one server-tool replay test still mentions the null `page_age` that is now stripped.
- **`toolpacks/skills.go` (D-112):** 26 surviving mutants are in code older than wave 3
  (frontmatter parsing, strict metadata validation, resource listing); two in new code
  (`RunScript` host-Python choice, a sort comparator) are equivalent.
- **Sandboxes (D-115):** 15 surviving mutants, all equivalent or boundary cases (an exact
  status of 300, the 64 MiB file limit, an empty query or content type).
- **`network` wire boundaries (W4-N2, risk 2):** the surviving mutants are boundary cases with
  no exact-edge test — the channel sort comparator, `len == limit`, a value of exactly 500
  characters.
- **Forced response, slow signal (D-127, ticket 002, risk 1×3 = 3).** The mutant that removes
  `if resp.ResponseForce { return resp, nil }` (`agent/turn.go:112`) is caught, but
  `TestForcedClientHandoffWithQueuedInputWaitsForClientResult` (`agent/forced_turn_test.go:122`)
  hangs until the `go test` timeout instead of failing fast: the loop re-sends the
  `ToolCallsEvent`. A per-test deadline would turn it into a quick failure.
- **Forced response, AG-UI tests (D-127, ticket 002, risk 1×2 = 2).**
  `TestForcedClientHandoffEndsRunWithQueuedInput` (`agui/forced_turn_test.go:67`) and
  `TestForcedFinalToolEndsRunWithQueuedInput` (`agui/forced_turn_test.go:93`) assert the event
  sequence, the reply text and the model-call count; a mutant that drops the calls handed off to
  the client still passes them, and only the `agent` test above catches it.
- **DecisionSet, error sentinel wording (D-128, ticket 003, risk 2×2 = 4).** D-128 says every
  rejection is `ErrInvalidDecisionSet` (wrapping `ErrNotADecision`), but a score whose levels do
  not start at 0 is reported by the OpenAI provider with `ErrNotADecision` only
  (`config/openai/decisions.go:694`). `errors.Is(err, response.ErrNotADecision)` works for both;
  a caller matching `ErrInvalidDecisionSet` misses that case. Fix either the wrapping or the record.
- **DecisionSet, metadata of a struct field vs a scalar (D-128, ticket 003, risk 1×2 = 2).** The
  Python test `test_metadata_is_each_answer_by_field` has no Go twin: nothing asserts that a
  field's entry in a struct's metadata equals the metadata of the same question asked as a
  scalar. The helper is shared, so it holds today; no test pins it.

## Behavior notes

- **`network` observation de-duplication is memory-only (W4-N1, risk 4).** `Hub.RecordObservation`
  remembers the `(owner, task)` pairs it already counted in memory, so after a hub restart a
  repeated terminal event of the same task counts a second time in the owner's observed track
  record. Python does the same (`_observed_task_ids` is memory-only), so it is left as is.
- **`network` remote `AgentClient.ChannelTools` uses the built-in adapters only (W4-N2,
  risk 2).** Over the wire the tool set is rebuilt on the client from the channel log; a
  custom adapter registered on the hub yields an empty tool set remotely, while the
  in-process client still sees it.
- **`network` the creator never acks its own lifecycle event (W4-N2, risk 4).**
  `handleReceipt` drops an ack whose sender is the recipient, and the creator is the sender of
  its own `ag2.channel.opened`, so that envelope is never acked (Python advances the cursor
  there). Harmless while nothing replays; once the replay lands, a fresh client can be sent
  `opened` twice.
- **`network` an ack scans the channel log under the hub lock (W4-N2, risk 4).** Resolving the
  envelope (`envelopeByID`) is O(n) per ack on a hot path.
- **`network` cursor API vs the wire (W4-N1/N2, risk 2).** `Hub.AdvanceInboxCursor` and
  `Hub.RecordNack` do not check that the envelope exists in the channel; the wire path does
  (existence, participant, not the sender). An embedder calling the methods directly can move
  a cursor past an envelope it never received.
- **`network` `NoAuth` takeover (W4-N2, risk 2).** A Hello for an agent registered with `NoAuth`
  displaces the current holder by name, as in Python; it is documented in D-119 and MIGRATION
  and pinned by a test, but `NoAuth`, `DefaultAuthRegistry` and `handleHello` carry no warning
  comment in the code.
- **The race detector does not run in this environment** (no C compiler for cgo), so the wave-4
  concurrency was checked by reading the lock order (`dispatch` outside `h.mu`;
  `bindEndpoint`/`dropEndpoint` release `w.mu` before `h.Attach`) rather than by
  `go test -race`. Unverified area, not a known defect.
- **TLC gate budget (W4-F, D-117).** The gate runs all six models in one `spawnSync` with a
  120 s budget; measured 40–53 s at load 9–15, of which `HubMulti` is 20–28 s. On a heavily
  loaded host the budget can be exceeded, and the failure then reads like a broken model
  rather than a timeout.

- **`mcpserver` (D-109).** Not done by design: the standalone listener of the *client* makes no
  second attempt after a 405 (the SDK tries twice; the outcome is the same); a resume stream does
  not repeat the priming event, and its `200` is sent before the replay result is known (same as
  the SDK); a user callback passed to `mcp.WithNotifications` that calls the client again
  deadlocks (documented on the option); a custom `EventStore` that does not implement
  `StreamForgetter` keeps the streams of ended sessions; there is no back-channel over stdio, so an
  elicitation or sampling request there is answered by the agent's own hook or model.
- **JSON-Schema validation of tool arguments (D-109).** `unevaluatedProperties`/
  `unevaluatedItems`, `$dynamicRef`, external `$ref`, patterns RE2 cannot compile and the draft-07
  array form of `items` are not enforced: the call is checked less strictly than Python's
  `jsonschema` would (accepting too much, never rejecting a valid call). The step budget is per
  value, so a pathological operator-written schema with an exponential `anyOf` fan-out stops
  checking the rest of that one value's keyword chain.
- **`mcp` client (D-108).** `Mcp-Param-*` headers from `x-mcp-header` are not set. The retry after
  `-32022` is omitted (one modern version is offered).
- **`tools/call` with an unknown tool name** calls the provider before rejecting the name
  (found in the W1-E4 review); the order against Python is unchecked and the effect harmless.
- **`contrib/searchtools` (D-113/D-114).** Serply answers a non-object JSON body with an error
  where Python returns an empty result; Keenable drops fractional seconds; a Glasser poll can
  overrun `wait_timeout` by up to 60 s. Only Exa, Firecrawl and TinyFish were wire-checked against
  their SDKs; Glasser, Keenable, Serply and Xquik were checked against the Python sources only.
  The package-level `sleep` and default HTTP client are test seams, so the tests must not run
  with `t.Parallel`.
- **Sandboxes (D-115).** The Daytona toolbox paths come from the published OpenAPI and the SDK
  source, not from a live account, and the Docker backend was tested only against a fake Engine
  API: run both once against the real services before relying on them. The Docker
  backend supports no TLS or ssh daemon endpoint.
- **`policies` transparency (D-106).** Episodic transparency is off by default where Python
  turns it on; use `WithEpisodicTransparency(true)` for parity.
- **`config/anthropic` cache sum with a measured-zero cache (ticket 001 / D-126, risk 3; found by
  the ticket-001 review, 2026-11-19).** The surviving mutant replaces the presence check inside
  `NormalizeUsage`'s cache sum with `> 0`. Its only observable difference is a payload with both
  cache counters reported as `0` and no `input_tokens`: the mutant then reports no prompt at all
  (`PromptTokens == nil`) where the code reports a measured `0` — the summed value is `0` either
  way, and the Messages API does not send a usage object without `input_tokens`, so the case does
  not occur in practice. Frequency 1 × harm 3 = 3, **below the threshold, not fixed**: the code
  keeps presence semantics (a reported `0` is a measured cache miss) and
  `TestNormalizeUsageCountsCacheInPrompt` pins the values, including the reported-zeros case
  next to a present `input_tokens`.

- **`network` property tests under host load (v1.3.2 wave A check, 2026-11-20, risk 2 × 3 = 6).**
  On a host with load average 31 (6 cores), the full `go test ./...` failed once in
  `TestRemoteRPCProperty` ("hub request timed out: echo", seed 20261112722), and a rerun of
  `TestRemoteRPCProperty`/`TestDeliveryProperty` failed again with another seed (20261112000) —
  both after the wave merged, though neither commit touches `network/`. The ticket-002 verifier
  saw the same shape (`TestDeliveryProperty`) earlier. Not proven to be a load flake, so the
  score stays above the threshold and the item is open: reproduce on an idle host before the
  0.11.0 release (`AGO_PROP_SEED=<seed> go test ./network -run TestRemoteRPCProperty`), and if it
  reproduces, treat it as a real hub defect next to D-124 instead of a flake.
  A full `go test -count=1 ./...` on `7832f29` (tickets 006 and 007 merged) at load average ~9.5
  gave 48 `ok`, `network` included. This supports the load-flake reading but does not prove it, so
  the item stays open until the pre-release run.

## Cleanup follow-ups

- `a2a/jws.go:40-45`: the `AGO-DEVIATION` comment still says AGo cards "always spell out
  capability booleans"; after the presence work (D-122) the signed payload follows
  `MessageToDict`, so the comment describes the previous revision. The code is right.
- `network/lifecycle.go`: the `OpenHub` doc comment became a single ~130-character line
  (found by the W4-N1 reviewer).
- `agent/overrides.go`: the field `turnPluginContributions.observers` is never read.
- `examples/policies_compact/main.go`: its header comment still says `SlidingWindowPolicy` has
  no transparent flag; `policies.WithWindowTransparency` exists since D-106.

- **network (D-124, risk 3):** a race between the ack pump and `markLive` may skip the channel creator's own live `ag2.channel.opened`. That delivery is not required by invariant 1.

- **CI (v0.10.0 release, risk 2×2):** one `go test ./...` run on a GitHub runner failed in the release
  workflow (darwin/arm64 build job, attempt 1 of run 43057561603) while five identical runs passed; the failing
  test is unknown (job logs need an admin token) and two local full runs at GOMAXPROCS=2 were clean. Capture
  the log on the next occurrence.

- **acp tool results join their parts (v1.3.2 sync, ticket 006, risk 1 × 3 = 3).** Python builds a tool result with one
  part per content item (`mappers._tool_call_result`, `ag2/acp/mappers.py:138-151`); `Update.UnmarshalJSON` concatenates
  the text of all items into `ContentText` and `providerSession.onUpdate` emits one `TextInput`. Two non-empty items
  `a`, `b` give one part `ab` instead of `["a","b"]`; an empty text still falls back to the status, as in Python.
  Audit §1c.2.
- **acp `plan_update` is not mapped (v1.3.2 sync, ticket 006, risk 1 × 2 = 2).** Python maps `AgentPlanContentUpdate`
  (since 1.3.0, `mappers.py:209`); AGo maps only `plan`, so the new kind reaches `OnUpdate` with no payload and gives no
  event. Candidate for its own ticket if plan parity is wanted. Audit §1d, last paragraph.
- **acp serve declares no image/embedded-context prompt support (v1.3.2 sync, ticket 006).** `image`, `audio` and
  `embeddedContext` stay `false` (Python: `image=true`, `embedded_context=true`); a conforming client does not send
  those blocks. A deliberate difference, D-131 (D-052, D-105); changing it is the audit's option (b) and needs a
  decision.
- **shell `ignore`: the interpreter caveat is not added to a custom tool description (v1.3.4 sync, ticket 010 / D-133, risk 1 × 3 = 3).** The sentence "the list does not stop a program that runs code (python -c, sh -c, env, xargs)" is appended only to the default description; a description given with `WithShellDescription` replaces it, and the caveat then lives in the `WithShellIgnore` / `WithShellBlockPatterns` doc comments only.
- **shell `ignore`: no test for a directory that becomes unreadable mid-walk (v1.3.4 sync, ticket 010 / D-133, risk 1 × 3 = 3).** The walk refuses (fail closed) when listing or resolving an entry fails, but only a directory that is unreadable from the start is tested (`TestShellIgnoreUnlistableDirectory`); the race itself has no test.
- **shell `ignore`: the walk resolves only symlink entries, so a file swapped between the check and the read is not seen (v1.3.4 sync, ticket 010 / D-133, risk 1 × 2 = 2).** Entries of an already-real directory are not re-resolved (Python resolves each); the only difference is a TOCTOU window (a regular file replaced by a symlink after the check and before `grep`/`diff` opens it), which also exists in Python between check and run.
- **`Context.Ask` returns an empty string for an answer that picks a choice (v1.3.4 sync, ticket 009 / D-132, risk 1 × 3 = 3).** `Ask` returns the answer's text; a pick
  (`HumanMessage.Choice`) is an option value, not words, so `Ask` hands back `""` and a caller cannot tell it from an empty
  answer. Deliberate: the approval middleware reads words ("y", "always") and must never take a pick for one. Only a hook that
  answers a request *without* `Choices` with a pick can hit it (AG-UI refuses such a resume with `INTERRUPT_PAYLOAD_REFUSED`, so
  this needs a custom hook); a caller that offers `Choices` reads the whole reply with `Context.AskMessage`.
