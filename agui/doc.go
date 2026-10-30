// Package agui serves an AGo agent over the AG-UI protocol: AG-UI core events
// as typed structs, one ago-turn projected into an AG-UI event stream, and an
// HTTP endpoint that answers GET with the agent's capabilities and POST with an
// SSE stream of one run.
//
// Parity with Python AG2 ag2/ag_ui/ (stream.py, asgi.py, run_input.py,
// capabilities.py, interrupts.py) against ag-ui-protocol 1.0 wire shapes
// (camelCase fields, "type" discriminator, optional fields omitted when unset).
// The wire vocabulary here is a read of the installed SDK's generated models
// (ag_ui._generated.models, emitted from spec/1.0/schema.json), the same source
// the Python module builds on.
//
// Held turns: a [Server] owns an agent's turns across HTTP exchanges. A run
// that asks a human a question — a HumanInputRequest or a tool approval —
// pauses on the wire with a RUN_FINISHED carrying an interrupt outcome, and the
// answer arrives as the resume entries of a later POST on the same thread;
// retention bounds how long the question is kept, and every interrupt is issued
// with a resume proof (Python parity: interrupts.py's ServedTurns, minimal and
// in memory — see docs/DECISIONS.md D-065 for the deferrals). Run input keys
// are read in both camelCase and snake_case spellings, and a server configured
// with WithStateSnapshots opens every run with a STATE_SNAPSHOT of the
// conversation's variables.
//
// Functional subset. Deferred: the subagent suspension ledger (a paused run
// does not close and re-announce its open invocations), seeding the turn's
// variables from the client's state, STATE_DELTAS, REASONING_ENCRYPTED_VALUE,
// and prompt-level input acceptance — see docs/DECISIONS.md D-053 and D-065.
package agui
