package events

import "testing"

func TestStampInertFirstHopStampsAndLaterHopsPreserve(t *testing.T) {
	src := NewUsageEvent(Usage{TotalTokens: I64(5)}, UsageKindModelCall)
	stamped := StampInert(src, []string{"outer", "leaf"}, "leaf-task", "leaf")
	ue, ok := stamped.(*UsageEvent)
	if !ok {
		t.Fatalf("stamped = %T, want *UsageEvent", stamped)
	}
	if len(ue.Path) != 2 || ue.Path[0] != "outer" || ue.Path[1] != "leaf" {
		t.Fatalf("path = %v", ue.Path)
	}
	if ue.TaskID != "leaf-task" || ue.AgentName != "leaf" {
		t.Fatalf("identity = %q/%q", ue.TaskID, ue.AgentName)
	}
	if ue.EventID != src.EventID {
		t.Fatalf("source id changed: %q != %q", ue.EventID, src.EventID)
	}
	if len(src.Path) != 0 || src.TaskID != "" || src.AgentName != "" {
		t.Fatalf("StampInert mutated its input: %+v", src)
	}

	again := StampInert(stamped, []string{"other"}, "other-task", "other")
	ue2 := again.(*UsageEvent)
	if len(ue2.Path) != 2 || ue2.Path[1] != "leaf" {
		t.Fatalf("later hop rewrote path: %v", ue2.Path)
	}
	if ue2.TaskID != "leaf-task" || ue2.AgentName != "leaf" {
		t.Fatalf("later hop rewrote identity: %q/%q", ue2.TaskID, ue2.AgentName)
	}
}

func TestStampInertTaskKeepsItsOwnIdentity(t *testing.T) {
	task := &TaskStarted{TaskID: "child-task", AgentName: "child", Objective: "work"}
	stamped, ok := StampInert(task, []string{"root", "child-task"}, "ignored", "ignored").(*TaskStarted)
	if !ok {
		t.Fatalf("stamped = %T, want *TaskStarted", stamped)
	}
	if stamped.TaskID != "child-task" || stamped.AgentName != "child" {
		t.Fatalf("task identity overwritten: %+v", stamped)
	}
	if len(stamped.Path) != 2 || stamped.Path[1] != "child-task" {
		t.Fatalf("path = %v", stamped.Path)
	}
	if len(task.Path) != 0 {
		t.Fatalf("StampInert mutated its input: %v", task.Path)
	}
}

func TestStampInertLeavesOrdinaryEventsAlone(t *testing.T) {
	resp := &ModelResponse{Message: &ModelMessage{Content: "ok"}}
	if got := StampInert(resp, []string{"x"}, "t", "a"); got != resp {
		t.Fatalf("ordinary event was copied or changed: %T", got)
	}
}

func TestAsLocalDropsInheritedProvenance(t *testing.T) {
	src := NewUsageEvent(Usage{TotalTokens: I64(5)}, UsageKindModelCall)
	stamped := StampInert(src, []string{"outer", "leaf"}, "leaf-task", "leaf")
	local, ok := AsLocal(stamped).(*UsageEvent)
	if !ok {
		t.Fatalf("local = %T, want *UsageEvent", local)
	}
	if local.EventID == src.EventID {
		t.Fatal("a republished forwarded record must get a fresh source id")
	}
	if len(local.Path) != 0 || local.TaskID != "" || local.AgentName != "" {
		t.Fatalf("provenance survived AsLocal: %+v", local)
	}
	if len(InertPath(stamped)) == 0 {
		t.Fatal("AsLocal mutated the forwarded input")
	}

	// A locally constructed record keeps the id its producer assigned.
	fresh := NewUsageEvent(Usage{TotalTokens: I64(1)}, UsageKindModelCall)
	if got := AsLocal(fresh); got != fresh {
		t.Fatal("AsLocal copied a local record and changed its source id")
	}
}

func TestAsLocalTaskDropsPath(t *testing.T) {
	task := StampInert(&TaskFailed{TaskID: "t", AgentName: "a", Error: "boom"}, []string{"t"}, "", "").(*TaskFailed)
	local := AsLocal(task).(*TaskFailed)
	if len(local.Path) != 0 || local.TaskID != "t" || local.Error != "boom" {
		t.Fatalf("local task = %+v", local)
	}
}

func TestInertProvenanceWireRoundTrip(t *testing.T) {
	usage := NewUsageEvent(Usage{TotalTokens: I64(30)}, UsageKindModelCall)
	usage.Path = []string{"worker", "leaf"}
	usage.TaskID = "leaf-task"
	usage.AgentName = "leaf"

	ud, err := MarshalEvent(usage)
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}
	back, err := UnmarshalEvent(ud)
	if err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	got, ok := back.(*UsageEvent)
	if !ok {
		t.Fatalf("back = %T, want *UsageEvent", back)
	}
	if got.EventID != usage.EventID || got.TaskID != usage.TaskID || got.AgentName != usage.AgentName ||
		len(got.Path) != 2 || got.Path[1] != "leaf" {
		t.Fatalf("usage round trip = %+v", got)
	}

	task := &TaskCompleted{TaskID: "t", AgentName: "a", InertEvent: InertEvent{Path: []string{"root", "t"}}, Result: "done"}
	td, err := MarshalEvent(task)
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}
	tback, err := UnmarshalEvent(td)
	if err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	tg, ok := tback.(*TaskCompleted)
	if !ok {
		t.Fatalf("back = %T, want *TaskCompleted", tback)
	}
	if len(tg.Path) != 2 || tg.Path[0] != "root" || tg.Result != "done" {
		t.Fatalf("task round trip = %+v", tg)
	}
}

func TestInertIsNotConversational(t *testing.T) {
	evs := []Event{
		NewUsageEvent(Usage{TotalTokens: I64(1)}, UsageKindModelCall),
		&TaskStarted{TaskID: "t"},
		&TaskProgress{TaskID: "t"},
		&TaskCompleted{TaskID: "t"},
		&TaskFailed{TaskID: "t"},
		&TaskExpired{TaskID: "t"},
		&TaskCancelled{TaskID: "t"},
	}
	for _, ev := range evs {
		if !IsInert(ev) {
			t.Errorf("%s: not inert", ev.EventType())
		}
		if ev.Conversational() {
			t.Errorf("%s: inert event is conversational", ev.EventType())
		}
	}
	if IsInert(&ModelResponse{}) {
		t.Fatal("ModelResponse classified as inert")
	}
}

// TestInertValueFormsAndTypedNil pins the value/pointer spellings an archived
// transcript produces: value-form events keep working through the interfaces,
// and a typed-nil pointer is an absent event, not an inert one.
func TestInertValueFormsAndTypedNil(t *testing.T) {
	value := UsageEvent{
		InertEvent: InertEvent{Path: []string{"outer"}},
		Usage:      Usage{TotalTokens: I64(3)},
		Kind:       UsageKindModelCall,
	}
	if !IsInert(value) {
		t.Fatal("value-form UsageEvent is not inert")
	}
	if got := InertPath(value); len(got) != 1 || got[0] != "outer" {
		t.Fatalf("value-form path = %v", got)
	}
	stamped, ok := StampInert(value, []string{"root", "leaf"}, "task", "agent").(*UsageEvent)
	if !ok {
		t.Fatalf("stamped value form = %T, want *UsageEvent", stamped)
	}
	if len(stamped.Path) != 1 || stamped.Path[0] != "outer" {
		t.Fatalf("a later hop rewrote the value-form path: %v", stamped.Path)
	}

	var nilUsage *UsageEvent
	if IsInert(nilUsage) {
		t.Fatal("typed-nil UsageEvent classified as inert")
	}
	if got := InertPath(nilUsage); got != nil {
		t.Fatalf("typed-nil path = %v, want nil", got)
	}
	if got := StampInert(nilUsage, []string{"x"}, "t", "a"); got != nilUsage {
		t.Fatalf("StampInert(typed nil) = %#v, want the typed-nil input", got)
	}
}

// TestAsLocalDropsPartialIdentity pins that either half of a UsageEvent's
// delegation identity marks it as inherited: a republished record must not keep
// the descendant's task id or agent name.
func TestAsLocalDropsPartialIdentity(t *testing.T) {
	tests := []struct {
		name   string
		taskID string
		agent  string
	}{
		{name: "task id only", taskID: "leaf-task"},
		{name: "agent name only", agent: "leaf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inherited := NewUsageEvent(Usage{TotalTokens: I64(4)}, UsageKindModelCall)
			inherited.TaskID, inherited.AgentName = tt.taskID, tt.agent
			local, ok := AsLocal(inherited).(*UsageEvent)
			if !ok {
				t.Fatalf("local = %T, want *UsageEvent", local)
			}
			if local.EventID == inherited.EventID {
				t.Fatal("partial identity survived AsLocal: source id unchanged")
			}
			if local.TaskID != "" || local.AgentName != "" || len(local.Path) != 0 {
				t.Fatalf("partial identity survived AsLocal: %+v", local)
			}
			if inherited.TaskID != tt.taskID || inherited.AgentName != tt.agent {
				t.Fatal("AsLocal mutated the forwarded input")
			}
		})
	}
}

// TestStampInertValueFormTask pins the value-form Task spelling.
func TestStampInertValueFormTask(t *testing.T) {
	task := TaskCompleted{TaskID: "t", AgentName: "a", Result: "done"}
	stamped, ok := StampInert(task, []string{"root", "t"}, "ignored", "ignored").(*TaskCompleted)
	if !ok {
		t.Fatalf("stamped = %T, want *TaskCompleted", stamped)
	}
	if len(stamped.Path) != 2 || stamped.Path[1] != "t" {
		t.Fatalf("path = %v", stamped.Path)
	}
	if stamped.TaskID != "t" || stamped.AgentName != "a" || stamped.Result != "done" {
		t.Fatalf("task identity changed: %+v", stamped)
	}
	if len(task.Path) != 0 {
		t.Fatalf("StampInert mutated its value-form input: %v", task.Path)
	}
}

// TestStampInertUnwrapsRoutedDelivery pins that provenance is applied to the
// authored event, not to a transport wrapper that has nowhere to store it.
func TestStampInertUnwrapsRoutedDelivery(t *testing.T) {
	src := NewUsageEvent(Usage{TotalTokens: I64(7)}, UsageKindModelCall)
	routed := Routed(src, Delivery{Validate: func() error { return nil }})
	stamped, ok := StampInert(routed, []string{"t"}, "t", "child").(*UsageEvent)
	if !ok {
		t.Fatalf("stamped = %T, want *UsageEvent (wrapper dropped)", stamped)
	}
	if len(stamped.Path) != 1 || stamped.TaskID != "t" || stamped.AgentName != "child" {
		t.Fatalf("provenance = %+v", stamped)
	}
	if local, ok := AsLocal(routed).(*UsageEvent); !ok {
		t.Fatalf("local = %T, want *UsageEvent", local)
	} else if local.EventID == "" || local.TaskID != "" || len(local.Path) != 0 {
		t.Fatalf("local = %+v", local)
	}
}

func TestTaskTerminalObjectiveRoundTrip(t *testing.T) {
	for _, ev := range []Event{
		&TaskCancelled{TaskID: "t", AgentName: "a", Objective: "work", Reason: "stop"},
		&TaskExpired{TaskID: "t", AgentName: "a", Objective: "work"},
	} {
		raw, err := MarshalEvent(ev)
		if err != nil {
			t.Fatalf("marshal %s: %v", ev.EventType(), err)
		}
		back, err := UnmarshalEvent(raw)
		if err != nil {
			t.Fatalf("unmarshal %s: %v", ev.EventType(), err)
		}
		switch got := back.(type) {
		case *TaskCancelled:
			if got.Objective != "work" || got.Reason != "stop" {
				t.Fatalf("cancelled round trip = %+v", got)
			}
		case *TaskExpired:
			if got.Objective != "work" {
				t.Fatalf("expired round trip = %+v", got)
			}
		default:
			t.Fatalf("back = %T", back)
		}
	}
}
