package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"time"

	agocontext "ago/context"
	"ago/events"
	"ago/hitl"
	"ago/middleware"
	"ago/stream"
)

var ctxPtrType = reflect.TypeOf((*agocontext.Context)(nil))

// FunctionTool wraps a Go function as a Tool (Python parity: ag2/tools/final/
// function_tool.py). The function's arguments are the fields of one struct parameter,
// bound from the model's JSON arguments by field name; fields may be DI slots
// (Inject/Variable types or tags) resolved from the conversation context. A
// *agocontext.Context parameter receives the conversation context stamped with the
// tool call id. A FunctionTool is immutable after construction and safe for
// concurrent use.
//
// AGO-DEVIATION: Python binds parameters by name through fast_depends; Go reflection
// exposes no parameter names, so binding and schema generation operate on the
// argument struct's fields (see docs/DECISIONS.md).
type FunctionTool struct {
	name        string
	description string
	schema      Schema
	timeout     time.Duration
	mw          []middleware.Middleware
	source      string
	fn          reflect.Value
	plan        *bindPlan
}

// FunctionToolOption configures NewFunctionTool.
type FunctionToolOption func(*functionToolOptions)

type functionToolOptions struct {
	paramsOverride map[string]any
	timeout        time.Duration
	mw             []middleware.Middleware
	source         string
}

// WithToolSchema replaces the generated parameters schema with an explicit JSON
// schema object (Python parity: the `schema=` kwarg).
func WithToolSchema(params map[string]any) FunctionToolOption {
	return func(o *functionToolOptions) { o.paramsOverride = params }
}

// WithTimeout bounds one invocation (the function and its dependency resolution,
// inside the middleware chain). Expiry answers the call with a ToolErrorEvent
// "Tool 'X' timed out after N seconds"; the running function cannot be interrupted.
func WithTimeout(d time.Duration) FunctionToolOption {
	return func(o *functionToolOptions) { o.timeout = d }
}

// WithMiddleware adds tool-scoped middleware. The first entry is the outermost layer
// of the tool's own chain; agent middleware wraps it from outside.
func WithMiddleware(mw ...middleware.Middleware) FunctionToolOption {
	return func(o *functionToolOptions) { o.mw = append(o.mw, mw...) }
}

// WithSource overrides the implementation identity stamped onto handled calls
// (Python parity: the `source` property).
func WithSource(s string) FunctionToolOption {
	return func(o *functionToolOptions) { o.source = s }
}

// NewFunctionTool builds a function tool.
//
// The fn signature must be one of:
//
//	func(args Args) T                          func(args Args) (T, error)
//	func(args Args) error                      func() (T, error)
//	func(args Args, ctx *agocontext.Context) … // *agocontext.Context anywhere
//
// Args is a single struct whose fields bind from the model's JSON arguments by field
// name (json tag or field name). Fields of type Inject/Variable or carrying
// `aginject`/`agvariable` tags are dependency-injected from the conversation context
// and excluded from the schema. The return value maps to the tool result: ToolResult
// passes through, []events.Input becomes the result parts, a string becomes a
// TextInput, everything else a DataInput. A returned (non-hitl) error becomes a
// ToolErrorEvent.
func NewFunctionTool(name, description string, fn any, opts ...FunctionToolOption) (*FunctionTool, error) {
	if name == "" {
		return nil, errors.New("tools: function tool name must not be empty")
	}
	fv := reflect.ValueOf(fn)
	if !fv.IsValid() || fv.Kind() != reflect.Func {
		return nil, fmt.Errorf("tools: tool %q: fn must be a func", name)
	}
	ft := fv.Type()
	if ft.IsVariadic() {
		return nil, fmt.Errorf("tools: tool %q: variadic functions are not supported", name)
	}
	if err := checkReturnTypes(name, ft); err != nil {
		return nil, err
	}
	plan, params, err := buildBindPlan(name, ft)
	if err != nil {
		return nil, err
	}

	t := &FunctionTool{
		name:        name,
		description: description,
		fn:          fv,
		plan:        plan,
	}
	var o functionToolOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.timeout < 0 {
		return nil, fmt.Errorf("tools: tool %q: timeout must not be negative", name)
	}
	t.timeout = o.timeout
	t.mw = o.mw
	t.source = o.source
	if t.source == "" {
		t.source = defaultSource(name, fv)
	}
	if o.paramsOverride != nil {
		params = deepCopySchema(o.paramsOverride)
	}
	stripTitle(params)
	t.schema = Schema{Name: name, Description: description, Parameters: params}
	return t, nil
}

func checkReturnTypes(name string, ft reflect.Type) error {
	switch ft.NumOut() {
	case 1:
		if ft.Out(0) == errorType {
			return nil // error-only function
		}
		return nil
	case 2:
		if ft.Out(1) != errorType {
			return fmt.Errorf("tools: tool %q: second return value must be error, got %s", name, ft.Out(1))
		}
		return nil
	default:
		return fmt.Errorf("tools: tool %q: fn must return (T, error), T or error; got %d return values", name, ft.NumOut())
	}
}

func defaultSource(name string, fv reflect.Value) string {
	pc := fv.Pointer()
	rn := "unknown"
	if f := runtime.FuncForPC(pc); f != nil {
		rn = f.Name()
	}
	return fmt.Sprintf("function:%s:%s", rn, name)
}

// --- binding plan -------------------------------------------------------------

type paramSpec struct {
	isCtx    bool
	isStruct bool
}

type diFieldBinding struct {
	index int // field index within the argument struct
	slot  diSlot
}

// bindPlan is the precomputed reflection layout of a tool function.
type bindPlan struct {
	specs      []paramSpec
	structType reflect.Type // deref'd argument struct type; nil when absent
	structIsP  bool
	diFields   []diFieldBinding
}

func buildBindPlan(name string, ft reflect.Type) (*bindPlan, map[string]any, error) {
	plan := &bindPlan{specs: make([]paramSpec, ft.NumIn())}
	structIdx := -1
	for i := 0; i < ft.NumIn(); i++ {
		pt := ft.In(i)
		if pt == ctxPtrType {
			plan.specs[i] = paramSpec{isCtx: true}
			continue
		}
		if isDISlotType(pt) {
			// AGO-DEVIATION: Go reflection exposes no parameter names, so a
			// standalone Inject/Variable parameter has no lookup key; DI slots
			// live on the argument struct's fields (tag or field type).
			return nil, nil, fmt.Errorf(
				"tools: tool %q: an Inject/Variable parameter has no name to resolve; "+
					"use a field of the argument struct with an aginject/agvariable tag "+
					"or of type tools.Inject/tools.Variable", name)
		}
		dt := derefType(pt)
		if dt.Kind() != reflect.Struct {
			return nil, nil, fmt.Errorf(
				"tools: tool %q: tool arguments bind from JSON by field name; "+
					"use a single struct parameter, got %s at position %d", name, quoteGoName(pt), i)
		}
		if structIdx >= 0 {
			return nil, nil, fmt.Errorf("tools: tool %q: at most one struct argument is supported", name)
		}
		structIdx = i
		plan.specs[i] = paramSpec{isStruct: true}
		plan.structType = dt
		plan.structIsP = pt.Kind() == reflect.Pointer
	}
	params := emptyObjectSchema()
	if structIdx >= 0 {
		for i := 0; i < plan.structType.NumField(); i++ {
			f := plan.structType.Field(i)
			if f.PkgPath != "" {
				continue
			}
			slot, isDI, err := diSlotForField(f)
			if err != nil {
				return nil, nil, fmt.Errorf("tools: tool %q: argument field %s: %w", name, f.Name, err)
			}
			if isDI {
				plan.diFields = append(plan.diFields, diFieldBinding{index: i, slot: slot})
			}
		}
		sch, err := argStructSchema(plan.structType)
		if err != nil {
			return nil, nil, fmt.Errorf("tools: tool %q: %w", name, err)
		}
		params = sch
	}
	return plan, params, nil
}

// bind assembles the call arguments: JSON arguments into the struct, DI fields from
// the context (DI values win over model-supplied ones), the context parameter last.
func (p *bindPlan) bind(call events.ToolCallEvent, cx *agocontext.Context, toolName string) ([]reflect.Value, error) {
	args := make([]reflect.Value, len(p.specs))
	for i, spec := range p.specs {
		switch {
		case spec.isCtx:
			args[i] = reflect.ValueOf(cx)
		case spec.isStruct:
			sv := reflect.New(p.structType)
			if raw := normalizeJSONArgs(call.Arguments); raw != "{}" {
				if err := json.Unmarshal([]byte(raw), sv.Interface()); err != nil {
					return nil, fmt.Errorf("tools: tool %q: invalid arguments: %w", toolName, err)
				}
			}
			for _, d := range p.diFields {
				v, ok := resolveDILookup(cx, d.slot)
				if !ok {
					return nil, fmt.Errorf("%w: tool %q: %s %q is not provided",
						ErrNotFound, toolName, d.slot.kind, d.slot.key)
				}
				if err := setDIField(sv.Elem().Field(d.index), d.slot, v); err != nil {
					return nil, fmt.Errorf("tools: tool %q: %w", toolName, err)
				}
			}
			if p.structIsP {
				args[i] = sv
			} else {
				args[i] = sv.Elem()
			}
		default:
			return nil, fmt.Errorf("tools: tool %q: unsupported parameter at position %d", toolName, i)
		}
	}
	return args, nil
}

// setDIField writes one resolved DI value into its struct field. Typed slots receive
// the annotation value carrying the payload in Default; plain fields receive the
// dependency value itself (assignable, or converted when safe).
func setDIField(fv reflect.Value, slot diSlot, v any) error {
	if slot.typed {
		ann := reflect.New(fv.Type()).Elem()
		nameField := ann.FieldByName("ID")
		if !nameField.IsValid() {
			nameField = ann.FieldByName("Name")
		}
		nameField.SetString(slot.key)
		ann.FieldByName("HasDefault").SetBool(true)
		def := ann.FieldByName("Default")
		if v != nil {
			rv := reflect.ValueOf(v)
			if !rv.Type().AssignableTo(def.Type()) {
				return fmt.Errorf("%s %q: value of type %s is not storable in %s", slot.kind, slot.key, rv.Type(), def.Type())
			}
			def.Set(rv)
		}
		fv.Set(ann)
		return nil
	}
	if v == nil {
		switch fv.Kind() {
		case reflect.Interface, reflect.Pointer, reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
			fv.Set(reflect.Zero(fv.Type()))
			return nil
		default:
			return fmt.Errorf("%s %q: nil value is not assignable to %s", slot.kind, slot.key, fv.Type())
		}
	}
	rv := reflect.ValueOf(v)
	switch {
	case rv.Type().AssignableTo(fv.Type()):
		fv.Set(rv)
	case rv.Type().ConvertibleTo(fv.Type()):
		fv.Set(rv.Convert(fv.Type()))
	default:
		return fmt.Errorf("%s %q: value of type %s is not assignable to %s", slot.kind, slot.key, rv.Type(), fv.Type())
	}
	return nil
}

// --- result mapping -----------------------------------------------------------

// valueToResult maps a function's return value to a ToolResult (Python parity:
// ToolResult.ensure_result with Input.ensure_input parts).
func valueToResult(v any) events.ToolResult {
	switch tv := v.(type) {
	case events.ToolResult:
		return tv
	case []events.Input:
		return events.ToolResult{Parts: tv}
	default:
		return events.ToolResult{Parts: []events.Input{events.EnsureInput(v)}}
	}
}

// --- Tool interface -----------------------------------------------------------

func (t *FunctionTool) Name() string         { return t.name }
func (t *FunctionTool) DeclaredInCode() bool { return true }

// Source returns the implementation identity stamped onto handled calls.
func (t *FunctionTool) Source() string { return t.source }

func (t *FunctionTool) Schemas(_ *agocontext.Context) ([]any, error) {
	return []any{t.schema}, nil
}

// Register subscribes the tool on the conversation stream for calls matching its
// name. The execution chain (agent middleware outermost, tool middleware innermost,
// timeout and the function at the core) runs inside the subscriber; its result event
// is sent back on the stream for the executor to collect. The subscription id is
// returned for the turn scope's teardown.
func (t *FunctionTool) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	exec := t.buildChain(mw)
	cond := events.Equal(func(ev events.Event) string {
		if call, ok := events.AsValue(ev).(events.ToolCallEvent); ok {
			return call.Name
		}
		return ""
	}, t.name)
	handler := func(ev events.Event) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("tool %s middleware panicked: %v", t.name, r)
			}
		}()
		call, ok := events.AsValue(ev).(events.ToolCallEvent)
		if !ok {
			return nil
		}
		// Python parity: the chain sees the call stamped with the implementation
		// source and the context carries the tool call id.
		stamped := call.HandledBy(t.source)
		result, err := exec(&stamped, cx.WithToolCallID(call.ID))
		if err != nil {
			// A failed human-input hand-off is not a tool result; it travels
			// back to the executor through the send error.
			return err
		}
		return cx.Send(result)
	}
	id := cx.Stream.Subscribe(handler, stream.SubscribeOptions{Condition: cond})
	return []stream.SubID{id}, nil
}

// buildChain wires the execution onion: agent middleware (mws[0] outermost — the
// repo-wide Chain convention) wraps the tool's own middleware (first entry
// outermost), which wraps the timeout and the function call.
func (t *FunctionTool) buildChain(agentMW []middleware.Middleware) middleware.ToolExecFunc {
	exec := t.execCall
	for i := len(t.mw) - 1; i >= 0; i-- {
		mw, inner := t.mw[i], exec
		exec = func(ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
			return mw.OnToolExecution(inner, ev, cx)
		}
	}
	for i := len(agentMW) - 1; i >= 0; i-- {
		mw, inner := agentMW[i], exec
		exec = func(ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
			return mw.OnToolExecution(inner, ev, cx)
		}
	}
	return exec
}

// execCall is the innermost chain node: the timeout around the function invocation.
func (t *FunctionTool) execCall(ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	if t.timeout <= 0 {
		return t.invoke(ev, cx)
	}
	type outcome struct {
		ev  events.Event
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		// The running function cannot be interrupted (Python parity: a sync tool
		// on a worker thread); an outcome arriving after expiry is discarded.
		ev, err := t.invoke(ev, cx)
		ch <- outcome{ev, err}
	}()
	select {
	case o := <-ch:
		return o.ev, o.err
	case <-time.After(t.timeout):
		return events.FromErrorCall(*ev, fmt.Errorf("Tool '%s' timed out after %g seconds", t.name, t.timeout.Seconds())), nil
	}
}

// invoke runs the function and maps the outcome to result events. Function-level
// failures become ToolErrorEvents; hitl failures propagate (Python parity: a failed
// HITL is not a tool result).
func (t *FunctionTool) invoke(ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	result, err := t.callFn(ev, cx)
	if err != nil {
		if isHitlError(err) {
			return nil, err
		}
		return events.FromErrorCall(*ev, err), nil
	}
	return events.FromResultCall(*ev, result), nil
}

func (t *FunctionTool) callFn(ev *events.ToolCallEvent, cx *agocontext.Context) (result events.ToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = events.ToolResult{}, fmt.Errorf("tool %s panicked: %v", t.name, r)
		}
	}()
	args, err := t.plan.bind(*ev, cx, t.name)
	if err != nil {
		return events.ToolResult{}, err
	}
	outs := t.fn.Call(args)
	switch len(outs) {
	case 1:
		if outs[0].Type() == errorType {
			e, _ := outs[0].Interface().(error)
			if e != nil {
				return events.ToolResult{}, e
			}
			return events.ToolResult{}, nil
		}
		return valueToResult(outs[0].Interface()), nil
	case 2:
		if e, _ := outs[1].Interface().(error); e != nil {
			return events.ToolResult{}, e
		}
		return valueToResult(outs[0].Interface()), nil
	default:
		return events.ToolResult{}, nil
	}
}

// withPrependedMiddleware returns a copy of the tool whose middleware list starts
// with mws (Python parity: with_middleware — a toolkit attaches its middleware this
// way without mutating the original).
func (t *FunctionTool) withPrependedMiddleware(mws []middleware.Middleware) *FunctionTool {
	if len(mws) == 0 {
		return t
	}
	cp := *t
	cp.mw = append(append([]middleware.Middleware{}, mws...), t.mw...)
	return &cp
}

// isHitlError reports whether err is a human-in-the-loop failure (Python parity:
// HumanInputError and its subclasses). Such errors never become tool results.
func isHitlError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, hitl.ErrNotProvided) {
		return true
	}
	var notProvided *hitl.NotProvidedError
	var failed *hitl.FailedError
	var timeout *hitl.TimeoutError
	return errors.As(err, &notProvided) || errors.As(err, &failed) || errors.As(err, &timeout)
}
