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
