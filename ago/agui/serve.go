package agui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"

	"ago/agent"
	"ago/events"
	"ago/stream"
)

// versionRegex reads the two-component grammar a declared protocol version is
// written in (Python parity: interrupts.py _VERSION).
var versionRegex = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// refuseProtocolVersion returns the RUN_ERROR for a client declaring another
// protocol major, or nil when the run may start. An absent version is a client
// predating 1.0, served quietly; a newer minor, or an unreadable version, is
// served with a warning — the protocol forbids rejecting either (Python parity:
// interrupts.py refuse_protocol_version).
func refuseProtocolVersion(input RunAgentInput) *RunErrorEvent {
	declared := input.ProtocolVersion
	if declared == "" {
		return nil
	}
	ours, okOur := parseVersion(ProtocolVersion)
	theirs, okTheirs := parseVersion(declared)
	if !okOur || !okTheirs {
		slog.Warn("agui: serving a client whose declared protocol version cannot be read", "version", declared)
		return nil
	}
	if ours.major != theirs.major {
		// Majors differ: no run starts that this server cannot speak to.
		return &RunErrorEvent{
			baseEvent: baseEvent{Type: EventRunError, Timestamp: TimestampMs()},
			Message: fmt.Sprintf("this server speaks AG-UI %s, and cannot serve a client on %s",
				ProtocolVersion, declared),
			Code: CodeUnsupportedProtocolVersion,
		}
	}
	if theirs.major > ours.major || theirs.minor > ours.minor {
		slog.Warn("agui: serving a newer protocol minor; what it adds will not be used",
			"declared", declared, "server", ProtocolVersion)
	}
	return nil
}

// protoVersion is a read protocol version, compared field-wise.
type protoVersion struct{ major, minor int }

// parseVersion reads "M.N"; ok is false for anything else.
func parseVersion(v string) (protoVersion, bool) {
	m := versionRegex.FindStringSubmatch(v)
	if m == nil {
		return protoVersion{}, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return protoVersion{major, minor}, true
}

// serveTurn runs one POSTed turn against the agent, projecting its stream onto
// emit and ending the run on the wire however the turn ended (Python parity:
// run_stream + drive_run). It always leaves a terminal event behind: an error
// becomes RUN_ERROR, a server-side stop becomes RUN_FINISHED cancelled.
func serveTurn(ctx context.Context, a *agent.Agent, input RunAgentInput, emit func(Event)) error {
	if refused := refuseProtocolVersion(input); refused != nil {
		emit(*refused)
		return nil
	}

	mapped := mapRunInputMessages(input)

	// The turn runs on its own stream: the mapped history is its transcript and
	// the current turn's parts are its first model request.
	run, err := a.Run(ctx)
	if err != nil {
		emit(runErrorEvent(err))
		return nil
	}
	defer run.Close()
	s := run.Stream()
	if err := s.History().Replace(mapped.history); err != nil {
		emit(runErrorEvent(fmt.Errorf("seed history: %w", err)))
		return nil
	}

	proj := NewStream(emit)
	sub := s.Subscribe(func(ev events.Event) error { proj.Handle(ev); return nil }, stream.SubscribeOptions{})
	defer s.Unsubscribe(sub)

	if len(mapped.current) > 0 {
		if err := run.Enqueue(mapped.current...); err != nil {
			emit(runErrorEvent(fmt.Errorf("enqueue input: %w", err)))
			return nil
		}
	}

	// RUN_STARTED is the exchange's, not the turn's, and names this server's own
	// protocol version (Python parity: serve_exchange).
	emit(NewRunStartedEvent(input.ThreadID, input.RunID))

	if err := run.Start(ctx); err != nil {
		emit(runErrorEvent(err))
		return nil
	}
	_, err = run.Result(ctx)
	if err != nil {
		// A stop the server made — the exchange went away — closes the run as
		// cancelled; everything else is a failure (Python parity: drive_run's
		// CancelledError → output.stop()).
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			emit(RunFinishedEvent{
				baseEvent: baseEvent{Type: EventRunFinished, Timestamp: TimestampMs()},
				ThreadID:  input.ThreadID,
				RunID:     input.RunID,
				Outcome:   CancelledOutcome{},
				Usage:     proj.TakeUsage(),
			})
			return nil
		}
		emit(runErrorEvent(err))
		return nil
	}
	emit(RunFinishedEvent{
		baseEvent: baseEvent{Type: EventRunFinished, Timestamp: TimestampMs()},
		ThreadID:  input.ThreadID,
		RunID:     input.RunID,
		Outcome:   proj.SuccessOutcome(),
		Usage:     proj.TakeUsage(),
	})
	return nil
}

func runErrorEvent(err error) RunErrorEvent {
	message := err.Error()
	if message == "" {
		message = "agui: run failed"
	}
	return RunErrorEvent{
		baseEvent: baseEvent{Type: EventRunError, Timestamp: TimestampMs()},
		Message:   message,
	}
}

// mappedMessages is the AG-UI history translated into the parts a turn gets:
// the prior-turn events seeding the transcript, and the current user turn.
type mappedMessages struct {
	history []events.Event
	current []events.Input
}

// mapRunInputMessages translates AG-UI history into what serveTurn hands the
// agent (Python parity: map_agui_messages_to_events): system and developer
// messages and the application's shared context become the opening request,
// prior turns become transcript events, and the trailing run of user messages
// becomes the current turn.
//
// AGO-DEVIATION: Python hands the prompt strings to agent.ask separately, and
// AGo's served agent cannot take per-run prompt fragments from outside the
// agent package, so they travel as the opening transcript request instead —
// the model reads them ahead of the conversation, which is where they belong.
// Content-part metadata is dropped (ago inputs have no metadata field), and
// provider-tagged file handles are always skipped: which provider the served
// agent talks to is not readable here, so only untagged handles are accepted.
func mapRunInputMessages(input RunAgentInput) mappedMessages {
	var (
		out    mappedMessages
		folds  []events.Input
		buf    []events.Input
		names  = map[string]string{} // tool call id -> name, off the assistant messages
		shared = clientContextPrompt(input.Context)
	)
	if shared != "" {
		folds = append(folds, &events.TextInput{Content: shared})
	}

	flush := func() {
		if len(buf) > 0 {
			out.history = append(out.history, &events.ModelRequest{Parts: buf})
			buf = nil
		}
	}

	for _, m := range input.Messages {
		switch m.Role {
		case "user":
			parts, _ := mapContent(m.Content, "")
			buf = append(buf, parts...)

		case "system", "developer":
			text, _, err := decodeContent(m.Content)
			if err != nil {
				slog.Warn("agui: dropping a system message whose content does not decode", "err", err)
				continue
			}
			folds = append(folds, &events.TextInput{Content: text})

		case "assistant":
			flush()
			var calls []events.ToolCallEvent
			for _, tc := range m.ToolCalls {
				calls = append(calls, events.ToolCallEvent{
					ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
				})
				names[tc.ID] = tc.Function.Name
			}
			text, _, _ := decodeContent(m.Content)
			resp := events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: calls}}
			if text != "" {
				resp.Message = &events.ModelMessage{Content: text}
			}
			out.history = append(out.history, &resp)

		case "reasoning":
			flush()
			text, _, _ := decodeContent(m.Content)
			if text != "" {
				out.history = append(out.history, &events.ModelReasoning{Content: text})
			}

		case "tool":
			flush()
			parts, _ := mapContent(m.Content, "")
			// An error is what the model must hear, and leads; the call is owed
			// a result even when every part was skipped (Python parity).
			if m.Error != "" {
				parts = append([]events.Input{&events.TextInput{Content: m.Error}}, parts...)
			} else if len(parts) == 0 {
				parts = []events.Input{&events.TextInput{}}
			}
			out.history = append(out.history, &events.ToolResultsEvent{
				Results: []events.ToolResultEvent{{
					ParentID: m.ToolCallID,
					Name:     names[m.ToolCallID],
					Result:   events.ToolResult{Parts: parts},
				}},
			})

		case "activity":
			// Not part of the model's conversation.
		}
	}

	if len(folds) > 0 {
		out.history = append([]events.Event{&events.ModelRequest{Parts: folds}}, out.history...)
	}
	out.current = buf
	return out
}

// clientContextPrompt renders a run input's context entries as the prompt block
// the model reads them in (Python parity: stream.py client_context_prompt).
func clientContextPrompt(entries []ContextEntry) string {
	if len(entries) == 0 {
		return ""
	}
	lines := ""
	for _, e := range entries {
		lines += fmt.Sprintf("- %s: %s\n", e.Description, e.Value)
	}
	return "## Context from the application\n\n" + lines
}

// mapContent maps a message body — plain text or content parts — to the ago
// inputs it carries (Python parity: map_agui_parts_to_inputs). Unmappable parts
// are skipped with a warning; the run carries on without them.
func mapContent(raw json.RawMessage, provider string) ([]events.Input, bool) {
	text, parts, err := decodeContent(raw)
	if err != nil {
		slog.Warn("agui: dropping a message whose content does not decode", "err", err)
		return nil, false
	}
	if len(parts) == 0 {
		if text == "" {
			return nil, false
		}
		return []events.Input{&events.TextInput{Content: text}}, true
	}
	var out []events.Input
	for _, part := range parts {
		if inp, ok := mapPart(part, provider); ok {
			out = append(out, inp)
		}
	}
	return out, true
}

// mapPart maps one content part, or reports that it is to be skipped (Python
// parity: map_agui_content_to_input).
func mapPart(part ContentPart, provider string) (events.Input, bool) {
	switch part.Type {
	case PartText:
		return &events.TextInput{Content: part.Text}, true
	case PartImage, PartAudio, PartVideo, PartDocument:
	default:
		slog.Warn("agui: skipping a content part of unrecognised type", "type", part.Type)
		return nil, false
	}
	if part.Source == nil {
		slog.Warn("agui: skipping a media part with no source", "type", part.Type)
		return nil, false
	}
	src := part.Source
	switch src.Type {
	case SourceData:
		data, err := base64.StdEncoding.DecodeString(src.Value)
		if err != nil {
			slog.Warn("agui: skipping a data part whose bytes do not decode", "err", err)
			return nil, false
		}
		return &events.BinaryInput{Data: data, MediaType: src.MimeType, Kind: binaryKind(part.Type)}, true
	case SourceURL:
		return &events.UrlInput{URL: src.Value, Kind: binaryKind(part.Type)}, true
	case SourceFile:
		// A handle is opaque and only its provider can resolve it. This server
		// cannot read the served agent's provider, so a tagged handle is always
		// someone else's; only an untagged one passes. Never log the value.
		if src.Provider != "" {
			slog.Warn("agui: skipping a file part holding a handle tagged for another provider")
			return nil, false
		}
		return &events.FileIdInput{FileID: src.Value}, true
	default:
		slog.Warn("agui: skipping a part whose source is of unrecognised type", "source", src.Type)
		return nil, false
	}
}

// binaryKind maps a content part type to the ago binary kind it travels as.
func binaryKind(partType string) events.BinaryType {
	switch partType {
	case PartImage:
		return events.BinaryImage
	case PartAudio:
		return events.BinaryAudio
	case PartVideo:
		return events.BinaryVideo
	default:
		return events.BinaryDocument
	}
}
