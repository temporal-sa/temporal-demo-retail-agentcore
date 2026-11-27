package crew

import (
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// history builds the events of an order that a second Worker finished.
func history() []*historypb.HistoryEvent {
	at := timestamppb.New(time.Date(2026, 11, 26, 12, 14, 0, 0, time.UTC))
	id := int64(0)
	ev := func(t enumspb.EventType, set func(*historypb.HistoryEvent)) *historypb.HistoryEvent {
		id++
		e := &historypb.HistoryEvent{EventId: id, EventType: t, EventTime: at}
		set(e)
		return e
	}
	var out []*historypb.HistoryEvent
	activity := func(name, worker string) {
		s := ev(enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED, func(e *historypb.HistoryEvent) {
			e.Attributes = &historypb.HistoryEvent_ActivityTaskScheduledEventAttributes{ActivityTaskScheduledEventAttributes: &historypb.ActivityTaskScheduledEventAttributes{ActivityType: &commonpb.ActivityType{Name: name}}}
		})
		st := ev(enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED, func(e *historypb.HistoryEvent) {
			e.Attributes = &historypb.HistoryEvent_ActivityTaskStartedEventAttributes{ActivityTaskStartedEventAttributes: &historypb.ActivityTaskStartedEventAttributes{ScheduledEventId: s.EventId, Identity: worker, Attempt: 1}}
		})
		c := ev(enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED, func(e *historypb.HistoryEvent) {
			e.Attributes = &historypb.HistoryEvent_ActivityTaskCompletedEventAttributes{ActivityTaskCompletedEventAttributes: &historypb.ActivityTaskCompletedEventAttributes{ScheduledEventId: s.EventId}}
		})
		out = append(out, s, st, c)
	}
	activity("strands_get_model_config", "ziggy@a")
	activity("invoke_model", "ziggy@a")
	activity("get_recent_orders", "ziggy@a")
	activity("quote_cart", "ziggy@a")
	out = append(out, ev(enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED, func(e *historypb.HistoryEvent) {
		e.Attributes = &historypb.HistoryEvent_WorkflowExecutionSignaledEventAttributes{WorkflowExecutionSignaledEventAttributes: &historypb.WorkflowExecutionSignaledEventAttributes{SignalName: "pay"}}
	}))
	activity("charge_payment", "ziggy@b")
	activity("invoke_model", "ziggy@b")
	return out
}

func TestBuildProofCountsEachCallOnceAndNamesBothWorkers(t *testing.T) {
	p := buildProof(history())
	if p.ModelCalls != 2 || p.ToolCalls != 3 || p.Repeats != 0 {
		t.Fatalf("model=%d tool=%d repeats=%d", p.ModelCalls, p.ToolCalls, p.Repeats)
	}
	if len(p.Workers) != 2 || p.Workers[0] != "ziggy@a" || p.Workers[1] != "ziggy@b" {
		t.Fatalf("workers %v", p.Workers)
	}
	kinds := map[string]int{}
	for _, r := range p.Rows {
		kinds[r.Kind]++
	}
	if kinds["checkout"] != 1 || kinds["signal"] != 1 || kinds["worker"] != 2 {
		t.Fatalf("rows by kind %v", kinds)
	}
}

func TestLoadConfigListsEveryProblem(t *testing.T) {
	_, err := LoadConfig(func(string) string { return "" })
	if err == nil || len(err.(interface{ Unwrap() []error }).Unwrap()) != 2 {
		t.Fatalf("want two problems, got %v", err)
	}
}
