package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// recoverProviderBlock is the provider block prepareRequest injects, in the exact
// bytes it encodes it to. A continuation has to carry it through unchanged or it
// would be routed to a different provider than the request it continues.
const recoverProviderBlock = `"provider":{"allow_fallbacks":false,"only":["deepinfra"]}`

func recoverChatBody() string {
	return `{"messages":[{"content":"write a story","role":"user"}],` + recoverProviderBlock +
		`,"model":"deepseek/deepseek-chat","stream":true,"usage":{"include":true}}` + "\n"
}

func recoverResponsesBody(input string) string {
	return `{"include":["usage"],"input":` + input + `,` + recoverProviderBlock +
		`,"model":"deepseek/deepseek-chat","stream":true}` + "\n"
}

func recoverMessagesBody() string {
	return `{"max_tokens":1024,"messages":[{"content":"write a story","role":"user"}],` + recoverProviderBlock +
		`,"model":"anthropic/claude-sonnet-4","stream":true}` + "\n"
}

// recoverTurns is the two turns a continuation appends for a non-empty prefix.
func recoverTurns(t *testing.T, list []any) (assistant, user map[string]any) {
	t.Helper()
	if len(list) < 2 {
		t.Fatalf("turns = %d, want at least the intervention", len(list))
	}
	assistant, _ = list[len(list)-2].(map[string]any)
	user, _ = list[len(list)-1].(map[string]any)
	return assistant, user
}

func recoverDecode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("continuation body is not a JSON object: %v (%s)", err, body)
	}
	return payload
}

func TestLoopContinuationChatRequest(t *testing.T) {
	verdict := loopVerdict{Channel: loopChannelContent, Period: 64, Repeat: 8, Suffix: 512, Prefix: "Once upon a time "}
	out, err := loopContinuationBody("/v1/chat/completions", []byte(recoverChatBody()), verdict, 0.4)
	if err != nil {
		t.Fatalf("loopContinuationBody: %v", err)
	}
	if !strings.Contains(string(out), recoverProviderBlock) {
		t.Fatalf("provider block not preserved byte for byte:\n%s", out)
	}
	payload := recoverDecode(t, out)
	if payload["model"] != "deepseek/deepseek-chat" {
		t.Fatalf("model = %v, want the continued model", payload["model"])
	}
	if payload["stream"] != true {
		t.Fatalf("stream = %v, want true", payload["stream"])
	}
	if penalty, ok := payload["frequency_penalty"].(float64); !ok || penalty != 0.4 {
		t.Fatalf("frequency_penalty = %v, want 0.4", payload["frequency_penalty"])
	}
	if penalty, ok := payload["presence_penalty"].(float64); !ok || penalty != 0.2 {
		t.Fatalf("presence_penalty = %v, want half the configured strength", payload["presence_penalty"])
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want the original turn plus two", len(messages))
	}
	assistant, user := recoverTurns(t, messages)
	if assistant["role"] != "assistant" || assistant["content"] != verdict.Prefix {
		t.Fatalf("prefix turn = %v, want the clean prefix as the assistant turn", assistant)
	}
	if user["role"] != "user" || user["content"] != loopIntervention {
		t.Fatalf("intervention turn = %v, want the intervention", user)
	}
	// The repetition is what the continuation exists to escape, so the prefix must
	// appear once, as the assistant turn, and nowhere else.
	if count := strings.Count(string(out), verdict.Prefix); count != 1 {
		t.Fatalf("prefix appears %d times in the continuation body, want once:\n%s", count, out)
	}
}

func TestLoopContinuationResponsesRequest(t *testing.T) {
	verdict := loopVerdict{Channel: loopChannelContent, Period: 64, Repeat: 8, Suffix: 512, Prefix: "Once upon a time "}
	out, err := loopContinuationBody("/v1/responses",
		[]byte(recoverResponsesBody(`[{"content":"write a story","role":"user"}]`)), verdict, 0.4)
	if err != nil {
		t.Fatalf("loopContinuationBody: %v", err)
	}
	if !strings.Contains(string(out), recoverProviderBlock) {
		t.Fatalf("provider block not preserved byte for byte:\n%s", out)
	}
	for _, unsupported := range []string{"frequency_penalty", "presence_penalty"} {
		if strings.Contains(string(out), unsupported) {
			t.Fatalf("%s sent to an API that has no such parameter:\n%s", unsupported, out)
		}
	}
	payload := recoverDecode(t, out)
	if payload["stream"] != true {
		t.Fatalf("stream = %v, want true", payload["stream"])
	}
	input, _ := payload["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %d items, want the original turn plus two", len(input))
	}
	assistant, user := recoverTurns(t, input)
	if assistant["role"] != "assistant" || assistant["content"] != verdict.Prefix {
		t.Fatalf("prefix item = %v, want the clean prefix as the assistant item", assistant)
	}
	if user["role"] != "user" || user["content"] != loopIntervention {
		t.Fatalf("intervention item = %v, want the intervention", user)
	}
}

func TestLoopContinuationResponsesStringInput(t *testing.T) {
	verdict := loopVerdict{Channel: loopChannelContent, Period: 64, Repeat: 8, Suffix: 512, Prefix: "Once upon a time "}
	out, err := loopContinuationBody("/v1/responses",
		[]byte(recoverResponsesBody(`"write a story"`)), verdict, 0.4)
	if err != nil {
		t.Fatalf("loopContinuationBody: %v", err)
	}
	payload := recoverDecode(t, out)
	input, _ := payload["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %d items, want the string turned into a turn plus two", len(input))
	}
	original, _ := input[0].(map[string]any)
	if original["role"] != "user" || original["content"] != "write a story" {
		t.Fatalf("first item = %v, want the original prompt as a user turn", original)
	}
	assistant, user := recoverTurns(t, input)
	if assistant["content"] != verdict.Prefix || user["content"] != loopIntervention {
		t.Fatalf("appended items = %v / %v", assistant, user)
	}
}

func TestLoopContinuationMessagesRequest(t *testing.T) {
	verdict := loopVerdict{Channel: loopChannelContent, Period: 64, Repeat: 8, Suffix: 512, Prefix: "Once upon a time "}
	out, err := loopContinuationBody("/v1/messages", []byte(recoverMessagesBody()), verdict, 0.4)
	if err != nil {
		t.Fatalf("loopContinuationBody: %v", err)
	}
	if !strings.Contains(string(out), recoverProviderBlock) {
		t.Fatalf("provider block not preserved byte for byte:\n%s", out)
	}
	for _, unsupported := range []string{"frequency_penalty", "presence_penalty"} {
		if strings.Contains(string(out), unsupported) {
			t.Fatalf("%s sent to an API that has no such parameter:\n%s", unsupported, out)
		}
	}
	payload := recoverDecode(t, out)
	if payload["stream"] != true {
		t.Fatalf("stream = %v, want true", payload["stream"])
	}
	// The continuation is a second request inside the client's one response, so the
	// budget it asks for is what the client budgeted minus what this answer already
	// generated: 1024 less the 17-byte prefix, estimated at 5 tokens.
	if payload["max_tokens"] != float64(1019) {
		t.Fatalf("max_tokens = %v, want the client's 1024 less what was generated", payload["max_tokens"])
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want the original turn plus two", len(messages))
	}
	assistant, user := recoverTurns(t, messages)
	if assistant["role"] != "assistant" || assistant["content"] != verdict.Prefix {
		t.Fatalf("prefix turn = %v, want the clean prefix as the assistant turn", assistant)
	}
	if user["role"] != "user" || user["content"] != loopIntervention {
		t.Fatalf("intervention turn = %v, want the intervention", user)
	}
}

func TestLoopContinuationRejectsEmptyPrefix(t *testing.T) {
	// A content loop with no prefix before the repetition has nothing to resume —
	// choices[] with n > 1 shares one channel and reports none — so the response is
	// aborted rather than continued from an empty assistant turn.
	verdict := loopVerdict{Channel: loopChannelContent, Period: 64, Repeat: 8, Suffix: 512}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		body := []byte(recoverChatBody())
		out, err := loopContinuationBody(path, body, verdict, 0.4)
		if !errors.Is(err, errLoopEmptyPrefix) {
			t.Fatalf("%s: err = %v, want errLoopEmptyPrefix", path, err)
		}
		if out != nil {
			t.Fatalf("%s: built a body anyway:\n%s", path, out)
		}
	}
}

func TestLoopContinuationRejectsChannelsWithoutText(t *testing.T) {
	// A thinking loop is no longer one of them: it is restarted while the response has
	// no answer yet, and rejected only once it does — see
	// TestLoopContinuationRejectsAThinkingLoopAfterTheAnswerStarted.
	for _, channel := range []loopChannel{loopChannelToolArgs, loopChannelNone} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			body := []byte(recoverChatBody())
			out, err := loopContinuationBody(path, body, loopVerdict{Channel: channel, Prefix: "partial"}, 0.4)
			if !errors.Is(err, errLoopNotContinuable) {
				t.Fatalf("%s on %s: err = %v, want errLoopNotContinuable", channel, path, err)
			}
			if out != nil {
				t.Fatalf("%s on %s: built a body anyway:\n%s", channel, path, out)
			}
		}
	}
}

func TestLoopContinuationRejectsUnknownPath(t *testing.T) {
	_, err := loopContinuationBody("/v1/embeddings", []byte(recoverChatBody()),
		loopVerdict{Channel: loopChannelContent, Prefix: "Once"}, 0.4)
	if err == nil || errors.Is(err, errLoopNotContinuable) {
		t.Fatalf("err = %v, want a path error", err)
	}
}

func TestLoopContinuationRejectsBodyWithoutTurns(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		if _, err := loopContinuationBody(path, []byte(`{"model":"m","stream":true}`),
			loopVerdict{Channel: loopChannelContent, Prefix: "Once"}, 0.4); err == nil {
			t.Fatalf("%s: built a continuation from a body with no messages", path)
		}
	}
	if _, err := loopContinuationBody("/v1/responses", []byte(`{"model":"m","stream":true}`),
		loopVerdict{Channel: loopChannelContent, Prefix: "Once"}, 0.4); err == nil {
		t.Fatal("/v1/responses: built a continuation from a body with no input")
	}
}

// recoverChatDelta is one chat-completions content delta frame.
func recoverChatDelta(text string) string {
	return `data: {"choices":[{"delta":{"content":` + loopQuoted(text) + `}}]}` + "\n\n"
}

// recoverFeedAll feeds every frame to the reframer and returns what the client saw.
func recoverFeedAll(reframer *continuationReframer, frames []string) string {
	var seen strings.Builder
	for _, frame := range frames {
		seen.Write(reframer.Feed([]byte(frame)))
	}
	return seen.String()
}

func TestLoopReframerChatDropsPreamble(t *testing.T) {
	reframer := newContinuationReframer("/v1/chat/completions")
	frames := []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n",
		recoverChatDelta("and then "),
		recoverChatDelta("the end"),
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	seen := recoverFeedAll(reframer, frames)
	want := frames[1] + frames[2]
	if seen != want {
		t.Fatalf("client saw:\n%q\nwant only the deltas:\n%q", seen, want)
	}
	if got := reframer.EstimatedOutputTokens(); got != 4 {
		t.Fatalf("estimated tokens = %d, want 4 for 16 bytes of text", got)
	}

	finish := string(reframer.Finish())
	if !strings.Contains(finish, `"finish_reason":"stop"`) {
		t.Fatalf("finish = %q, want a stop reason", finish)
	}
	// The leg measured its own generation, so the client's closing frame reports that
	// measurement rather than what the spliced text is estimated to be worth.
	if !strings.Contains(finish, `"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}`) {
		t.Fatalf("finish = %q, want the leg's own usage in the closing frame", finish)
	}
	if strings.Count(finish, "data: [DONE]") != 1 {
		t.Fatalf("finish = %q, want exactly one [DONE]", finish)
	}
	if again := reframer.Finish(); again != nil {
		t.Fatalf("second Finish = %q, want nothing", again)
	}
	if after := reframer.Feed([]byte(recoverChatDelta("late"))); after != nil {
		t.Fatalf("Feed after Finish = %q, want nothing", after)
	}
}

func TestLoopReframerMessagesDropsPreamble(t *testing.T) {
	reframer := newContinuationReframer("/v1/messages")
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"role\":\"assistant\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: ping\ndata: {\"type\":\"ping\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"and then\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	seen := recoverFeedAll(reframer, frames)
	// The continuation's own message, pings and closing frames are dropped, and its
	// opening block is folded into the block the client has open. The stop that follows
	// closes that same block, so it is one of the two frames the client is owed.
	if want := frames[3] + frames[4]; seen != want {
		t.Fatalf("client saw:\n%q\nwant the delta and the block close:\n%q", seen, want)
	}
	for _, foreign := range []string{"msg_2", "message_start", "message_delta"} {
		if strings.Contains(seen, foreign) {
			t.Fatalf("client saw %q, which belongs to the continuation's own message:\n%s", foreign, seen)
		}
	}

	finish := string(reframer.Finish())
	// The leg closed the block it continued, so the terminal owes no close of its own:
	// a second one would close a block the client no longer has open.
	if strings.Contains(finish, "content_block_stop") {
		t.Fatalf("finish = %q, want no second close for a block the leg closed", finish)
	}
	if !strings.Contains(finish, `"stop_reason":"end_turn"`) {
		t.Fatalf("finish = %q, want a stop reason", finish)
	}
	// The leg reported the tokens it generated in its own message_delta, and a
	// measurement is what the client is told rather than the estimate of 8 bytes.
	if !strings.Contains(finish, `"output_tokens":3`) {
		t.Fatalf("finish = %q, want the leg's own output-token count", finish)
	}
	if strings.Count(finish, "event: message_stop") != 1 {
		t.Fatalf("finish = %q, want exactly one message_stop", finish)
	}
	if again := reframer.Finish(); again != nil {
		t.Fatalf("second Finish = %q, want nothing", again)
	}
}

func TestLoopReframerResponsesDropsPreamble(t *testing.T) {
	reframer := newContinuationReframer("/v1/responses")
	frames := []string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n",
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0}\n\n",
		"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"content_index\":0}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"and then\"}\n\n",
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"text\":\"and then\"}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
	}
	seen := recoverFeedAll(reframer, frames)
	if want := frames[3]; seen != want {
		t.Fatalf("client saw:\n%q\nwant only the delta:\n%q", seen, want)
	}
	if strings.Contains(seen, "completed") {
		t.Fatalf("client saw a second terminal frame:\n%s", seen)
	}

	finish := string(reframer.Finish())
	if strings.Count(finish, "event: response.completed") != 1 {
		t.Fatalf("finish = %q, want exactly one response.completed", finish)
	}
	if again := reframer.Finish(); again != nil {
		t.Fatalf("second Finish = %q, want nothing", again)
	}
}

func TestLoopReframerReassemblesSplitFrame(t *testing.T) {
	reframer := newContinuationReframer("/v1/chat/completions")
	frame := recoverChatDelta("hello")
	split := len(frame) / 2
	if seen := reframer.Feed([]byte(frame[:split])); seen != nil {
		t.Fatalf("half a frame released early: %q", seen)
	}
	if seen := string(reframer.Feed([]byte(frame[split:]))); seen != frame {
		t.Fatalf("released %q, want the whole frame %q", seen, frame)
	}
	if got := reframer.EstimatedOutputTokens(); got != 2 {
		t.Fatalf("estimated tokens = %d, want 2 for 5 bytes", got)
	}
}

func TestLoopReframerDropsBodyThatIsNotAStream(t *testing.T) {
	reframer := newContinuationReframer("/v1/chat/completions")
	seen := reframer.Feed([]byte(strings.Repeat("x", loopPendingBytes+1)))
	if seen != nil {
		t.Fatalf("released %d bytes of a non-SSE body", len(seen))
	}
	if len(reframer.pending) != 0 {
		t.Fatalf("pending = %d bytes, want the oversized body dropped", len(reframer.pending))
	}
}

func TestLoopReframerPassesUnknownPathThrough(t *testing.T) {
	reframer := newContinuationReframer("/v1/embeddings")
	chunk := []byte(`{"data":[]}`)
	if seen := reframer.Feed(chunk); string(seen) != string(chunk) {
		t.Fatalf("released %q, want the chunk untouched", seen)
	}
	if finish := reframer.Finish(); finish != nil {
		t.Fatalf("finish = %q, want nothing for a path this reframer does not know", finish)
	}
}

func TestLoopReframerMessagesClosesTheBlockTheClientHasOpen(t *testing.T) {
	// The cut can land past block 0 — after a thinking block, an earlier text block, a
	// tool call — so the continuation's block index has to be mapped onto the client's
	// open block, and the terminal has to close that block.
	reframer := newContinuationReframerFor("/v1/messages", 2)
	frames := []string{
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"and then\"}}\n\n",
	}
	seen := recoverFeedAll(reframer, frames)
	// The delta is remapped onto the block the client has open, and the leg's own block
	// start is folded into it: the index is the only thing in the frame that changes.
	for _, want := range []string{"event: content_block_delta", `"index":2`, `"text":"and then"`, `"type":"text_delta"`} {
		if !strings.Contains(seen, want) {
			t.Fatalf("client saw:\n%q\nmissing:\n%q", seen, want)
		}
	}
	if strings.Contains(seen, `"index":0`) {
		t.Fatalf("client saw the continuation's own block index:\n%q", seen)
	}
	if got := reframer.EstimatedOutputTokens(); got != 2 {
		t.Fatalf("estimated tokens = %d, want 2 for 8 bytes", got)
	}

	finish := string(reframer.Finish())
	if !strings.Contains(finish, `data: {"type":"content_block_stop","index":2}`) {
		t.Fatalf("finish = %q, want the client's own block 2 closed", finish)
	}
	if got := strings.Count(finish, "event: content_block_stop"); got != 1 {
		t.Fatalf("finish = %q, want exactly one block close", finish)
	}
}

func TestLoopReframerMessagesRemapsASecondBlock(t *testing.T) {
	// A continuation that answers with prose and then a tool call opens a second block.
	// The client already has its own block open, so the leg's first block continues that
	// one, the second is mapped onto the next index the client has not seen, and the
	// block the client was in is closed before the new one starts.
	reframer := newContinuationReframer("/v1/messages")
	frames := []string{
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"I will look\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"read_file\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
	}
	seen := recoverFeedAll(reframer, frames)
	for _, want := range []string{
		`"index":0,"delta":{"type":"text_delta","text":"I will look"}`,
		`data: {"type":"content_block_stop","index":0}`,
		`"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}`,
		`"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"`,
		`data: {"type":"content_block_stop","index":1}`,
	} {
		if !strings.Contains(seen, want) {
			t.Fatalf("client saw:\n%q\nmissing:\n%q", seen, want)
		}
	}
	// The leg's own opening block is the client's, so it is not opened a second time.
	if got := strings.Count(seen, "event: content_block_start"); got != 1 {
		t.Fatalf("%d block starts reached the client, want only the tool call's", got)
	}

	finish := string(reframer.Finish())
	// The tool block was closed by the leg, so the terminal closes nothing and reports
	// the reason the answer ended for: a tool call, not the model stopping.
	if strings.Contains(finish, "content_block_stop") {
		t.Fatalf("finish = %q, want no close for a block the leg already closed", finish)
	}
	if !strings.Contains(finish, `"stop_reason":"tool_use"`) {
		t.Fatalf("finish = %q, want the tool call named as the stop reason", finish)
	}
}

func TestLoopReframerChatForwardsToolCallIdentity(t *testing.T) {
	reframer := newContinuationReframer("/v1/chat/completions")
	identity := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}` + "\n\n"
	arguments := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.go\"}"}}]}}]}` + "\n\n"
	frames := []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n",
		identity,
		arguments,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":6}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	seen := recoverFeedAll(reframer, frames)
	// The frame that opens the call carries no arguments, so it generates no text — and
	// it is still the only thing that tells the client which call it was handed.
	if want := identity + arguments; seen != want {
		t.Fatalf("client saw:\n%q\nwant the tool call's frames:\n%q", seen, want)
	}
	if !reframer.Spliced() {
		t.Fatal("a spliced tool call is not counted as a splice")
	}

	finish := string(reframer.Finish())
	if !strings.Contains(finish, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish = %q, want the tool call named as the finish reason", finish)
	}
	if strings.Contains(finish, `"finish_reason":"stop"`) {
		t.Fatalf("finish = %q, want no claim that the model stopped", finish)
	}
	if !strings.Contains(finish, `"usage":{"completion_tokens":6,"total_tokens":6}`) {
		t.Fatalf("finish = %q, want the leg's usage in the closing frame", finish)
	}
}

func TestLoopReframerResponsesForwardsFunctionCallItems(t *testing.T) {
	reframer := newContinuationReframer("/v1/responses")
	item := `event: response.output_item.added` + "\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","name":"read_file","arguments":""}}` + "\n\n"
	text := `event: response.output_item.added` + "\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_2"}}` + "\n\n"
	arguments := "event: response.function_call_arguments.delta\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"path\\\":\"}\n\n"
	completed := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":5,\"total_tokens\":12}}}\n\n"
	seen := recoverFeedAll(reframer, []string{item, text, arguments})
	if want := item + arguments; seen != want {
		t.Fatalf("client saw:\n%q\nwant the function call's frames only:\n%q", seen, want)
	}

	// The completed event's own usage is what the client's closing frame reports.
	recoverFeedAll(reframer, []string{completed})
	finish := string(reframer.Finish())
	if !strings.Contains(finish, `"usage":{"input_tokens":7,"output_tokens":5,"total_tokens":12}`) {
		t.Fatalf("finish = %q, want the leg's usage in the completed event", finish)
	}
}

func TestLoopContinuationCapsTheOutputBudget(t *testing.T) {
	// One client response must not return twice the output it budgeted for: the
	// continuation asks for what is left of the client's budget, less what the response
	// already generated.
	prefix := strings.Repeat("x", 2000)
	generated := loopTokensFor(len(prefix))
	cases := []struct {
		name   string
		path   string
		body   string
		field  string
		field2 string
		want   int
	}{
		{"chat", "/v1/chat/completions", `{"messages":[{"content":"write a story","role":"user"}],"max_tokens":4096,"model":"m","stream":true}`, "max_tokens", "max_completion_tokens", 4096 - generated},
		{"messages", "/v1/messages", `{"max_tokens":4096,"messages":[{"content":"write a story","role":"user"}],"model":"m","stream":true}`, "max_tokens", "", 4096 - generated},
		{"responses", "/v1/responses", `{"input":[{"content":"write a story","role":"user"}],"max_output_tokens":4096,"model":"m","stream":true}`, "max_output_tokens", "", 4096 - generated},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			out, err := loopContinuationBody(test.path, []byte(test.body),
				loopVerdict{Channel: loopChannelContent, Prefix: prefix}, 0.4)
			if err != nil {
				t.Fatalf("loopContinuationBody: %v", err)
			}
			payload := recoverDecode(t, out)
			if got := payload[test.field]; got != float64(test.want) {
				t.Fatalf("%s = %v, want %d of the client's 4096 left", test.field, got, test.want)
			}
			if test.field2 != "" {
				if _, ok := payload[test.field2]; ok {
					t.Fatalf("%s was invented for a request that never sent it", test.field2)
				}
			}
			if test.want >= 4096 {
				t.Fatalf("the cap left the whole budget: %d", test.want)
			}
		})
	}
}

func TestLoopContinuationKeepsABudgetFloor(t *testing.T) {
	// A client that had spent almost all of its budget still gets room to finish the
	// sentence it was cut in — but never more than it budgeted.
	prefix := strings.Repeat("x", 4000)
	out, err := loopContinuationBody("/v1/messages",
		[]byte(`{"max_tokens":300,"messages":[{"content":"write a story","role":"user"}],"model":"m","stream":true}`),
		loopVerdict{Channel: loopChannelContent, Prefix: prefix}, 0.4)
	if err != nil {
		t.Fatalf("loopContinuationBody: %v", err)
	}
	payload := recoverDecode(t, out)
	if got := payload["max_tokens"]; got != float64(loopContinuationMinBudget) {
		t.Fatalf("max_tokens = %v, want the floor of %d", got, loopContinuationMinBudget)
	}
	if loopContinuationMinBudget > 300 {
		t.Fatalf("the floor raised a budget of 300 to %d", loopContinuationMinBudget)
	}
}

func TestLoopContinuationRestartsAThinkingLoop(t *testing.T) {
	// A thought that looped is not a turn: no provider takes it back as the assistant's
	// earlier message, so the continuation is the client's own conversation with one
	// instruction added, and the thought itself is never re-sent.
	thinking := "I'll write the response. I'll check the comments and report. "
	verdict := loopVerdict{Channel: loopChannelReasoning, Period: 64, Repeat: 8, Suffix: 512, Prefix: thinking}
	cases := []struct {
		name string
		path string
		body string
		list string
	}{
		{"chat", "/v1/chat/completions", recoverChatBody(), "messages"},
		{"messages", "/v1/messages", recoverMessagesBody(), "messages"},
		{"responses list input", "/v1/responses", recoverResponsesBody(`[{"content":"write a story","role":"user"}]`), "input"},
		{"responses string input", "/v1/responses", recoverResponsesBody(`"write a story"`), "input"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			out, err := loopContinuationBody(test.path, []byte(test.body), verdict, 0.4)
			if err != nil {
				t.Fatalf("loopContinuationBody: %v", err)
			}
			if strings.Contains(string(out), thinking) {
				t.Fatalf("the looping thought was re-sent:\n%s", out)
			}
			if count := strings.Count(string(out), loopRestart); count != 1 {
				t.Fatalf("the restart instruction appears %d times, want once:\n%s", count, out)
			}
			if strings.Contains(string(out), loopIntervention) {
				t.Fatalf("a restart asked to continue an answer that does not exist:\n%s", out)
			}
			if !strings.Contains(string(out), recoverProviderBlock) {
				t.Fatalf("provider block not preserved byte for byte:\n%s", out)
			}
			payload := recoverDecode(t, out)
			if payload["stream"] != true {
				t.Fatalf("stream = %v, want true", payload["stream"])
			}
			list, _ := payload[test.list].([]any)
			if len(list) != 1 {
				t.Fatalf("%s = %d turns, want the client's own turn and nothing else", test.list, len(list))
			}
			turn, _ := list[0].(map[string]any)
			if turn["role"] != "user" {
				t.Fatalf("turn = %v, want the client's user turn", turn)
			}
			// The instruction joined the last user turn rather than opening a second one
			// in a row, which is what keeps role alternation intact.
			content, _ := turn["content"].(string)
			if !strings.HasPrefix(content, "write a story") || !strings.HasSuffix(content, loopRestart) {
				t.Fatalf("content = %q, want the client's turn with the instruction appended", content)
			}
			// Only chat completions has repetition penalties; the other two APIs must not
			// be sent invented parameters.
			penalty, hasPenalty := payload["frequency_penalty"]
			if hasPenalty != (test.path == "/v1/chat/completions") {
				t.Fatalf("frequency_penalty = %v on %s, want it only where it exists", penalty, test.path)
			}
		})
	}
}

func TestLoopRestartOpensAUserTurnWhenTheLastTurnIsNotTheUsers(t *testing.T) {
	body := `{"messages":[{"content":"write a story","role":"user"},` +
		`{"content":"once upon a time","role":"assistant"}],"model":"m","stream":true}`
	out, err := loopContinuationBody("/v1/chat/completions", []byte(body),
		loopVerdict{Channel: loopChannelReasoning, Prefix: "thinking it over "}, 0.4)
	if err != nil {
		t.Fatalf("loopContinuationBody: %v", err)
	}
	messages, _ := recoverDecode(t, out)["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want the client's two turns plus the instruction", len(messages))
	}
	previous, _ := messages[1].(map[string]any)
	if previous["role"] != "assistant" || previous["content"] != "once upon a time" {
		t.Fatalf("the client's assistant turn was rewritten: %v", previous)
	}
	last, _ := messages[2].(map[string]any)
	if last["role"] != "user" || last["content"] != loopRestart {
		t.Fatalf("last turn = %v, want the instruction as a user turn of its own", last)
	}
}

func TestLoopRestartAppendsATextPartToATypedTurn(t *testing.T) {
	body := `{"messages":[{"content":[{"type":"text","text":"write a story"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}],"role":"user"}],` +
		`"model":"m","stream":true}`
	out, err := loopContinuationBody("/v1/messages", []byte(body),
		loopVerdict{Channel: loopChannelReasoning}, 0.4)
	if err != nil {
		t.Fatalf("loopContinuationBody: %v", err)
	}
	messages, _ := recoverDecode(t, out)["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want the client's one turn", len(messages))
	}
	turn, _ := messages[0].(map[string]any)
	parts, _ := turn["content"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want the client's two plus the instruction", len(parts))
	}
	added, _ := parts[2].(map[string]any)
	if added["type"] != "text" || added["text"] != loopRestart {
		t.Fatalf("added part = %v, want the instruction as a text part", added)
	}
}

func TestLoopContinuationRestartJoinsAResponsesTypedTurn(t *testing.T) {
	// The Responses API names a text part input_text, so a restart that appended a
	// chat-style "text" part to a typed input turn would be refused by the API.
	body := `{"input":[{"role":"user","content":[{"type":"input_text","text":"write a story"}]}],` +
		`"model":"m","stream":true}`
	out, err := loopContinuationBody("/v1/responses", []byte(body),
		loopVerdict{Channel: loopChannelReasoning}, 0.4)
	if err != nil {
		t.Fatalf("loopContinuationBody: %v", err)
	}
	input, _ := recoverDecode(t, out)["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %d items, want the client's own item", len(input))
	}
	turn, _ := input[0].(map[string]any)
	parts, _ := turn["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want the client's one plus the instruction", len(parts))
	}
	added, _ := parts[1].(map[string]any)
	if added["type"] != "input_text" || added["text"] != loopRestart {
		t.Fatalf("added part = %v, want the instruction as an input_text part", added)
	}
}

func TestLoopRestartLeavesAnUnrewritableTurnAlone(t *testing.T) {
	// A content shape this package does not rewrite is not replaced with the
	// instruction: deleting part of the client's request would be worse than the extra
	// user turn that keeps it.
	body := `{"messages":[{"content":{"unexpected":"shape"},"role":"user"}],"model":"m","stream":true}`
	out, err := loopContinuationBody("/v1/chat/completions", []byte(body),
		loopVerdict{Channel: loopChannelReasoning}, 0.4)
	if err != nil {
		t.Fatalf("loopContinuationBody: %v", err)
	}
	messages, _ := recoverDecode(t, out)["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want the client's turn plus the instruction", len(messages))
	}
	first, _ := messages[0].(map[string]any)
	content, _ := first["content"].(map[string]any)
	if content["unexpected"] != "shape" {
		t.Fatalf("the client's content was rewritten: %v", first)
	}
	second, _ := messages[1].(map[string]any)
	if second["role"] != "user" || second["content"] != loopRestart {
		t.Fatalf("second turn = %v, want the instruction as a user turn of its own", second)
	}
}

func TestLoopRestartFillsAUserTurnWithNoContent(t *testing.T) {
	// An empty user turn is a shape this package can rewrite: the instruction becomes its
	// content rather than opening a second user turn beside it.
	body := `{"messages":[{"content":null,"role":"user"}],"model":"m","stream":true}`
	out, err := loopContinuationBody("/v1/chat/completions", []byte(body),
		loopVerdict{Channel: loopChannelReasoning}, 0.4)
	if err != nil {
		t.Fatalf("loopContinuationBody: %v", err)
	}
	messages, _ := recoverDecode(t, out)["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want the client's one turn", len(messages))
	}
	turn, _ := messages[0].(map[string]any)
	if turn["role"] != "user" || turn["content"] != loopRestart {
		t.Fatalf("turn = %v, want the instruction as its content", turn)
	}
}

func TestLoopContinuationRejectsAThinkingLoopAfterTheAnswerStarted(t *testing.T) {
	// Restarting there would append a second answer to prose the client is already
	// reading, so the answer ends at the cut instead.
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		out, err := loopContinuationBody(path, []byte(recoverChatBody()),
			loopVerdict{Channel: loopChannelReasoning, Prefix: "thinking it over ", Generated: true}, 0.4)
		if !errors.Is(err, errLoopAnswerStarted) {
			t.Fatalf("%s: err = %v, want errLoopAnswerStarted", path, err)
		}
		if out != nil {
			t.Fatalf("%s: built a body anyway:\n%s", path, out)
		}
	}
}

func TestLoopReframerForwardsStructuredReasoning(t *testing.T) {
	// A restarted leg can think before it answers, and that thought is the client's to
	// see: a frame it cannot read must not be taken for a frame it does not need.
	reframer := newContinuationReframer("/v1/chat/completions")
	frame := loopChatReasoningDetailFrame("The answer picks itself back up here. ")
	if out := string(reframer.Feed([]byte(frame))); out != frame {
		t.Fatalf("client saw %q, want the leg's reasoning frame byte for byte", out)
	}
	if !reframer.Spliced() {
		t.Fatal("a leg whose first text was reasoning was not counted as spliced")
	}
	if got := reframer.EstimatedOutputTokens(); got != loopTokensFor(len("The answer picks itself back up here. ")) {
		t.Fatalf("estimated tokens = %d, want the text it passed through", got)
	}
}

func TestLoopReframerDropsEncryptedReasoning(t *testing.T) {
	// An encrypted item is the leg's own identity, not text the client is owed: it
	// carries nothing readable and belongs to the response the splice drops.
	reframer := newContinuationReframer("/v1/chat/completions")
	if out := reframer.Feed([]byte(loopChatEncryptedReasoningFrame("gAAAAABm5T0X"))); len(out) != 0 {
		t.Fatalf("client saw %q, want the opaque item dropped", out)
	}
	if reframer.Spliced() {
		t.Fatal("an encrypted item counted as spliced text")
	}
}
