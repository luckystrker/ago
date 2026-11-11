# Known issues

Findings that were reviewed, scored and **deliberately left open**: risk = frequency (1–5) ×
harm (1–10) below 5, so they are catalogued here instead of being fixed on the spot. Risk ≥ 5
is fixed; harm 10 (money, data) goes to the owner. Each entry names where it came from; the
decision records are in [DECISIONS.md](DECISIONS.md), the per-package status in
[PORT_STATUS.md](PORT_STATUS.md).

Entries below come from the gap-audit wave 3 review (2026-11-10) unless stated otherwise.

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

- `tools/TestExecutorHitlFailureAbandonsBatchAndPropagates` ("call events sent = 1, want 2"
  under full-run load): fixed in W1-E3, commit `0d0b11a` (D-100).
- `acp/TestServeAgentStreamsAndContinues` (`chunks=[Hey] want [He y]` under load): fixed in
  W2-ACP, commit `476ade8` (D-105).

## Test coverage gaps in new code

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

## Behavior notes

- **`network` observation de-duplication is memory-only (W4-N1, risk 4).** `Hub.RecordObservation`
  remembers the `(owner, task)` pairs it already counted in memory, so after a hub restart a
  repeated terminal event of the same task counts a second time in the owner's observed track
  record. Python does the same (`_observed_task_ids` is memory-only), so it is left as is.

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

## Cleanup follow-ups

- `agent/overrides.go`: the field `turnPluginContributions.observers` is never read.
- `examples/policies_compact/main.go`: its header comment still says `SlidingWindowPolicy` has
  no transparent flag; `policies.WithWindowTransparency` exists since D-106.
