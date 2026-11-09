package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// This file holds the two pure pieces of loop recovery: the continuation request
// that asks a model to resume a response it was repeating in, and the reframer that
// turns that second stream back into the tail of the first one. Neither touches the
// streaming path; a caller that has a verdict splices them together. No response
// text is logged or persisted anywhere in this file.

// errLoopNotContinuable reports a verdict a continuation cannot resume from. A
// tool-argument prefix can be a mid-JSON fragment, so building a request from it
// would ask the model to continue something it never wrote. The caller falls back to
// aborting the response instead.
var errLoopNotContinuable = errors.New("loop: channel is not continuable")

// errLoopAnswerStarted reports a thinking loop in a response that has already
// generated answer text. A thinking loop is restarted rather than continued, and a
// restart there would ask for a second answer inside the message the client is
// already reading, so that verdict is aborted instead.
var errLoopAnswerStarted = errors.New("loop: answer text already generated")

// errLoopEmptyPrefix reports a content loop with nothing before the repetition to
// resume from: choices[] with n > 1 shares one channel and reports no prefix at
// all. A continuation with no assistant turn to continue would ask the model to
// answer the user's question from scratch inside a response the client is already
// reading, so that verdict is aborted instead.
var errLoopEmptyPrefix = errors.New("loop: no clean prefix to continue from")

// loopContinuable reports why a verdict cannot be continued, or nil when it can.
// It is the single policy the continuation request and the stream stage share, so
// the decision to spend a request is made in one place.
func loopContinuable(verdict *loopVerdict) error {
	if verdict == nil {
		return errLoopNotContinuable
	}
	switch verdict.Channel {
	case loopChannelContent:
		if verdict.Prefix == "" {
			return errLoopEmptyPrefix
		}
		return nil
	case loopChannelReasoning:
		// Thinking is not a turn: no provider takes a thought back as the assistant's
		// own earlier message, so a thinking loop is answered by restarting the answer
		// rather than by continuing the thought. That is only worth a request while the
		// response has generated nothing the client holds — a restart after prose began
		// would append a second answer to it, and one after a tool call would ask for a
		// second call to go with the one already delivered.
		if verdict.Generated {
			return errLoopAnswerStarted
		}
		return nil
	}
	return fmt.Errorf("%w: %s channel", errLoopNotContinuable, verdict.Channel)
}

// loopIntervention is the user turn a continuation request adds after the clean
// prefix. It is the only thing the model is told about the cut, so it says what
// happened and what not to do in as few words as work.
const loopIntervention = "Your previous answer degenerated into a repetition and was cut off. " +
	"Continue from exactly where it stopped, without repeating text you already wrote, " +
	"without restarting the answer, and without commenting on this instruction."

// loopRestart is the user turn a restart adds when the repetition was in the model's
// thinking and the response had generated nothing the client holds. There is no turn to
// continue there, so it asks for the answer itself rather than for the rest of one.
const loopRestart = "Your previous attempt degenerated into a repeating thought and was cut off before it produced an answer. " +
	"Answer the request above directly, without repeating yourself, and without commenting on this instruction."

// loopBytesPerToken is the ratio a continuation leg's output is priced at when no
// usage frame arrived. It is an estimate: English prose runs close to four bytes per
// token, while code and non-Latin scripts run further from it.
const loopBytesPerToken = 4

// loopContinuationMinBudget is the floor a continuation request's output budget is
// kept at. A continuation exists to get an answer past a loop, so a client budget that
// was almost spent still leaves enough room to end the sentence it was cut in — but
// never more than the client budgeted in the first place.
const loopContinuationMinBudget = 256

// String names the channel in an error or a log line. Only the name is reported,
// never the text.
func (c loopChannel) String() string {
	switch c {
	case loopChannelContent:
		return "content"
	case loopChannelReasoning:
		return "reasoning"
	case loopChannelToolArgs:
		return "tool-arguments"
	}
	return "none"
}

// loopContinuationBody rewrites an already-prepared request body into the request
// that asks the model to resume the text a verdict cut off.
//
// A repetition in the answer re-sends the clean prefix as the model's own earlier
// assistant turn, followed by one short user turn telling it that its output
// degenerated into a repetition and to continue from exactly where it stopped. A
// repetition in the model's thinking has no such turn to offer — a thought is not
// something a provider takes back as the assistant's own message — so it restarts
// instead: the client's conversation plus one instruction to answer directly, with the
// thought that looped left out. Either way the repetition itself is never re-sent: the
// point of the continuation is to get the answer past the loop.
//
// Sampling differs by protocol. Chat completions has frequency and presence
// penalties, so it gets the configured penalty and half of it; the Responses and
// Messages APIs have no such parameters and must not be sent invented ones, so they
// get the intervention turn alone. stream stays on for every path.
//
// The provider block orr injected is copied from the original bytes, so the
// continuation is routed to the same provider as the request it continues. A body
// that has no provider block simply keeps none.
func loopContinuationBody(path string, body []byte, verdict loopVerdict, penalty float64) ([]byte, error) {
	if err := loopContinuable(&verdict); err != nil {
		return nil, err
	}
	if !mutablePaths[path] {
		return nil, fmt.Errorf("loop: no continuation request shape for %s", path)
	}
	payload, err := loopDecodeBody(body)
	if err != nil {
		return nil, err
	}
	// The injected provider block is re-emitted from its original bytes rather than
	// from the decoded value, so encoding cannot reorder or reformat it.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("loop: continuation body: %w", err)
	}
	if provider, ok := raw["provider"]; ok {
		payload["provider"] = provider
	}

	// A continuation resumes an answer; a thinking loop has no answer to resume, so it
	// is restarted instead: the conversation as the client sent it, with the thought
	// that looped left out and one instruction added. loopContinuable guarantees a
	// prefix for every channel that is continued, because a continuation with no
	// assistant turn to resume would ask the model to answer the question again inside
	// a response already on the wire.
	restart := verdict.Channel == loopChannelReasoning
	instruction := loopIntervention
	var turns []any
	if restart {
		instruction = loopRestart
	} else {
		turns = []any{
			map[string]any{"role": "assistant", "content": verdict.Prefix},
			map[string]any{"role": "user", "content": instruction},
		}
	}

	switch path {
	case "/v1/chat/completions", "/v1/messages":
		messages, ok := payload["messages"].([]any)
		if !ok || len(messages) == 0 {
			return nil, fmt.Errorf("loop: %s continuation body has no messages", path)
		}
		if restart {
			payload["messages"] = loopIntervene(messages, instruction, "text")
			break
		}
		payload["messages"] = append(messages, turns...)
	case "/v1/responses":
		// The Responses API takes its conversation as `input`, either as a string or
		// as a list of items. A string is turned into the one user turn it means.
		var input []any
		switch value := payload["input"].(type) {
		case string:
			input = []any{map[string]any{"role": "user", "content": value}}
		case []any:
			if len(value) == 0 {
				return nil, fmt.Errorf("loop: %s continuation body has no input", path)
			}
			input = value
		default:
			return nil, fmt.Errorf("loop: %s continuation body has no input", path)
		}
		if restart {
			payload["input"] = loopIntervene(input, instruction, "input_text")
			break
		}
		payload["input"] = append(input, turns...)
	}

	if path == "/v1/chat/completions" {
		payload["frequency_penalty"] = penalty
		payload["presence_penalty"] = penalty / 2
	}
	payload["stream"] = true
	// A continuation is a second request inside one client response, so it must not
	// return twice the output the client budgeted: the budget it asks for is what is
	// left of the client's, minus what this stream already generated.
	generated := loopTokensFor(len(verdict.Prefix))
	switch path {
	case "/v1/chat/completions":
		loopCapBudget(payload, "max_tokens", generated)
		loopCapBudget(payload, "max_completion_tokens", generated)
	case "/v1/messages":
		loopCapBudget(payload, "max_tokens", generated)
	case "/v1/responses":
		loopCapBudget(payload, "max_output_tokens", generated)
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, fmt.Errorf("loop: encode continuation body: %w", err)
	}
	return buf.Bytes(), nil
}

// loopIntervene appends the restart instruction to the conversation as the client
// sent it. When the last turn is the user's, the instruction joins that turn rather
// than opening a second user turn in a row: a restart has no assistant turn of orr's
// own to put between them, and some providers expect the roles to alternate. A turn
// whose content is in a shape this package does not rewrite keeps it: the instruction
// opens a user turn of its own instead, because replacing that content would delete
// part of the client's request.
func loopIntervene(turns []any, instruction, partType string) []any {
	if len(turns) > 0 {
		if last, ok := turns[len(turns)-1].(map[string]any); ok && last["role"] == "user" {
			if content, joined := loopAppendText(last["content"], instruction, partType); joined {
				last["content"] = content
				return turns
			}
		}
	}
	return append(turns, map[string]any{"role": "user", "content": instruction})
}

// loopAppendText appends a paragraph to a turn's content, which a protocol may send
// either as a string or as a list of typed parts named in that protocol's own way. ok
// is false for a shape this package must not rewrite.
func loopAppendText(content any, text, partType string) (appended any, ok bool) {
	switch value := content.(type) {
	case nil:
		return text, true
	case string:
		return value + "\n\n" + text, true
	case []any:
		return append(value, map[string]any{"type": partType, "text": text}), true
	}
	return content, false
}

// loopDecodeBody reads a request body into a mutable object. Numbers are kept as
// json.Number so a field this file does not touch — max_tokens, temperature — is
// re-emitted exactly as it arrived.
func loopDecodeBody(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("loop: continuation body: %w", err)
	}
	if payload == nil {
		return nil, errors.New("loop: continuation body must be a JSON object")
	}
	return payload, nil
}

// loopTokensFor is how many tokens a number of generated bytes is worth, rounded up.
// It is the same estimate everywhere one is needed, so a leg's cost, the count
// Messages reports, and the budget a continuation asks for cannot disagree.
func loopTokensFor(generated int) int {
	if generated <= 0 {
		return 0
	}
	return (generated + loopBytesPerToken - 1) / loopBytesPerToken
}

// loopCapBudget lowers one output budget in a continuation request by the tokens the
// response it continues has already generated. A budget left below the floor is raised
// back to it, but never past the client's own budget, and a field the client did not
// send is left alone: it had no budget to cap.
func loopCapBudget(payload map[string]any, field string, generated int) {
	raw, ok := payload[field]
	if !ok {
		return
	}
	number, ok := raw.(json.Number)
	if !ok {
		return
	}
	budget, err := number.Int64()
	if err != nil || budget <= 0 {
		return
	}
	left := int(budget) - generated
	if left < loopContinuationMinBudget {
		left = min(int(budget), loopContinuationMinBudget)
	}
	payload[field] = left
}

// continuationReframer turns the continuation stream into the tail of the response
// it continues. The second stream opens its own message, its own response and its own
// content block, and closes them again; the client already has all of that from the
// stream being continued, so Feed keeps only the frames the client is owed — the ones
// carrying generated text, the ones carrying a tool call's identity, and, on the
// Messages protocol, the block starts and stops the client's message needs — and
// drops the rest. Finish emits the closing sequence the client must see once, after
// the splice.
//
// The block the cut landed in is the block the client has open, so a leg-2 block index
// is mapped onto it: the continuation's first text block is that block under another
// name, and its start is dropped. A block the leg opens after it — a tool call, say —
// is mapped onto the next index the client has not seen and its start is passed on
// with that index, after the client's block is closed, so a continuation that answers
// with a tool call still leaves the client a well-formed message.
//
// Because the continuation's opening frames are dropped, Finish is what closes a block
// the leg left open, and it is where the client's response is terminated: never by a
// frame of the second leg.
//
// Frames are passed through byte for byte unless a content block index has to be
// rewritten, and a frame split across reads is reassembled first, so the splice cannot
// corrupt a delta. A path without a known delta shape passes everything straight
// through, so a caller never has to special-case it.
type continuationReframer struct {
	path string
	// enabled is false for a path this reframer has no frame shapes for.
	enabled bool

	pending []byte
	// emitted counts the bytes of generated text passed through, which is what the
	// token estimate is priced from.
	emitted  int
	finished bool

	// base is the index of the /v1/messages content block the client has open, which is
	// the block the continuation's own text continues.
	base int
	// next is the client index the next block the leg opens is mapped onto.
	next int
	// blocks maps a leg's content block index onto the client's.
	blocks map[int]int
	// open is the client block that is still open, loopNoBlock once a frame of the leg
	// closed it. The terminal owes a close only while one is open.
	open int
	// usage is what the leg measured of itself, and toolCalls whether the last piece of
	// content it produced was a tool call rather than prose.
	usage     loopUsage
	toolCalls bool
}

// newContinuationReframer returns a reframer for the streaming API at path, closing
// the content block a cut in the response's first block leaves open. The returned
// value must not be nil.
func newContinuationReframer(path string) *continuationReframer {
	return newContinuationReframerFor(path, 0)
}

// newContinuationReframerFor is newContinuationReframer for a response whose client
// has openBlock open: a thinking block, an earlier text block, or a tool call all push
// the block a cut lands in past block 0. A negative openBlock is read as 0, because a
// body that never showed a block start leaves the first block the only guess — and a
// cut in generated text does leave its block open.
func newContinuationReframerFor(path string, openBlock int) *continuationReframer {
	if openBlock < 0 {
		openBlock = 0
	}
	return &continuationReframer{
		path:    path,
		enabled: mutablePaths[path],
		base:    openBlock,
		next:    openBlock + 1,
		blocks:  make(map[int]int, 4),
		open:    openBlock,
	}
}

// Feed takes the next raw bytes of the continuation stream and returns the frames the
// client may see: its generated text, a tool call's identity, and the block starts and
// stops that protocol needs. The message_start, role delta, response.created and the
// closing frames of the second response are dropped, because the client's response is
// still open and will be closed once by Finish.
//
// Feed returns whole frames only. An incomplete trailing frame is held until the
// next read; a body that is not an event stream never completes a frame, so it is
// dropped rather than buffered past loopPendingBytes, and the caller's Finish still
// closes the response.
func (r *continuationReframer) Feed(chunk []byte) []byte {
	if r.finished {
		return nil
	}
	if !r.enabled {
		return chunk
	}
	var release []byte
	r.pending = append(r.pending, chunk...)
	consumed := 0
	for consumed < len(r.pending) {
		length := cutFrame(r.pending[consumed:])
		if length == 0 {
			break
		}
		frame := r.pending[consumed : consumed+length]
		consumed += length
		if out, text := r.reframe(frame); out != nil {
			r.emitted += text
			release = append(release, out...)
		}
	}
	if consumed > 0 {
		r.pending = append(r.pending[:0], r.pending[consumed:]...)
	}
	if len(r.pending) > loopPendingBytes {
		// Nothing in this body will ever complete a frame, so holding it only costs
		// memory: a continuation that answered with an error page is not spliced.
		r.pending = nil
	}
	return release
}

// Finish returns the closing frames of the spliced response, exactly once. A second
// call returns nothing, so a caller cannot close the client's response twice.
//
// The sequence itself is loopTerminalFor's, the one implementation the abort path
// writes too, so a protocol change lands in one place and the two paths cannot drift
// apart. What a splice adds is what only it knows: the content block the client is in,
// the usage the leg measured — which beats the byte estimate — and whether the answer
// ended in a tool call rather than prose.
func (r *continuationReframer) Finish() []byte {
	if r.finished {
		return nil
	}
	r.finished = true
	return loopTerminalFor(r.shape())
}

// shape is what the client's closing sequence reports about the splice.
func (r *continuationReframer) shape() loopTerminalShape {
	return loopTerminalShape{
		Path:      r.path,
		Block:     r.open,
		Output:    r.EstimatedOutputTokens(),
		Usage:     r.usage,
		ToolCalls: r.toolCalls,
	}
}

// Spliced reports whether any of the continuation reached the client: generated text,
// or a tool call whose identity the client is being handed. A leg that answered with
// neither — an error frame, an empty answer — leaves the response at the cut, and that
// is what the caller reports.
func (r *continuationReframer) Spliced() bool {
	return r.emitted > 0 || r.toolCalls
}

// EstimatedOutputTokens is the token count to account a continuation leg with when its
// usage frame never arrived, estimated from the text Feed passed through at roughly
// loopBytesPerToken bytes per token and rounded up. It is an estimate, not a
// measurement: it exists so a spliced leg is priced at all, and a leg that did report
// usage is accounted — and reported to the client — with its own numbers instead.
func (r *continuationReframer) EstimatedOutputTokens() int {
	return loopTokensFor(r.emitted)
}

// reframe decides what one frame of the continuation becomes on the client's wire. It
// returns the bytes to pass through — the frame itself when nothing in it changes —
// and how many bytes of generated text they carry, or nil for a frame that belongs to
// the continuation's own response and is dropped.
func (r *continuationReframer) reframe(frame []byte) ([]byte, int) {
	data := sseData(frame)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return nil, 0
	}
	switch r.path {
	case "/v1/chat/completions":
		return r.reframeChat(frame, data)
	case "/v1/responses":
		return r.reframeResponses(frame, data)
	case "/v1/messages":
		return r.reframeMessages(frame, data)
	}
	return nil, 0
}

func (r *continuationReframer) reframeChat(frame, data []byte) ([]byte, int) {
	var payload struct {
		Choices []struct {
			Delta struct {
				Content          json.RawMessage       `json:"content"`
				Reasoning        string                `json:"reasoning"`
				ReasoningContent string                `json:"reasoning_content"`
				ReasoningDetails []loopReasoningDetail `json:"reasoning_details"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, 0
	}
	// The frame that ends the leg carries its own usage, which is the measurement the
	// client's closing frame reports in place of the estimate.
	r.usage.noteChat(payload.Usage)
	var text strings.Builder
	call := false
	for _, choice := range payload.Choices {
		text.WriteString(contentText(choice.Delta.Content))
		text.WriteString(choice.Delta.ReasoningContent)
		text.WriteString(choice.Delta.Reasoning)
		for _, detail := range choice.Delta.ReasoningDetails {
			// An encrypted item carries nothing the client can read either, so a frame
			// holding only one generates no text and is dropped with the leg's own noise.
			text.WriteString(detail.text())
		}
		for _, each := range choice.Delta.ToolCalls {
			text.WriteString(each.Function.Arguments)
			call = true
		}
	}
	switch {
	case call:
		// Passing these on is what makes a spliced tool call usable: the frame that
		// opens the call carries its id and name with no arguments yet, so it generates
		// no text and the client has no other way to learn which call it was handed.
		r.toolCalls = true
		return frame, text.Len()
	case text.Len() > 0:
		r.toolCalls = false
		return frame, text.Len()
	}
	// The leg's own finish frame carries the reason it ended for and no text. The
	// terminal writes the reason the client sees, so the frame itself is dropped and only
	// its reason is remembered.
	for _, choice := range payload.Choices {
		if choice.FinishReason == "tool_calls" {
			r.toolCalls = true
		}
	}
	return nil, 0
}

func (r *continuationReframer) reframeResponses(frame, data []byte) ([]byte, int) {
	var payload struct {
		Type  string          `json:"type"`
		Delta string          `json:"delta"`
		Item  json.RawMessage `json:"item"`
		Usage json.RawMessage `json:"usage"`
		// The completed event nests the response, and the usage with it.
		Response struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, 0
	}
	switch payload.Type {
	case "response.output_text.delta":
		if payload.Delta == "" {
			return nil, 0
		}
		r.toolCalls = false
		return frame, len(payload.Delta)
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if payload.Delta == "" {
			return nil, 0
		}
		return frame, len(payload.Delta)
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		r.toolCalls = true
		return frame, len(payload.Delta)
	case "response.output_item.added", "response.output_item.done":
		// An item frame rewrites the client's response to hold an item it never asked
		// for. A text item belongs to the continuation's own response and goes with the
		// rest; the function call is the one item the client is being handed.
		if !loopFunctionCallItem(payload.Item) {
			return nil, 0
		}
		r.toolCalls = true
		return frame, 0
	case "response.completed":
		// The Responses API nests its usage in the response object; a usage at the top
		// level is all some providers send, so it is read when the nested one is absent.
		if loopUsagePresent(payload.Response.Usage) {
			r.usage.noteResponses(payload.Response.Usage)
		} else {
			r.usage.noteResponses(payload.Usage)
		}
		return nil, 0
	}
	return nil, 0
}

func (r *continuationReframer) reframeMessages(frame, data []byte) ([]byte, int) {
	var payload struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Block struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, 0
	}
	switch payload.Type {
	case "content_block_start":
		client, known := r.blocks[payload.Index]
		if known {
			// A block the leg opened twice: the client already has it open, and a second
			// start would open a block that is open.
			return nil, 0
		}
		if r.open == r.base && payload.Block.Type == "text" {
			// The leg's first text block is the prose the client's open block is in the
			// middle of. The client's own start is the start it has; this one only
			// renames the same block.
			r.blocks[payload.Index] = r.base
			return nil, 0
		}
		client = r.next
		r.next++
		r.blocks[payload.Index] = client
		r.toolCalls = payload.Block.Type == "tool_use"
		release := r.closeOpenBlock()
		r.open = client
		return append(release, rewriteIndex(frame, client)...), 0
	case "content_block_stop":
		client, known := r.blocks[payload.Index]
		if !known {
			// A stop for a block whose start never arrived is the block the continuation
			// was written into.
			client = r.open
			if client == loopNoBlock {
				return nil, 0
			}
			r.blocks[payload.Index] = client
		}
		if client == r.open {
			r.open = loopNoBlock
		}
		return rewriteIndex(frame, client), 0
	case "content_block_delta":
		client, known := r.blocks[payload.Index]
		if !known {
			client = r.open
			if client == loopNoBlock {
				return nil, 0
			}
			r.blocks[payload.Index] = client
		}
		text := ""
		switch payload.Delta.Type {
		case "text_delta":
			text = payload.Delta.Text
			if text != "" {
				r.toolCalls = false
			}
		case "thinking_delta":
			text = payload.Delta.Thinking
		case "input_json_delta":
			text = payload.Delta.PartialJSON
			if text != "" {
				r.toolCalls = true
			}
		}
		if text == "" {
			return nil, 0
		}
		return rewriteIndex(frame, client), len(text)
	case "message_delta":
		// The leg's usage is the only measurement of what it generated; its stop reason
		// is not the client's, because the terminal names the one the splice produced.
		r.usage.noteMessages(payload.Usage)
		return nil, 0
	}
	return nil, 0
}

// closeOpenBlock closes the client's open block before the leg opens one of its own. A
// block cannot start while another is open, and the block the cut landed in is only one
// of the blocks a message can hold.
func (r *continuationReframer) closeOpenBlock() []byte {
	if r.open == loopNoBlock {
		return nil
	}
	stopped := fmt.Appendf(nil, "event: content_block_stop\n"+
		"data: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", r.open)
	r.open = loopNoBlock
	return stopped
}

// rewriteIndex returns frame with its content block index set to index. A frame whose
// index is already the client's is passed through byte for byte; only a remapped index
// pays for a re-encode, and the payload's other bytes are carried through untouched.
func rewriteIndex(frame []byte, index int) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(sseData(frame), &payload) != nil {
		return frame
	}
	var current int
	if raw, ok := payload["index"]; ok && json.Unmarshal(raw, &current) == nil && current == index {
		return frame
	}
	payload["index"] = fmt.Appendf(nil, "%d", index)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return frame
	}
	var event string
	_ = json.Unmarshal(payload["type"], &event)
	var out []byte
	if event != "" {
		out = fmt.Appendf(nil, "event: %s\n", event)
	}
	return fmt.Appendf(out, "data: %s\n\n", encoded)
}

// loopFunctionCallItem reports whether an output item is a function call, which is the
// one item a spliced continuation may add to the client's response.
func loopFunctionCallItem(raw json.RawMessage) bool {
	var item struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &item) == nil && item.Type == "function_call"
}

// loopUsage is what a continuation leg measured of itself. A leg that ended before its
// usage frame arrived leaves every field zero with Reported false, and the byte
// estimate stands in for its output count instead.
type loopUsage struct {
	// Reported is true when the leg sent a usage frame. Its counts are then what the
	// client is told wherever the protocol can carry them, because a measurement beats
	// an estimate.
	Reported bool
	// Input is the leg's prompt or input tokens, 0 when it did not report them.
	Input int
	// Output is the leg's generated tokens.
	Output int
	// Total is the leg's own sum, 0 when it did not report one.
	Total int
}

// withEstimate is the usage to report for a leg that generated output tokens' worth of
// text: the leg's own figures when it reported any, the estimate otherwise.
func (u loopUsage) withEstimate(output int) loopUsage {
	if u.Reported {
		return u
	}
	if output <= 0 {
		return loopUsage{}
	}
	return loopUsage{Output: output, Total: output}
}

// loopUsagePresent reports whether a frame carried a usage object at all: an absent or
// null usage is no measurement, not a measurement of zero.
func loopUsagePresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// note records what one usage object said, unless the frame carried none.
func (u *loopUsage) note(raw json.RawMessage, input, output, total int) {
	if u == nil || !loopUsagePresent(raw) {
		return
	}
	u.Reported, u.Input, u.Output, u.Total = true, input, output, total
}

// noteChat reads the usage object of a chat completions frame.
func (u *loopUsage) noteChat(raw json.RawMessage) {
	var usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return
	}
	u.note(raw, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
}

// noteResponses reads a usage object whose counts the Responses API names input and
// output.
func (u *loopUsage) noteResponses(raw json.RawMessage) {
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return
	}
	u.note(raw, usage.InputTokens, usage.OutputTokens, usage.TotalTokens)
}

// noteMessages reads the usage object of a messages frame, which carries no total.
func (u *loopUsage) noteMessages(raw json.RawMessage) {
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return
	}
	u.note(raw, usage.InputTokens, usage.OutputTokens, 0)
}

// loopTerminalShape is everything a protocol's closing sequence can carry: the
// generated-token count Messages reports, the content block that protocol still has
// open, the usage a continuation leg measured, and whether the answer ended in a tool
// call rather than prose.
type loopTerminalShape struct {
	// Path is the streaming API the client called.
	Path string
	// Block is the /v1/messages content block a close is owed for. Its zero value is
	// block 0, which is what a cut in the response's first block leaves open;
	// loopNoBlock says the client has no block open and no stop is owed. The chat and
	// Responses sequences have no blocks and ignore it.
	Block int
	// Output is the estimated generated-token count. Messages reports it, and the chat
	// and Responses usage objects fall back to it when the leg measured nothing.
	Output int
	// Usage is what the continuation leg reported of itself.
	Usage loopUsage
	// ToolCalls reports that the spliced content ended in a tool call, so the closing
	// frame names that reason instead of claiming the model stopped.
	ToolCalls bool
}

// loopNoBlock is the shape's way of saying the client has no content block open: a
// continuation whose own frames already closed the block it continued owes no second
// close.
const loopNoBlock = -1

// loopTerminal is loopTerminalFor with nothing but the generated-token count to report,
// which is all the abort path has: no block is tracked, so block 0 is closed, and a
// zero count carries no usage object at all.
func loopTerminal(path string, outputTokens int) []byte {
	return loopTerminalFor(loopTerminalShape{Path: path, Output: outputTokens})
}

// loopTerminalFor is the protocol-correct end of a stream that was cut at a loop, and
// the only place those sequences exist: the abort path and the recovery path both end a
// response here, so a protocol change cannot leave one copy behind. What each protocol
// can report about the spliced answer goes in it: the usage a continuation leg measured
// in the chat and Responses closing frames, and the generated-token count plus the
// block the client is in for Messages.
//
// The client is told the response finished rather than left waiting for frames that
// will never arrive: chat completions get a finish_reason chunk and its done sentinel,
// the Responses API its completed event, and the Messages API the block stop, message
// delta, and message stop that close a message.
func loopTerminalFor(shape loopTerminalShape) []byte {
	switch shape.Path {
	case "/v1/chat/completions":
		return shape.chatTerminal()
	case "/v1/responses":
		return shape.responsesTerminal()
	case "/v1/messages":
		return shape.messagesTerminal()
	}
	return nil
}

func (s loopTerminalShape) chatTerminal() []byte {
	reason := "stop"
	if s.ToolCalls {
		reason = "tool_calls"
	}
	frame := fmt.Appendf(nil, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"%s"}]`, reason)
	if usage := s.usageFields("prompt_tokens", "completion_tokens"); usage != "" {
		frame = fmt.Appendf(frame, `,"usage":{%s}`, usage)
	}
	return append(frame, "}\n\ndata: [DONE]\n\n"...)
}

func (s loopTerminalShape) responsesTerminal() []byte {
	frame := []byte("event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"")
	if usage := s.usageFields("input_tokens", "output_tokens"); usage != "" {
		frame = fmt.Appendf(frame, `,"usage":{%s}`, usage)
	}
	return append(frame, "}}\n\n"...)
}

func (s loopTerminalShape) messagesTerminal() []byte {
	reason := "end_turn"
	if s.ToolCalls {
		reason = "tool_use"
	}
	usage := s.Usage.withEstimate(s.Output)
	var frame []byte
	if s.Block != loopNoBlock {
		frame = fmt.Appendf(nil, "event: content_block_stop\n"+
			"data: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", s.Block)
	}
	return fmt.Appendf(frame, "event: message_delta\n"+
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"%s\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":%d}}\n\n"+
		"event: message_stop\n"+
		"data: {\"type\":\"message_stop\"}\n\n", reason, usage.Output)
}

// usageFields is the inside of a usage object for a protocol whose counts are named
// inputKey and outputKey: the leg's own figures when it reported them, the output
// estimate when it did not, and nothing at all when there is no figure to report. An
// unreported input count is left out rather than sent as zero, which would claim the
// continuation read no prompt.
func (s loopTerminalShape) usageFields(inputKey, outputKey string) string {
	usage := s.Usage.withEstimate(s.Output)
	if usage.Output <= 0 {
		return ""
	}
	var fields []string
	if usage.Input > 0 {
		fields = append(fields, fmt.Sprintf(`"%s":%d`, inputKey, usage.Input))
	}
	fields = append(fields, fmt.Sprintf(`"%s":%d`, outputKey, usage.Output))
	total := usage.Total
	if total <= 0 {
		total = usage.Input + usage.Output
	}
	fields = append(fields, fmt.Sprintf(`"total_tokens":%d`, total))
	return strings.Join(fields, ",")
}

// loopBlockTracker follows the /v1/messages content block the client has open, from the
// frames the stage actually released. A cut has to close the block the client is
// inside, and that is not always block 0: a thinking block, a tool call, or an answer
// that closed a block before it started looping all move the open block past it.
type loopBlockTracker struct {
	pending []byte
	// open is the index of the block the client has open, valid while isOpen.
	open   int
	isOpen bool
	// seen is whether any block frame arrived at all. A body that never showed one
	// leaves block 0 the only guess, which is what the terminal does without a tracker.
	seen bool
}

// scan folds the next released bytes into the tracker, reassembling a frame split
// across writes before reading it.
func (t *loopBlockTracker) scan(chunk []byte) {
	t.pending = append(t.pending, chunk...)
	consumed := 0
	for consumed < len(t.pending) {
		length := cutFrame(t.pending[consumed:])
		if length == 0 {
			break
		}
		t.frame(t.pending[consumed : consumed+length])
		consumed += length
	}
	if consumed > 0 {
		t.pending = append(t.pending[:0], t.pending[consumed:]...)
	}
	if len(t.pending) > loopPendingBytes {
		// A body whose frames never complete is not a stream this tracker can follow, and
		// holding it would only cost memory.
		t.pending = nil
	}
}

// frame reads one whole frame for a block that opened or closed.
func (t *loopBlockTracker) frame(frame []byte) {
	data := sseData(frame)
	if len(data) == 0 {
		return
	}
	var payload struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	switch payload.Type {
	case "content_block_start":
		t.open, t.isOpen, t.seen = payload.Index, true, true
	case "content_block_stop":
		if t.isOpen && t.open == payload.Index {
			t.isOpen = false
		}
		t.seen = true
	}
}

// block is the index the client's closing sequence has to close: the open block, or
// loopNoBlock when the stream already closed it.
func (t *loopBlockTracker) block() int {
	if !t.seen {
		return 0
	}
	if !t.isOpen {
		return loopNoBlock
	}
	return t.open
}
