// Package eval grades agent runs: build a Trace from a conversation's event
// history, score it with composable scorers, and compare runs.
//
// The flow is produce-then-grade (Python parity: ag2.eval run_agent /
// evaluate_traces): every Suite Case asks the agent, the reply's history
// becomes a Trace, the case's scorers grade it, and the run tallies into a
// RunResult that saves/loads as JSON.
//
// AGO-DEVIATION: Python reconstructs the Trace from OpenTelemetry spans
// (TelemetryMiddleware → in-memory exporter → span→Trace). AGo builds the
// Trace directly from the stream's event log — the same event-sourced
// substrate the framework already persists — with no OTEL dependency.
// Deferred with it: OTEL/Tempo/OTLP trace sources, run repeats/variants/
// per-task model configs/budget thresholds and eval lifecycle events
// (docs/DECISIONS.md D-051). Since D-061 the package also carries the failure
// attribution scorer, the scorer registry (the @scorer DI stand-in), Suite
// JSONL loading, RunDiff regression diffing, Wilson/kappa stats and parallel
// case execution.
package eval
