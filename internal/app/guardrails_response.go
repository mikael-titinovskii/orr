package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// guardResponseStreamPrime bounds how much of a streamed response is checked
	// before the first byte reaches the client. It is a prefix gate: streaming is
	// preserved, and the rest of the response is not re-checked.
	guardResponseStreamPrime = 8 << 10
	// guardResponsePrimeBudget bounds how long that prefix may hold the client
	// back. A plain read to the limit would withhold the first token until the
	// model had generated the whole prefix, so the budget is what ends the wait
	// for an ordinary answer; the limit only stops a fast stream earlier.
	// guardResponseBodyPrime bounds the non-streaming body checked before it is
	// forwarded. A larger body is checked on its prefix and then forwarded.
	guardResponseBodyPrime = 8 << 20
)

// guardResponsePrimeBudget is a variable so a test can shrink the wait; nothing
// in production changes it.
var guardResponsePrimeBudget = 1500 * time.Millisecond

// guardUpstreamResponse checks a successful upstream response before it reaches
// the client. It returns blocked with the rail that rejected it, or an error
// when the sidecar could not answer (fail-closed in block mode).
func (p *proxy) guardUpstreamResponse(ctx context.Context, resp *http.Response, record *requestRecord) (bool, string, error) {
	if p.cfg.Guardrails.Mode != guardrailsModeOn {
		return false, "", nil
	}
	action := p.cfg.Guardrails.Action
	if action == "" {
		action = guardrailsActionBlock
	}
	if action == guardrailsActionOff {
		return false, "", nil
	}

	streaming := isEventStream(resp.Header.Get("Content-Type"))
	var prefix []byte
	var err error
	if streaming {
		primed := newPrimedResponseBody(resp.Body, guardResponseStreamPrime)
		prefix = primed.Prime(guardResponsePrimeBudget)
		resp.Body = primed
	} else {
		prefix, err = io.ReadAll(io.LimitReader(resp.Body, guardResponseBodyPrime))
		resp.Body = prefixedBody{prefix: bytes.NewReader(prefix), rest: resp.Body}
	}
	if err != nil {
		// A body that failed mid-read is an upstream failure, not a guardrail
		// verdict: it is reported in every action mode, so observe mode does not
		// hand the client a truncated answer marked successful.
		return false, "", err
	}
	if len(prefix) == 0 {
		return false, "", nil
	}
	if text := responseGuardText(prefix, streaming); text != "" {
		content, err := json.Marshal(text)
		if err != nil {
			return false, "", err
		}
		verdict, err := p.guardrailCheck(ctx, record, record.Model, []guardrailMessage{{
			Role: "assistant", Content: content,
		}}, []string{"output"})
		if err != nil {
			return p.guardResponseFailure(record, "output", action, err)
		}
		if blocked, rail := p.recordGuardVerdict(record, verdict, action, text); blocked {
			return true, rail, nil
		}
	}
	// Tool calls are checked by the tool_call rails, on the arguments the harness
	// would execute. They are not part of the prose check above, which drops them
	// on purpose.
	if calls := completeToolCalls(responseToolCalls(prefix, streaming), streaming); len(calls) > 0 {
		arguments := encodeToolCalls(calls)
		verdict, err := p.guardrailCheck(ctx, record, record.Model, []guardrailMessage{{
			Role: "assistant", ToolCalls: arguments,
		}}, []string{"tool_call"})
		if err != nil {
			return p.guardResponseFailure(record, "output", action, err)
		}
		if blocked, rail := p.recordGuardVerdict(record, verdict, action, string(arguments)); blocked {
			return true, rail, nil
		}
	}
	return false, "", nil
}

// completeToolCalls makes every streamed call judgeable. The window ends
// wherever the answer had got to, so the last call in a prefix routinely holds
// half a JSON object; the sidecar cannot parse that and answers with a block
// that names no rail, which is what refused ordinary long writes. Dropping such
// a call instead would hand the harness an unjudged call whose arguments the
// window did not reach. So a truncated call is checked on what the window did
// see: the argument value the rails read, taken from the partial text. A
// non-streaming body is read in full, so a call there that still does not parse
// is a real anomaly and stays in the check as it arrived.
func completeToolCalls(calls []rawToolCall, streaming bool) []rawToolCall {
	if !streaming {
		return calls
	}
	judged := make([]rawToolCall, 0, len(calls))
	for _, call := range calls {
		if json.Valid([]byte(call.Function.Arguments)) {
			judged = append(judged, call)
			continue
		}
		arguments := partialToolArguments(canonicalToolName(call.Function.Name), call.Function.Arguments)
		if arguments == "" {
			// A tool outside the canonical classes has no rail of its own, so
			// there is nothing to answer for it either way.
			continue
		}
		call.Function.Arguments = arguments
		judged = append(judged, call)
	}
	return judged
}

// partialToolArguments builds the argument object a rail reads out of a call the
// prime window cut. The rails look at one value — the command or the path — so
// that value is taken from the partial text when it is there; when it is not,
// the visible text itself is judged rather than passed unchecked, because the
// provider chooses its field order and could otherwise push the path past the
// window.
func partialToolArguments(tool, arguments string) string {
	aliases := toolArgumentAliases[tool]
	if len(aliases) == 0 {
		return ""
	}
	target := "path"
	if tool == "run_command" {
		target = "command"
	}
	value, ok := partialJSONField(arguments, append([]string{target}, aliases...))
	if !ok {
		value = arguments
	}
	encoded, err := json.Marshal(map[string]string{target: value})
	if err != nil {
		return ""
	}
	return string(encoded)
}

// partialJSONField returns the value of the first of names that appears in a
// possibly truncated JSON object. A value cut by the window is still returned:
// what the rails match is the text that was visible.
func partialJSONField(text string, names []string) (string, bool) {
	for _, name := range names {
		quoted := `"` + name + `"`
		at := strings.Index(text, quoted)
		if at < 0 {
			continue
		}
		rest := text[at+len(quoted):]
		rest = strings.TrimLeft(rest, " \t\r\n")
		rest = strings.TrimPrefix(rest, ":")
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" {
			// The window ended at the key: the value never arrived.
			return "", true
		}
		if !strings.HasPrefix(rest, `"`) {
			continue
		}
		value, closed := partialJSONString(rest[1:])
		if value != "" || closed {
			return value, true
		}
	}
	return "", false
}

// partialJSONString returns the body of a JSON string up to its closing quote,
// stopping at the end of a truncated input. Nothing is unescaped: the rails match
// the characters that were visible, and a rewritten escape is one more way for a
// value to differ from what the harness will run.
func partialJSONString(text string) (string, bool) {
	escaped := false
	for at, character := range text {
		switch {
		case escaped:
			escaped = false
		case character == '\\':
			escaped = true
		case character == '"':
			return text[:at], true
		}
	}
	return text, false
}

// recordGuardVerdict stores a blocked verdict, and reports whether the caller
// should answer with a refusal. In observe mode, or while an operator window
// from `orr guardrails allow` is open, the verdict is recorded and the response
// still reaches the client.
// guardrailUnnamedRail names a block the sidecar reported without a rail. A
// tool call the sidecar cannot parse is blocked by its manager rather than by
// a declared rail, and an empty reason in the log would leave an operator with
// "GUARD BLOCKED" and nothing else to go on.
func guardrailUnnamedRail(rail, reason string) string {
	if rail != "" {
		return rail
	}
	if head, _, ok := strings.Cut(reason, ":"); ok && strings.TrimSpace(head) != "" {
		return strings.TrimSpace(head)
	}
	if strings.TrimSpace(reason) != "" {
		return strings.TrimSpace(reason)
	}
	return "check"
}

func (p *proxy) recordGuardVerdict(record *requestRecord, verdict guardrailVerdict, action, sample string) (bool, string) {
	if verdict.Status != "blocked" {
		return false, ""
	}
	rail := guardrailUnnamedRail(verdict.Rail, verdict.Reason)
	record.GuardPhase = "output"
	record.GuardRail = rail
	record.GuardLabel = verdict.Label
	record.GuardText = guardrailSample(sample)
	if p.guardrailsEnforce(action) {
		record.GuardBlocked = true
	} else {
		record.GuardObserved = true
	}
	return record.GuardBlocked, rail
}

// guardResponseFailure records a check that could not answer. It is fatal only
// in block mode: observe mode documents that the response is left untouched, so
// a completed upstream answer must not be traded for a 503 because the sidecar
// was restarting. The error is logged in both modes: the refusal alone says a
// check failed, not whether the sidecar answered with an error or never
// received the request.
func (p *proxy) guardResponseFailure(record *requestRecord, phase, action string, err error) (bool, string, error) {
	log.Printf("guardrails: %s check failed: %v", phase, err)
	logGuardrailUnavailable(record, phase)
	if action == guardrailsActionBlock {
		return false, "", err
	}
	return false, "", nil
}

func logGuardrailUnavailable(record *requestRecord, phase string) {
	record.GuardPhase = phase
	record.GuardRail = "check unavailable"
	record.GuardUnavailable = true
}

// responseGuardText extracts the answer text a response carries, and drops the
// code in it: a coding model's answer is mostly fenced snippets and inline spans,
// which a client renders as text rather than executes. Checking them would block
// ordinary answers while protecting nothing.
func responseGuardText(body []byte, streaming bool) string {
	if streaming {
		return stripCode(streamGuardText(body))
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return stripCode(string(body))
	}
	var builder strings.Builder
	collectResponseText(value, &builder, 0, true)
	return stripCode(builder.String())
}

// streamGuardText rebuilds the text of an SSE prefix from its data payloads.
// Scanning the raw frame instead would mix prose with JSON keys and, worse,
// keep the escaped newlines that make fenced blocks impossible to recognise.
func streamGuardText(body []byte) string {
	var builder strings.Builder
	found := false
	for _, line := range bytes.Split(body, []byte("\n")) {
		payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var value any
		if err := json.Unmarshal(payload, &value); err != nil {
			// The prefix is cut mid-frame; the incomplete tail is not checked.
			continue
		}
		found = true
		collectResponseText(value, &builder, 0, false)
	}
	if !found {
		return string(body)
	}
	return builder.String()
}

// collectResponseText appends the strings a response carries and leaves out the
// protocol bookkeeping around them. Every frame of a stream repeats its id, model
// name, object type, provider and role, and every reasoning delta repeats its
// type and format; read as answer text those are spliced between the fragments
// of the answer, and a classifier takes the repetition for obfuscation — a
// benign thinking that passes on its own was blocked once the frame metadata was
// interleaved with it.
//
// A completed message keeps its fields on separate lines so that two of them
// cannot be read as one run of text; streamed deltas are consecutive pieces of
// one answer and are joined.
func collectResponseText(value any, builder *strings.Builder, depth int, separate bool) {
	if depth > 32 || builder.Len() > 4<<20 {
		return
	}
	switch typed := value.(type) {
	case string:
		builder.WriteString(typed)
		if separate {
			builder.WriteByte('\n')
		}
	case []any:
		for _, item := range typed {
			collectResponseText(item, builder, depth+1, separate)
		}
	case map[string]any:
		if isToolCallValue(typed) {
			return
		}
		for _, key := range sortedKeys(typed) {
			if isProtocolKey(key) {
				continue
			}
			// A reasoning block is repeated as structured details beside the
			// reasoning field; reading both would halve the window's reach.
			if key == "reasoning_details" && hasText(typed, "reasoning", "reasoning_content") {
				continue
			}
			collectResponseText(typed[key], builder, depth+1, separate)
		}
	}
}

func hasText(value map[string]any, keys ...string) bool {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && text != "" {
			return true
		}
	}
	return false
}

// sortedKeys keeps field order stable: map iteration is randomised, so a frame's
// strings would otherwise be assembled in a different order per request, and a
// fence delimiter that is its own delta would be recognised only by luck.
func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// isProtocolKey reports the fields that carry a protocol's bookkeeping rather
// than answer text: an id, a model name, an object type, a provider name, a
// role, an index, a stop reason, and the type and format of a reasoning block.
// None of them is rendered as the answer, and each repeats in every frame of a
// stream.
//
// A model's tool call is here too. Its arguments are code by construction — a
// file being written, a command being built — so reading them as answer text
// flags every React component and every HTML snippet. They are judged by the
// tool_call rails instead.
func isProtocolKey(key string) bool {
	switch key {
	case "id", "object", "model", "provider", "provider_name", "created",
		"system_fingerprint", "service_tier", "type", "format", "role", "index",
		"finish_reason", "native_finish_reason", "stop_reason", "stop_sequence",
		"signature", "logprobs", "usage", "event", "status", "request_id",
		"item_id", "response_id":
		return true
	case "tool_calls", "tool_call", "function_call", "tool_use", "arguments", "input", "partial_json":
		return true
	}
	return false
}

func isToolCallValue(value map[string]any) bool {
	kind, _ := value["type"].(string)
	return kind == "tool_use" || kind == "function_call"
}

// stripCode removes fenced blocks, inline spans, and everything after an
// unterminated fence.
// stripCode removes fenced blocks and inline spans from prose. A fence that is
// never closed, or an odd number of backticks, is checked rather than dropped:
// otherwise the answer chooses what the rails get to see by opening a fence.
func stripCode(text string) string {
	var builder strings.Builder
	var tail []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		if isFence(line) {
			inFence = !inFence
			if !inFence {
				// A closed fence is code a client displays; only the tail after
				// the last close is prose, so nothing earlier may be re-checked.
				tail = tail[:0]
			}
			continue
		}
		if inFence {
			tail = append(tail, line)
			continue
		}
		builder.WriteString(stripSpans(line))
		builder.WriteByte('\n')
	}
	if inFence {
		// Unterminated: everything after it is prose the client still reads.
		for _, line := range tail {
			builder.WriteString(stripSpans(line))
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

// isFence reports whether a line opens or closes a fenced code block.
func isFence(line string) bool {
	line = strings.TrimLeft(line, " \t")
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return false
	}
	run := 1
	for run < len(line) && line[run] == line[0] {
		run++
	}
	return run >= 3
}

// stripSpans removes inline code spans. A line with an odd number of backticks
// is returned intact: dropping its tail would let an unbalanced backtick hide
// the rest of the line from the rails.
func stripSpans(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}
	if strings.Count(line, "`")%2 != 0 {
		return line
	}
	var builder strings.Builder
	inSpan := false
	for _, character := range line {
		if character == '`' {
			inSpan = !inSpan
			continue
		}
		if !inSpan {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

// responseToolCalls returns the tool calls a response carried. Their arguments
// are executed by the harness, so they are judged by the tool_call rails rather
// than read as answer text.
func responseToolCalls(body []byte, streaming bool) []rawToolCall {
	if streaming {
		return streamToolCalls(body)
	}
	return messageToolCalls(body)
}

// messageToolCalls reads the calls of a complete response, in both the Chat
// Completions shape and the Anthropic content-block shape.
func messageToolCalls(body []byte) []rawToolCall {
	var payload struct {
		Choices []struct {
			Message struct {
				ToolCalls []rawToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Content []rawContentBlock `json:"content"`
		Output  []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			CallID    string `json:"call_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	var calls []rawToolCall
	for _, choice := range payload.Choices {
		calls = append(calls, choice.Message.ToolCalls...)
	}
	for _, item := range payload.Output {
		if item.Type != "function_call" {
			continue
		}
		call := rawToolCall{ID: firstNonEmpty(item.CallID, item.ID), Type: "function"}
		call.Function.Name = item.Name
		call.Function.Arguments = item.Arguments
		calls = append(calls, call)
	}
	for _, block := range payload.Content {
		if block.Type != "tool_use" {
			continue
		}
		call := rawToolCall{ID: block.ID, Type: "function"}
		call.Function.Name = block.Name
		if len(block.Input) > 0 && string(block.Input) != "null" {
			call.Function.Arguments = string(block.Input)
		}
		calls = append(calls, call)
	}
	return calls
}

// streamToolCalls reassembles calls that arrive as deltas: Chat Completions
// sends fragments indexed per call, Anthropic sends a tool_use block start
// followed by input_json_delta fragments, and the Responses API sends a
// function_call item followed by function_call_arguments deltas.
func streamToolCalls(body []byte) []rawToolCall {
	type pending struct {
		call rawToolCall
		args strings.Builder
	}
	entries := map[string]*pending{}
	var order []string
	entry := func(key, id, name string) *pending {
		if existing, ok := entries[key]; ok {
			return existing
		}
		created := &pending{call: rawToolCall{ID: id, Type: "function"}}
		created.call.Function.Name = name
		entries[key] = created
		order = append(order, key)
		return created
	}
	// responseKeys maps an output index to the entry its call was opened under,
	// because the argument deltas carry only the index.
	responseKeys := map[int]string{}
	for _, line := range bytes.Split(body, []byte("\n")) {
		payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var event struct {
			Type        string `json:"type"`
			Index       int    `json:"index"`
			OutputIndex int    `json:"output_index"`
			Choices     []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			ContentBlock *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Item *struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				CallID    string `json:"call_id"`
			} `json:"item"`
			Delta json.RawMessage `json:"delta"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			// The prefix is cut mid-frame; an incomplete frame carries nothing
			// that can be checked and is left to the next window.
			continue
		}
		for _, choice := range event.Choices {
			for _, fragment := range choice.Delta.ToolCalls {
				current := entry(fmt.Sprintf("chat-%d", fragment.Index), fragment.ID, fragment.Function.Name)
				if fragment.ID != "" {
					current.call.ID = fragment.ID
				}
				if fragment.Function.Name != "" {
					current.call.Function.Name = fragment.Function.Name
				}
				current.args.WriteString(fragment.Function.Arguments)
			}
		}
		switch event.Type {
		case "content_block_start":
			if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				entry(fmt.Sprintf("anthropic-%d", event.Index), event.ContentBlock.ID, event.ContentBlock.Name)
			}
		case "content_block_delta":
			var delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			}
			if err := json.Unmarshal(event.Delta, &delta); err == nil && delta.Type == "input_json_delta" {
				if current, ok := entries[fmt.Sprintf("anthropic-%d", event.Index)]; ok {
					current.args.WriteString(delta.PartialJSON)
				}
			}
		case "response.output_item.added", "response.output_item.done":
			if event.Item == nil || event.Item.Type != "function_call" {
				continue
			}
			key := fmt.Sprintf("responses-%d", event.OutputIndex)
			current := entry(key, firstNonEmpty(event.Item.CallID, event.Item.ID), event.Item.Name)
			if event.Item.Name != "" {
				current.call.Function.Name = event.Item.Name
			}
			// A stream may carry the finished item without any argument deltas.
			if current.args.Len() == 0 {
				current.args.WriteString(event.Item.Arguments)
			}
			responseKeys[event.OutputIndex] = key
		case "response.function_call_arguments.done":
			var done struct {
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal(payload, &done); err != nil || done.Arguments == "" {
				continue
			}
			key, ok := responseKeys[event.OutputIndex]
			if !ok {
				key = fmt.Sprintf("responses-%d", event.OutputIndex)
				responseKeys[event.OutputIndex] = key
			}
			current := entry(key, "", "")
			if current.args.Len() == 0 {
				current.args.WriteString(done.Arguments)
			}
		case "response.function_call_arguments.delta":
			var fragment string
			if err := json.Unmarshal(event.Delta, &fragment); err != nil {
				continue
			}
			key, ok := responseKeys[event.OutputIndex]
			if !ok {
				key = fmt.Sprintf("responses-%d", event.OutputIndex)
				responseKeys[event.OutputIndex] = key
			}
			entry(key, "", "").args.WriteString(fragment)
		}
	}
	calls := make([]rawToolCall, 0, len(order))
	for _, key := range order {
		current := entries[key]
		current.call.Function.Arguments = current.args.String()
		calls = append(calls, current.call)
	}
	return calls
}

// prefixedBody lets the prefix be checked without consuming it: the first read
// serves the buffered bytes, then the rest of the upstream body.
type prefixedBody struct {
	prefix *bytes.Reader
	rest   io.ReadCloser
}

func (b prefixedBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	return b.rest.Read(p)
}

func (b prefixedBody) Close() error {
	return b.rest.Close()
}

// primedResponseBody reads a live stream in the background while the gate looks
// at its beginning. Reading the prefix inline would hold the client until the
// model had generated all of it; here Prime takes whatever arrived within its
// budget, and the stream resumes from exactly that point — nothing is dropped
// and nothing is buffered twice.
type primedResponseBody struct {
	mu   sync.Mutex
	buf  []byte
	off  int
	done bool
	wake chan struct{}
	rest io.ReadCloser
}

func newPrimedResponseBody(rest io.ReadCloser, limit int) *primedResponseBody {
	body := &primedResponseBody{rest: rest, wake: make(chan struct{}, 1)}
	go body.fill(limit)
	return body
}

// fill reads until the prefix limit or the end of the stream, then stops and
// leaves the rest of the body to Read.
func (b *primedResponseBody) fill(limit int) {
	chunk := make([]byte, 32<<10)
	for {
		n, err := b.rest.Read(chunk)
		if n > 0 {
			b.mu.Lock()
			b.buf = append(b.buf, chunk[:n]...)
			reached := len(b.buf) >= limit
			b.mu.Unlock()
			b.signal()
			if reached {
				b.finish()
				return
			}
		}
		if err != nil {
			b.finish()
			return
		}
	}
}

func (b *primedResponseBody) finish() {
	b.mu.Lock()
	b.done = true
	b.mu.Unlock()
	b.signal()
}

func (b *primedResponseBody) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Prime returns the bytes the gate checks. They stay in the buffer: a response
// that passes is forwarded from its first byte, so priming must not consume it.
// The first bytes are awaited without a deadline — the client is waiting for
// exactly those bytes anyway, and returning empty would skip the check — and
// the budget then bounds how much more may be gathered.
func (b *primedResponseBody) Prime(budget time.Duration) []byte {
	for {
		b.mu.Lock()
		done, size := b.done, len(b.buf)
		b.mu.Unlock()
		if done || size > 0 {
			break
		}
		<-b.wake
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	for {
		b.mu.Lock()
		done := b.done
		b.mu.Unlock()
		if done {
			break
		}
		select {
		case <-b.wake:
		case <-timer.C:
			return b.snapshot()
		}
	}
	return b.snapshot()
}

func (b *primedResponseBody) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...)
}

func (b *primedResponseBody) Read(p []byte) (int, error) {
	for {
		b.mu.Lock()
		if b.off < len(b.buf) {
			n := copy(p, b.buf[b.off:])
			b.off += n
			b.mu.Unlock()
			return n, nil
		}
		done := b.done
		b.mu.Unlock()
		if done {
			return b.rest.Read(p)
		}
		<-b.wake
	}
}

func (b *primedResponseBody) Close() error {
	return b.rest.Close()
}

// sentinel
