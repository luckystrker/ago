package mcpserver

import (
	"context"

	"github.com/luckystrker/ago/events"
)

// progressLogger is the logger name log messages are tagged with (Python parity:
// _LOGGER_NAME).
const progressLogger = "ag2.mcp"

// progressForwarder turns a served agent's stream events into MCP notifications
// for as long as it is subscribed (Python parity: executor._ProgressForwarder):
// a model chunk is a notifications/progress carrying the call's progressToken
// (only when the client asked for progress on this call), and each tool call and
// result is a notifications/message at level info.
//
// AGO-DEVIATION: Python sends these on the connection's standalone channel (it
// never passes related_request_id), so a client that holds no GET stream sees
// none of them. AGo sends them on the request's own stream — where the spec
// places a notification belonging to a request, and where the client that asked
// for progress is certain to be listening.
type progressForwarder struct {
	ctx      context.Context
	ex       *exchange
	token    any
	progress float64
}

func newProgressForwarder(ctx context.Context, ex *exchange, env *callEnv) *progressForwarder {
	f := &progressForwarder{ctx: ctx, ex: ex}
	if env != nil {
		f.token = env.progressToken
	}
	return f
}

func (f *progressForwarder) handle(ev events.Event) error {
	if f.ex == nil || !f.ex.sse {
		return nil
	}
	switch v := events.AsValue(ev).(type) {
	case events.ModelMessageChunk:
		if f.token != nil {
			f.progress++
			f.ex.notify(f.ctx, "notifications/progress", map[string]any{
				"progressToken": f.token,
				"progress":      f.progress,
				"message":       v.Content,
			})
		}
	case events.ToolResultEvent:
		f.log("tool result: " + v.Name)
	case events.ToolCallEvent:
		f.log("tool call: " + v.Name)
	}
	return nil
}

func (f *progressForwarder) log(text string) {
	f.ex.notify(f.ctx, "notifications/message", map[string]any{
		"level": "info", "data": text, "logger": progressLogger,
	})
}
