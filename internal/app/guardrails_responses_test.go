package app

import (
	"strings"
	"testing"
)

func TestResponseToolCallsFromResponsesAPI(t *testing.T) {
	body := []byte(`{"output":[{"type":"function_call","call_id":"c1","name":"run_command","arguments":"{\"command\":\"rm -rf /\"}"}]}`)
	calls := responseToolCalls(body, false)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want the returned function_call item", calls)
	}
	if calls[0].ID != "c1" || calls[0].Function.Name != "run_command" {
		t.Fatalf("call = %+v, want id c1 and name run_command", calls[0])
	}
	if !strings.Contains(calls[0].Function.Arguments, "rm -rf") {
		t.Fatalf("arguments = %q, want the command", calls[0].Function.Arguments)
	}
}

func TestStreamedToolCallsFromResponsesAPI(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","name":"run_command","arguments":""}}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":"}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"rm -rf /\"}"}`,
		"",
		"",
	}, "\n")
	calls := responseToolCalls([]byte(stream), true)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one reassembled call", calls)
	}
	if calls[0].Function.Name != "run_command" || !strings.Contains(calls[0].Function.Arguments, "rm -rf") {
		t.Fatalf("call = %+v, want the name and the joined arguments", calls[0])
	}
}

func TestStreamedToolCallsFromDoneFrames(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","name":"run_command","arguments":"{\"command\":\"rm -rf /\"}"}}`,
		"",
		"",
	}, "\n")
	calls := responseToolCalls([]byte(stream), true)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want the call carried by the done frame", calls)
	}
	if !strings.Contains(calls[0].Function.Arguments, "rm -rf") {
		t.Fatalf("arguments = %q, want the finished command", calls[0].Function.Arguments)
	}
}

func TestStreamedToolCallsFromArgumentsDoneFrame(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","name":"run_command"}}`,
		"",
		`data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"command\":\"rm -rf /\"}"}`,
		"",
		"",
	}, "\n")
	calls := responseToolCalls([]byte(stream), true)
	if len(calls) != 1 || !strings.Contains(calls[0].Function.Arguments, "rm -rf") {
		t.Fatalf("calls = %+v, want the arguments from the done frame", calls)
	}
}
