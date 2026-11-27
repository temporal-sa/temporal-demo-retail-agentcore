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
		var result *commonpb.Payloads
		if name == "invoke_model" {
			result = &commonpb.Payloads{Payloads: []*commonpb.Payload{{Data: []byte(`[{"messageStart":{"role":"assistant"}},{"metadata":{"usage":{"inputTokens":1200,"outputTokens":80,"totalTokens":1280}}}]`)}}}
		}
		c := ev(enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED, func(e *historypb.HistoryEvent) {
			e.Attributes = &historypb.HistoryEvent_ActivityTaskCompletedEventAttributes{ActivityTaskCompletedEventAttributes: &historypb.ActivityTaskCompletedEventAttributes{ScheduledEventId: s.EventId, Result: result}}
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
	if p.InputTokens != 2400 || p.OutputTokens != 160 {
		t.Fatalf("tokens in=%d out=%d", p.InputTokens, p.OutputTokens)
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
	if p.LastWorker != "ziggy@b" {
		t.Fatalf("last worker %q", p.LastWorker)
	}
}

func TestLoadConfigListsEveryProblem(t *testing.T) {
	_, err := LoadConfig(func(string) string { return "" })
	if err == nil || len(err.(interface{ Unwrap() []error }).Unwrap()) != 2 {
		t.Fatalf("want two problems, got %v", err)
	}
}

func TestSupportInputWaitsForAPaidOrder(t *testing.T) {
	if _, ok := supportInput("wid", map[string]any{"quote": map[string]any{}}, "Zora Vex"); ok {
		t.Fatal("an order with no payment can't open support")
	}
	memo := map[string]any{
		"quote": map[string]any{"cafeId": "titan-ring", "memberId": "zora-vex", "lines": []any{map[string]any{"itemId": "ceres-veggie-wrap", "name": "Ceres veggie wrap", "qty": 1.0, "unitPrice": 6.4, "linePrice": 6.4, "options": []any{}, "note": "Mo"}}},
		"order": map[string]any{"orderNumber": "OG-4242", "cafeName": "Titan Ring", "paymentId": "ch_1", "amount": 48.24},
	}
	in, ok := supportInput("wid", memo, "Zora Vex")
	order := in["order"].(map[string]any)
	if !ok || order["payment_id"] != "ch_1" || order["amount_paid"] != 48.24 || len(order["lines"].([]any)) != 1 {
		t.Fatalf("input %v", in)
	}
	if in["opener"] != "I need some help with my order OG-4242." {
		t.Fatalf("opener %q", in["opener"])
	}
	if in["greeting"] != "Hi Zora! I'm Ziggy, from support. Sorry it wasn't right. What went wrong?" {
		t.Fatalf("greeting %q", in["greeting"])
	}
}
