// Package agui serves an AGo agent over the AG-UI protocol: AG-UI core events
// as typed structs, one ago-turn projected into an AG-UI event stream, and an
// HTTP endpoint that answers GET with the agent's capabilities and POST with an
// SSE stream of one run.
//
// Parity with Python AG2 ag2/ag_ui/ (stream.py, asgi.py, run_input.py,
// capabilities.py) against ag-ui-protocol 1.0 wire shapes (camelCase fields,
// "type" discriminator, optional fields omitted when unset). The wire
// vocabulary here is a read of the installed SDK's generated models
// (ag_ui._generated.models, emitted from spec/1.0/schema.json), the same source
// the Python module builds on.
//
// Functional subset. The Python module holds a turn open across HTTP exchanges
// so an agent's question can be answered by a later POST (interrupts.py:
// ServedTurns, retention, resume proofs). That machinery is not ported — an
// agent that asks a human a question fails the run, which reaches the client as
// RUN_ERROR. See docs/DECISIONS.md D-053 for the full deferral list.
package agui
