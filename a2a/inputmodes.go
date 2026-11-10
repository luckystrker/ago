package a2a

import (
	"fmt"
	"sort"
)

// AG2InputModes are the media types an AG2 client sends (Python parity:
// extension.py AG2_INPUT_MODES): the vnd.ag2.* data-part types the
// client-tools extension and the history shipping ride on. A card that turns
// on ValidateInputModes must declare them, or the server would reject the very
// traffic its own clients send.
var AG2InputModes = []string{MIMEHistory, MIMEToolCall, MIMEToolResult, MIMEToolSchemas}

// declaredInputModes is the card's input-mode set: defaultInputModes plus every
// skill's inputModes.
func declaredInputModes(card AgentCard) map[string]bool {
	out := make(map[string]bool, len(card.DefaultInputModes))
	for _, m := range card.DefaultInputModes {
		out[m] = true
	}
	for _, skill := range card.Skills {
		for _, m := range skill.InputModes {
			out[m] = true
		}
	}
	return out
}

// InputModesError reports whether turning ValidateInputModes on would lock AG2
// clients out: it is the build-time half of the Python server's
// _check_input_modes, which raises when the card does not declare every
// AG2InputModes entry. A card that declares nothing accepts everything (the
// SDK's rule: an empty declaration is an absent one), so it is not an error.
//
// AGO-DEVIATION: Python raises this from build_jsonrpc / build_rest, which can
// fail; NewServer cannot return an error without breaking its signature, so
// the same check is a method a caller runs before serving (and the runtime half
// — rejecting undeclared parts — is on by default with ValidateInputModes).
func (c ServerConfig) InputModesError() error {
	if !c.ValidateInputModes {
		return nil
	}
	declared := declaredInputModes(c.Card)
	if len(declared) == 0 {
		return nil
	}
	var missing []string
	for _, m := range AG2InputModes {
		if !declared[m] {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("a2a: ValidateInputModes would reject AG2 clients: the card does not declare %v as input modes; build the card with the vnd.ag2 types in DefaultInputModes", missing)
}

// checkInputModes rejects a message part whose media type the card does not
// declare — the runtime half of the SDK's input-mode validation, which the
// Python server enables with validate_input_modes. A card that declares
// nothing accepts everything. nil when the message is acceptable.
func (s *Server) checkInputModes(msg *Message) *RPCError {
	if !s.cfg.ValidateInputModes || msg == nil {
		return nil
	}
	declared := declaredInputModes(s.card)
	if len(declared) == 0 {
		return nil
	}
	for _, part := range msg.Parts {
		mediaType := partMediaType(part)
		if mediaType == "" || declared[mediaType] {
			continue
		}
		return &RPCError{
			Code:    codeInvalidParams,
			Message: fmt.Sprintf("input mode %q is not declared by this agent card", mediaType),
		}
	}
	return nil
}

// partMediaType is the media type a part travels as: text/plain for a text
// part, the data part's mediaType (application/json when unset), the file's
// mimeType ("" when the part names none, which passes every check).
func partMediaType(part Part) string {
	switch p := part.(type) {
	case *TextPart:
		return "text/plain"
	case *DataPart:
		if p.MimeType != "" {
			return p.MimeType
		}
		return "application/json"
	case *FilePart:
		return p.File.MimeType
	}
	return ""
}
