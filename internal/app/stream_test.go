package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// streamBody is an upstream body a stage reads from: the chunks it serves, the
// error it ends with, and whether the stage closed it early.
type streamBody struct {
	chunks [][]byte
	index  int
	err    error
	closed bool
}

func (b *streamBody) Read(p []byte) (int, error) {
	if b.index >= len(b.chunks) {
		if b.err != nil {
			return 0, b.err
		}
		return 0, io.EOF
	}
	n := copy(p, b.chunks[b.index])
	b.index++
	return n, nil
}

func (b *streamBody) Close() error {
	b.closed = true
	return nil
}

// streamRecorder is the client side of a stage: what it wrote and how often it
// flushed.
type streamRecorder struct {
	written bytes.Buffer
	writes  int
	flushes int
}

func (r *streamRecorder) Write(p []byte) (int, error) {
	r.writes++
	return r.written.Write(p)
}

func (r *streamRecorder) Flush() { r.flushes++ }

// streamConfig is a loop configuration with the shipped thresholds and the
// mode under test.
func streamConfig(mode loopMode) loopConfig {
	return loopConfig{
		Mode:             mode,
		MinPeriod:        defaultLoopMinPeriod,
		MinRepeat:        defaultLoopMinRepeat,
		MaxContinuations: defaultLoopMaxContinuations,
		Penalty:          defaultLoopPenalty,
	}
}

// streamStageFor builds a stage over body for one response of the given content
// type, and returns the recorder it writes to.
func streamStageFor(path string, cfg loopConfig, contentType string, body io.ReadCloser) (*streamStage, *streamRecorder) {
	recorder := &streamRecorder{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       body,
	}
	return newStreamStage(path, cfg, resp, recorder, nil), recorder
}

// streamChunks is a body served the way the proxy's read buffer serves it:
// chunks of at most size bytes, cut wherever the buffer ends.
func streamChunks(body string, size int) [][]byte {
	var chunks [][]byte
	for _, chunk := range loopSplit(body, size) {
		chunks = append(chunks, []byte(chunk))
	}
	return chunks
}

// streamFrameBuilder is the delta frame shape of one generation path.
func streamFrameBuilder(path string) func(string) string {
	switch path {
	case "/v1/responses":
		return loopResponsesFrame
	case "/v1/messages":
		return streamMessagesFrame
	default:
		return loopChatFrame
	}
}

// streamMessagesFrame is one /v1/messages text delta.
func streamMessagesFrame(text string) string {
	return `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` +
		loopQuoted(text) + `}}` + "\n\n"
}

// streamTerminal is the event each protocol's closing sequence is written under. A test
// that does not care which fields the closing frame carries locates it with this; one
// that does compares against loopTerminalFor.
func streamTerminal(path string) string {
	switch path {
	case "/v1/responses":
		return "event: response.completed"
	case "/v1/messages":
		return "event: message_stop"
	}
	return "data: [DONE]"
}

// streamClosed reports whether the one closing frame the protocol needs is the last frame
// of out: exactly one of them, and nothing after it.
func streamClosed(out, path string) bool {
	marker := streamTerminal(path)
	if strings.Count(out, marker) != 1 {
		return false
	}
	frames := strings.Split(strings.TrimSuffix(out, "\n\n"), "\n\n")
	return strings.Contains(frames[len(frames)-1], marker)
}

// streamLoopBody starts normally and then repeats one line for ever, in the
// frame shape of path. The line carries no character JSON has to escape, so it
// appears verbatim in a frame and can be counted in what the client received.
func streamLoopBody(path string) (chunks []string, line string) {
	frame := streamFrameBuilder(path)
	line = "The answer stays the same because the reasoning keeps circling back. "
	chunks = append(chunks, frame("Here is the summary you asked for. "))
	for range 40 {
		chunks = append(chunks, frame(line))
	}
	return chunks, line
}

// streamCleanBody is an ordinary answer that never repeats itself, ending the
// way each protocol ends a message.
func streamCleanBody(path string) []string {
	frame := streamFrameBuilder(path)
	chunks := []string{frame("An ordinary answer that never repeats itself.\n")}
	for i := range 40 {
		chunks = append(chunks, frame(fmt.Sprintf("Sentence %d says something new about point %d.\n", i, i*i+3)))
	}
	switch path {
	case "/v1/responses":
		chunks = append(chunks, `event: response.completed`+"\n"+`data: {"type":"response.completed"}`+"\n\n")
	case "/v1/messages":
		chunks = append(chunks,
			`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`+"\n\n",
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`+"\n\n")
	default:
		chunks = append(chunks,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n",
			"data: [DONE]\n\n")
	}
	return chunks
}

// streamStrings is chunks as one body.
func streamStrings(chunks []string) string { return strings.Join(chunks, "") }

// streamContinuationText is the text a fake continuation leg generates. It is the
// proof a splice landed: the client must see it after the cut.
const streamContinuationText = "The answer picks itself back up here. "

// streamContinuation is what a fake continuation leg answers with: the frames that
// open a response of its own, some generated text, its own usage frame, and its own
// terminal sequence. The client must see only the text — everything else belongs to
// a response it never opened, and the sequence that closes the message is written by
// the stage instead, exactly once.
func streamContinuation(path string) string {
	frame := streamFrameBuilder(path)
	var body strings.Builder
	switch path {
	case "/v1/messages":
		body.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"role\":\"assistant\"}}\n\n")
		body.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	case "/v1/responses":
		body.WriteString("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n")
	default:
		body.WriteString(`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n")
	}
	body.WriteString(frame(streamContinuationText))
	body.WriteString(frame("A second sentence carries the answer on. "))
	switch path {
	case "/v1/messages":
		body.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":17}}\n\n")
		body.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	case "/v1/responses":
		body.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	default:
		body.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":17,"cost":0.002}}` + "\n\n")
		body.WriteString("data: [DONE]\n\n")
	}
	return body.String()
}

// streamContinuationOnly is what a continuation leg's own response adds and the
// client must never see twice: its identity, its opening, and its closing frames.
func streamContinuationOnly(path string) []string {
	switch path {
	case "/v1/messages":
		return []string{"msg_2", `"type":"message_start"`}
	case "/v1/responses":
		return []string{`"type":"response.created"`, `"status":"in_progress"`}
	default:
		return []string{`"role":"assistant"`}
	}
}

// streamSpliceStage builds a recover-mode stage over body whose continuation is
// sent by send, with a keepalive interval short enough for a test to observe.
func streamSpliceStage(path string, cfg loopConfig, body io.ReadCloser, send func(*loopVerdict) (io.ReadCloser, error)) (*streamStage, *streamRecorder) {
	stage, recorder := streamStageFor(path, cfg, "text/event-stream", body)
	stage.continuation = send
	stage.keepalive = time.Millisecond
	return stage, recorder
}

// streamAnswered is a continuation sender that answers with body every time and
// counts how often it was asked, so a test can tell one splice from a chain.
func streamAnswered(body string, asked *int) func(*loopVerdict) (io.ReadCloser, error) {
	return func(*loopVerdict) (io.ReadCloser, error) {
		*asked = *asked + 1
		return io.NopCloser(strings.NewReader(body)), nil
	}
}

// streamSignalSink is a client that reports when a keepalive comment reached it.
type streamSignalSink struct {
	streamRecorder
	keepalives chan struct{}
	once       sync.Once
}

func (s *streamSignalSink) Write(p []byte) (int, error) {
	if bytes.Contains(p, streamKeepalive) {
		s.once.Do(func() { close(s.keepalives) })
	}
	return s.streamRecorder.Write(p)
}

func TestStreamStageOffModeBuildsNoInspector(t *testing.T) {
	stage, recorder := streamStageFor("/v1/chat/completions", streamConfig(loopModeOff),
		"text/event-stream", &streamBody{chunks: streamChunks(streamStrings(streamCleanBody("/v1/chat/completions")), 512)})
	if stage.inspector != nil {
		t.Fatal("off mode built an inspector")
	}
	if _, err := stage.Run(); err != nil {
		t.Fatal(err)
	}
	if recorder.written.String() != streamStrings(streamCleanBody("/v1/chat/completions")) {
		t.Fatal("off mode changed the bytes on the wire")
	}
}

func TestStreamStageCleanStreamIsByteIdentical(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, mode := range []loopMode{loopModeOff, loopModeObserve, loopModeAbort, loopModeRecover} {
			chunks := streamCleanBody(path)
			full := streamStrings(chunks)
			body := &streamBody{chunks: streamChunks(full, 1024)}
			stage, recorder := streamStageFor(path, streamConfig(mode), "text/event-stream", body)
			verdict, err := stage.Run()
			if err != nil {
				t.Fatalf("%s %s: %v", path, mode, err)
			}
			if verdict != nil {
				t.Fatalf("%s %s: verdict on a clean stream", path, mode)
			}
			if recorder.written.String() != full {
				t.Fatalf("%s %s: released %d bytes, want all %d", path, mode, recorder.written.Len(), len(full))
			}
			if body.closed {
				t.Fatalf("%s %s: closed a body that ran to EOF", path, mode)
			}
			if recorder.flushes != recorder.writes || recorder.writes == 0 {
				t.Fatalf("%s %s: %d writes but %d flushes, want one flush per write", path, mode, recorder.writes, recorder.flushes)
			}
		}
	}
}

func TestStreamStageCleanStreamFlushesEveryFrame(t *testing.T) {
	chunks := streamCleanBody("/v1/chat/completions")
	body := &streamBody{chunks: streamChunks(streamStrings(chunks), 1024)}
	stage, recorder := streamStageFor("/v1/chat/completions", streamConfig(loopModeObserve), "text/event-stream", body)
	if _, err := stage.Run(); err != nil {
		t.Fatal(err)
	}
	// Nothing is withheld on a clean stream, so each read is written and flushed
	// once: the client sees the same cadence it saw before the stage existed.
	if recorder.writes != len(body.chunks) || recorder.flushes != len(body.chunks) {
		t.Fatalf("%d writes and %d flushes for %d reads, want one each", recorder.writes, recorder.flushes, len(body.chunks))
	}
}

// The trim must not leave half a rune behind: a dropped byte count is a text length,
// not necessarily a rune boundary.
func TestTrimDroppedPrefixKeepsRunesWhole(t *testing.T) {
	for _, tc := range []struct {
		prefix  string
		dropped int
		want    string
	}{
		{"abc", 0, "abc"},
		{"abc", 1, "ab"},
		{"abc", 9, ""},
		{"aé", 1, "a"}, // é is two bytes; one byte back lands mid-rune
		{"aé", 2, "a"}, // the whole rune
		{"aé", 3, ""},
	} {
		if got := trimDroppedPrefix(tc.prefix, tc.dropped); got != tc.want {
			t.Fatalf("trimDroppedPrefix(%q, %d) = %q, want %q", tc.prefix, tc.dropped, got, tc.want)
		}
	}
}

func TestStreamStageAbortEmitsTerminalForEachProtocol(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		chunks, line := streamLoopBody(path)
		full := streamStrings(chunks)
		body := &streamBody{chunks: streamChunks(full, 512)}
		stage, recorder := streamStageFor(path, streamConfig(loopModeAbort), "text/event-stream", body)
		verdict, err := stage.Run()
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if verdict == nil {
			t.Fatalf("%s: no verdict for a looping stream", path)
		}
		loopCheckVerdictShape(t, verdict, len(line))
		terminal := string(loopTerminal(path, 0))
		if terminal == "" {
			t.Fatalf("%s: no terminal sequence", path)
		}
		out := recorder.written.String()
		if !strings.HasSuffix(out, terminal) {
			t.Fatalf("%s: output does not end with %q", path, terminal)
		}
		if count := strings.Count(out, terminal); count != 1 {
			t.Fatalf("%s: %d terminal sequences, want exactly one", path, count)
		}
		if count := strings.Count(out, line); count == 0 || count >= 20 {
			t.Fatalf("%s: the cut landed after %d repetitions, want it early but after the loop began", path, count)
		}
		if len(out) >= len(full)/2 {
			t.Fatalf("%s: client received %d of %d bytes, want the loop cut short", path, len(out), len(full))
		}
		if !body.closed {
			t.Fatalf("%s: the cut left the upstream body open", path)
		}
	}
}

func TestStreamStageChatTerminalClosesTheStream(t *testing.T) {
	chunks, _ := streamLoopBody("/v1/chat/completions")
	stage, recorder := streamStageFor("/v1/chat/completions", streamConfig(loopModeAbort),
		"text/event-stream", &streamBody{chunks: streamChunks(streamStrings(chunks), 512)})
	if _, err := stage.Run(); err != nil {
		t.Fatal(err)
	}
	out := recorder.written.String()
	if count := strings.Count(out, "data: [DONE]"); count != 1 {
		t.Fatalf("%d done sentinels, want one", count)
	}
	if count := strings.Count(out, `"finish_reason":"stop"`); count != 1 {
		t.Fatalf("%d finish_reason frames, want one", count)
	}
}

func TestStreamStageMessagesTerminalEndsTheTurn(t *testing.T) {
	chunks, _ := streamLoopBody("/v1/messages")
	stage, recorder := streamStageFor("/v1/messages", streamConfig(loopModeAbort),
		"text/event-stream", &streamBody{chunks: streamChunks(streamStrings(chunks), 512)})
	if _, err := stage.Run(); err != nil {
		t.Fatal(err)
	}
	out := recorder.written.String()
	for _, want := range []string{"event: content_block_stop", "event: message_delta", `"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("terminal sequence is missing %q", want)
		}
	}
	if strings.Count(out, "event: message_stop") != 1 {
		t.Fatalf("%d message_stop events, want one", strings.Count(out, "event: message_stop"))
	}
}

func TestStreamStageObserveForwardsEveryByte(t *testing.T) {
	chunks, line := streamLoopBody("/v1/chat/completions")
	full := streamStrings(chunks)
	body := &streamBody{chunks: streamChunks(full, 512)}
	stage, recorder := streamStageFor("/v1/chat/completions", streamConfig(loopModeObserve), "text/event-stream", body)
	verdict, err := stage.Run()
	if err != nil {
		t.Fatal(err)
	}
	if verdict == nil {
		t.Fatal("no verdict in observe mode")
	}
	loopCheckVerdictShape(t, verdict, len(line))
	if recorder.written.String() != full {
		t.Fatalf("observe released %d bytes, want all %d", recorder.written.Len(), len(full))
	}
	if body.closed {
		t.Fatal("observe mode closed the upstream body")
	}
	if strings.Count(recorder.written.String(), line) != 40 {
		t.Fatalf("observe delivered %d repetitions, want all 40", strings.Count(recorder.written.String(), line))
	}
}

func TestStreamStageRecoverSplicesAContinuation(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			chunks, line := streamLoopBody(path)
			full := streamStrings(chunks)
			body := &streamBody{chunks: streamChunks(full, 512)}
			var asked int
			stage, recorder := streamSpliceStage(path, streamConfig(loopModeRecover), body,
				streamAnswered(streamContinuation(path), &asked))

			verdict, err := stage.Run()
			if err != nil {
				t.Fatal(err)
			}
			if verdict == nil {
				t.Fatal("no verdict for a looping stream")
			}
			if asked != 1 {
				t.Fatalf("asked for %d continuations, want exactly one", asked)
			}
			out := recorder.written.String()
			// One closing sequence reaches the client, and it is the one the stage
			// writes: the aborted leg's own finish frame never got through.
			if !streamClosed(out, path) {
				t.Fatalf("the spliced response is not closed once by %q:\n%q", streamTerminal(path), out[max(0, len(out)-200):])
			}
			// The leg measured its own generation, and the closing frame reports that
			// measurement rather than the estimate of the spliced text.
			if path == "/v1/chat/completions" && !strings.Contains(out, `"completion_tokens":17`) {
				t.Fatal("the closing frame does not report the continuation's own usage")
			}
			if path == "/v1/messages" && !strings.Contains(out, `"output_tokens":17`) {
				t.Fatal("the closing frame does not report the continuation's own output tokens")
			}
			if !strings.Contains(out, streamContinuationText) {
				t.Fatal("the continuation's text never reached the client")
			}
			for _, foreign := range streamContinuationOnly(path) {
				if strings.Contains(out, foreign) {
					t.Fatalf("the client saw %q, which belongs to the continuation's own response", foreign)
				}
			}
			if stage.recovery == nil || stage.recovery.Splices != 1 {
				t.Fatalf("recovery = %+v, want one splice", stage.recovery)
			}
			if stage.recovery.EstimatedTokens == 0 {
				t.Fatal("no token estimate for the spliced text")
			}
			if !strings.Contains(string(stage.recoveryTail), streamContinuationText) {
				t.Fatal("the continuation's own bytes are not in the tail its cost is read from")
			}
			if path == "/v1/chat/completions" && !strings.Contains(string(stage.recoveryTail), `"completion_tokens":17`) {
				t.Fatal("the continuation's usage frame is not in the tail its cost is read from")
			}
			if count := strings.Count(out, line); count == 0 || count >= 20 {
				t.Fatalf("the cut landed after %d repetitions, want it early but after the loop began", count)
			}
			if !body.closed {
				t.Fatal("the cut left the upstream body open")
			}
		})
	}
}

func TestStreamStageRecoverKeepsTheClientAliveDuringTheContinuation(t *testing.T) {
	chunks, _ := streamLoopBody("/v1/chat/completions")
	sink := &streamSignalSink{keepalives: make(chan struct{})}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       &streamBody{chunks: streamChunks(streamStrings(chunks), 512)},
	}
	stage := newStreamStage("/v1/chat/completions", streamConfig(loopModeRecover), resp, sink, nil)
	stage.keepalive = time.Millisecond
	// The continuation answers only once the client has been told the stream is
	// alive, so this proves the comment goes out while the request is in flight
	// rather than after it.
	stage.continuation = func(*loopVerdict) (io.ReadCloser, error) {
		select {
		case <-sink.keepalives:
		case <-time.After(5 * time.Second):
			return nil, errors.New("no keepalive reached the client")
		}
		return io.NopCloser(strings.NewReader(streamContinuation("/v1/chat/completions"))), nil
	}
	if _, err := stage.Run(); err != nil {
		t.Fatal(err)
	}
	out := sink.written.String()
	if count := strings.Count(out, string(streamKeepalive)); count == 0 {
		t.Fatal("no keepalive comment reached the client while the continuation was in flight")
	}
	// A keepalive is a comment, never an empty frame: a client parsing frames would
	// have to make sense of one that carries nothing.
	for _, empty := range []string{"data: \n\n", "data:\n\n", "data: \n"} {
		if strings.Contains(out, empty) {
			t.Fatalf("an empty data frame reached the client: %q", empty)
		}
	}
	if keep, text := strings.Index(out, string(streamKeepalive)), strings.Index(out, streamContinuationText); keep < 0 || keep > text {
		t.Fatalf("keepalive at %d and continuation text at %d, want the comment first", keep, text)
	}
	if !streamClosed(out, "/v1/chat/completions") {
		t.Fatal("the spliced response does not end with the terminal sequence")
	}
}

func TestStreamStageRecoverRespectsTheContinuationAllowance(t *testing.T) {
	chunks, _ := streamLoopBody("/v1/chat/completions")
	cfg := streamConfig(loopModeRecover)
	// Zero continuations is a configured answer, not a failure: the cut is where the
	// response ends.
	cfg.MaxContinuations = 0
	var asked int
	stage, recorder := streamSpliceStage("/v1/chat/completions", cfg,
		&streamBody{chunks: streamChunks(streamStrings(chunks), 512)},
		streamAnswered(streamContinuation("/v1/chat/completions"), &asked))

	verdict, err := stage.Run()
	if err != nil {
		t.Fatal(err)
	}
	if verdict == nil {
		t.Fatal("no verdict for a looping stream")
	}
	if asked != 0 {
		t.Fatalf("asked for %d continuations with none allowed", asked)
	}
	out := recorder.written.String()
	if !strings.HasSuffix(out, string(loopTerminal("/v1/chat/completions", 0))) {
		t.Fatal("the cut response does not end with the terminal sequence")
	}
	if strings.Contains(out, streamContinuationText) {
		t.Fatal("a continuation was spliced in with none allowed")
	}
}

func TestStreamStageRecoverEndsTheAnswerWhenTheContinuationLoops(t *testing.T) {
	chunks, _ := streamLoopBody("/v1/chat/completions")
	cfg := streamConfig(loopModeRecover)
	// The cap allows three legs, and the continuation still repeats itself. Asking it
	// to continue again would spend a request to arrive back at the same verdict, so
	// the answer ends at the second cut instead.
	cfg.MaxContinuations = 3
	legChunks, _ := streamLoopBody("/v1/chat/completions")
	var asked int
	stage, recorder := streamSpliceStage("/v1/chat/completions", cfg,
		&streamBody{chunks: streamChunks(streamStrings(chunks), 512)},
		streamAnswered(streamStrings(legChunks), &asked))

	if _, err := stage.Run(); err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Fatalf("asked for %d continuations, want the repeating one to end the answer", asked)
	}
	out := recorder.written.String()
	if !streamClosed(out, "/v1/chat/completions") {
		t.Fatalf("the response is not closed once by the terminal sequence:\n%q", out[max(0, len(out)-200):])
	}
	if stage.recovery.Splices != 1 {
		t.Fatalf("splices = %d, want the text that was spliced before the repeat", stage.recovery.Splices)
	}
	if !strings.Contains(stage.recovery.Reason, "repeated") {
		t.Fatalf("reason = %q, want the repetition named", stage.recovery.Reason)
	}
}

func TestStreamStageRecoverEndsTheAnswerWhenTheContinuationFails(t *testing.T) {
	for _, failure := range []string{
		"continuation answered 429",
		"continuation answered 403",
		"continuation request failed",
		"continuation is not an event stream",
	} {
		t.Run(failure, func(t *testing.T) {
			chunks, line := streamLoopBody("/v1/chat/completions")
			var asked int
			stage, recorder := streamSpliceStage("/v1/chat/completions", streamConfig(loopModeRecover),
				&streamBody{chunks: streamChunks(streamStrings(chunks), 512)},
				func(*loopVerdict) (io.ReadCloser, error) {
					asked++
					return nil, errors.New(failure)
				})

			if _, err := stage.Run(); err != nil {
				t.Fatal(err)
			}
			if asked != 1 {
				t.Fatalf("asked for %d continuations, want one attempt", asked)
			}
			out := recorder.written.String()
			// The client still gets one valid terminator, and nothing of a response
			// that never arrived.
			terminal := string(loopTerminal("/v1/chat/completions", 0))
			if out != "" && !strings.HasSuffix(out, terminal) {
				t.Fatal("the failed continuation did not close the response")
			}
			if count := strings.Count(out, terminal); count != 1 {
				t.Fatalf("%d terminal sequences, want exactly one", count)
			}
			if count := strings.Count(out, "data: [DONE]"); count != 1 {
				t.Fatalf("%d done sentinels, want exactly one", count)
			}
			if strings.Contains(out, streamContinuationText) {
				t.Fatal("the client saw text from a continuation that failed")
			}
			if stage.recovery.Splices != 0 {
				t.Fatalf("splices = %d, want none", stage.recovery.Splices)
			}
			if !strings.Contains(stage.recovery.Reason, failure) {
				t.Fatalf("reason = %q, want %q recorded", stage.recovery.Reason, failure)
			}
			if count := strings.Count(out, line); count == 0 || count >= 20 {
				t.Fatalf("the cut landed after %d repetitions, want it early", count)
			}
		})
	}
}

func TestStreamStageRecoverSplicesWhatArrivedWhenTheContinuationBreaks(t *testing.T) {
	chunks, line := streamLoopBody("/v1/chat/completions")
	leg := streamContinuation("/v1/chat/completions")
	// The leg streams its text and then its connection breaks, so the splice has
	// already happened when the error arrives: what it managed to say is spliced in,
	// the answer ends there, and the client still receives one terminator.
	broken := &streamBody{chunks: streamChunks(leg, 64), err: errors.New("upstream reset the continuation")}
	stage, recorder := streamSpliceStage("/v1/chat/completions", streamConfig(loopModeRecover),
		&streamBody{chunks: streamChunks(streamStrings(chunks), 512)},
		func(*loopVerdict) (io.ReadCloser, error) {
			return broken, nil
		})

	if _, err := stage.Run(); err != nil {
		t.Fatal(err)
	}
	out := recorder.written.String()
	if !streamClosed(out, "/v1/chat/completions") {
		t.Fatalf("the broken continuation did not close the response:\n%q", out[max(0, len(out)-200):])
	}
	if !strings.Contains(out, streamContinuationText) {
		t.Fatal("the text the continuation generated before the break was not spliced in")
	}
	if stage.recovery.Splices != 1 {
		t.Fatalf("splices = %d, want the text that arrived before the break", stage.recovery.Splices)
	}
	if !strings.Contains(stage.recovery.Reason, "continuation stream ended early") {
		t.Fatalf("reason = %q, want the broken stream named", stage.recovery.Reason)
	}
	// The reason is a fixed phrase, not the transport's error text: an upstream
	// failure can carry the URL, and nothing a leg generated or a request named
	// belongs in a record.
	if strings.Contains(stage.recovery.Reason, "reset") {
		t.Fatalf("reason = %q, want the transport error kept out of it", stage.recovery.Reason)
	}
	if count := strings.Count(out, line); count == 0 || count >= 20 {
		t.Fatalf("the cut landed after %d repetitions, want it early", count)
	}
	if !strings.Contains(string(stage.recoveryTail), streamContinuationText) {
		t.Fatal("the broken leg's own bytes are not in the tail its cost is read from")
	}
}

func TestStreamStageRecoverAbortsVerdictsItCannotContinue(t *testing.T) {
	cases := []struct {
		name    string
		verdict loopVerdict
	}{
		{"tool arguments", loopVerdict{Channel: loopChannelToolArgs, Period: 64, Repeat: 8, Prefix: `{"path":`}},
		{"thinking after the answer started", loopVerdict{Channel: loopChannelReasoning, Period: 64, Repeat: 8, Suffix: 512, Prefix: "thinking it over ", Generated: true}},
		{"no clean prefix", loopVerdict{Channel: loopChannelContent, Period: 64, Repeat: 8}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var asked int
			stage, recorder := streamSpliceStage("/v1/chat/completions", streamConfig(loopModeRecover),
				&streamBody{}, streamAnswered(streamContinuation("/v1/chat/completions"), &asked))
			if err := stage.finishCut(&test.verdict); err != nil {
				t.Fatal(err)
			}
			if asked != 0 {
				t.Fatalf("asked for %d continuations from a verdict that cannot be continued", asked)
			}
			if out := recorder.written.String(); out != string(loopTerminal("/v1/chat/completions", 0)) {
				t.Fatalf("wrote %q, want the plain terminal sequence", out)
			}
			if stage.recovery == nil || stage.recovery.Reason == "" {
				t.Fatal("no reason recorded for the aborted verdict")
			}
		})
	}
}

func TestStreamStageRecoverRestartsAThinkingLoop(t *testing.T) {
	// A thinking loop in a response that has generated no answer text yet is restarted
	// rather than aborted: the one request it may spend is spent on the answer itself.
	var asked int
	stage, recorder := streamSpliceStage("/v1/chat/completions", streamConfig(loopModeRecover),
		&streamBody{}, streamAnswered(streamContinuation("/v1/chat/completions"), &asked))
	verdict := &loopVerdict{Channel: loopChannelReasoning, Period: 64, Repeat: 8, Suffix: 512, Prefix: "thinking it over "}
	if err := stage.finishCut(verdict); err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Fatalf("asked for %d continuations from a restartable thinking loop", asked)
	}
	out := recorder.written.String()
	if !strings.Contains(out, streamContinuationText) {
		t.Fatal("the restarted answer never reached the client")
	}
	if !streamClosed(out, "/v1/chat/completions") {
		t.Fatalf("the restarted answer did not close the response:\n%q", out[max(0, len(out)-200):])
	}
	if stage.recovery == nil || stage.recovery.Splices != 1 {
		t.Fatalf("recovery = %+v, want the restart recorded as spliced", stage.recovery)
	}
}

func TestStreamStageTerminalCarriesOnlyWhatTheSpliceReports(t *testing.T) {
	// The abort path and the recovery path write the same sequence from the same
	// implementation, so a protocol change cannot leave one copy behind. What the splice
	// adds is what only it can know — the tokens it generated, as Messages counts them and
	// as the usage object chat and Responses carry — and removing that leaves the abort
	// bytes exactly.
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		abort := string(loopTerminal(path, 0))
		reframer := newContinuationReframer(path)
		reframer.Feed([]byte(streamFrameBuilder(path)("eight by")))
		recovered := string(reframer.Finish())
		// The leg reported no usage of its own, so the splice reports the estimate of the
		// 8 bytes it passed through.
		var reported, placeholder string
		switch path {
		case "/v1/chat/completions":
			reported = `,"usage":{"completion_tokens":2,"total_tokens":2}`
		case "/v1/responses":
			reported = `,"usage":{"output_tokens":2,"total_tokens":2}`
		case "/v1/messages":
			reported, placeholder = `"output_tokens":2`, `"output_tokens":0`
		}
		if !strings.Contains(recovered, reported) {
			t.Fatalf("%s: terminal does not report the spliced tokens:\n%q", path, recovered)
		}
		if normalised := strings.Replace(recovered, reported, placeholder, 1); normalised != abort {
			t.Fatalf("%s: terminals differ beyond what the splice reports:\n%q\n%q", path, normalised, abort)
		}
	}
	// Messages closes the block the client has open before the frame that ends the
	// message, on both paths.
	abort := string(loopTerminal("/v1/messages", 0))
	if stop, delta := strings.Index(abort, "event: content_block_stop"), strings.Index(abort, "event: message_delta"); stop < 0 || stop > delta {
		t.Fatalf("the abort terminal does not close the block before message_delta: %q", abort)
	}
}

func TestStreamStageTailKeepsEveryByteRead(t *testing.T) {
	chunks, line := streamLoopBody("/v1/chat/completions")
	full := streamStrings(chunks)
	stage, recorder := streamStageFor("/v1/chat/completions", streamConfig(loopModeAbort),
		"text/event-stream", &streamBody{chunks: [][]byte{[]byte(full)}})
	if _, err := stage.Run(); err != nil {
		t.Fatal(err)
	}
	if len(stage.tail) > streamTailBytes {
		t.Fatalf("tail is %d bytes, want at most %d", len(stage.tail), streamTailBytes)
	}
	// The tail is what the usage and refusal parsers read: it must hold the bytes
	// the cut withheld from the client, not only the bytes that went out.
	if got := strings.Count(string(stage.tail), line); got != 40 {
		t.Fatalf("tail holds %d repetitions, want all 40", got)
	}
	if got := strings.Count(recorder.written.String(), line); got >= 40 {
		t.Fatalf("client received %d repetitions, so the tail proves nothing", got)
	}
}

func TestStreamStageReleasesTrailingFrameAtEOF(t *testing.T) {
	// A frame that never got its terminating blank line still reaches the client:
	// the EOF flush is what releases it.
	body := `data: {"choices":[{"delta":{"content":"An answer cut off mid frame"}}]}`
	stage, recorder := streamStageFor("/v1/chat/completions", streamConfig(loopModeAbort),
		"text/event-stream", &streamBody{chunks: [][]byte{[]byte(body)}})
	verdict, err := stage.Run()
	if err != nil {
		t.Fatal(err)
	}
	if verdict != nil {
		t.Fatal("verdict on a single short frame")
	}
	if recorder.written.String() != body {
		t.Fatalf("released %q, want the trailing frame", recorder.written.String())
	}
}

func TestStreamStageReleasesWithheldFramesAtEOF(t *testing.T) {
	// Observe mode keeps feeding after a verdict, so the frames withheld at the
	// suspicion point are still owed to the client when the stream ends.
	chunks, line := streamLoopBody("/v1/chat/completions")
	full := streamStrings(chunks)
	stage, recorder := streamStageFor("/v1/chat/completions", streamConfig(loopModeObserve),
		"text/event-stream", &streamBody{chunks: streamChunks(full, 4096)})
	if _, err := stage.Run(); err != nil {
		t.Fatal(err)
	}
	if recorder.written.String() != full {
		t.Fatalf("observe released %d bytes, want all %d", recorder.written.Len(), len(full))
	}
	if strings.Count(recorder.written.String(), line) != 40 {
		t.Fatalf("observe delivered %d repetitions, want all 40", strings.Count(recorder.written.String(), line))
	}
}

func TestStreamStageSkipsBodiesItCannotAlignFramesOn(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		contentType string
	}{
		{"json response", "/v1/chat/completions", "application/json"},
		{"error page", "/v1/chat/completions", "text/html; charset=utf-8"},
		{"missing content type", "/v1/chat/completions", ""},
		{"pass-through path", "/v1/embeddings", "text/event-stream"},
	}
	for _, test := range tests {
		chunks, _ := streamLoopBody("/v1/chat/completions")
		full := streamStrings(chunks)
		body := &streamBody{chunks: streamChunks(full, 512)}
		stage, recorder := streamStageFor(test.path, streamConfig(loopModeAbort), test.contentType, body)
		if stage.inspector != nil {
			t.Fatalf("%s: built an inspector", test.name)
		}
		verdict, err := stage.Run()
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if verdict != nil {
			t.Fatalf("%s: verdict on a body that is not an event stream", test.name)
		}
		if recorder.written.String() != full {
			t.Fatalf("%s: released %d bytes, want all %d", test.name, recorder.written.Len(), len(full))
		}
	}
}

func TestStreamStageAcceptsEventStreamWithCharset(t *testing.T) {
	chunks, _ := streamLoopBody("/v1/chat/completions")
	stage, _ := streamStageFor("/v1/chat/completions", streamConfig(loopModeAbort),
		"Text/Event-Stream; charset=utf-8", &streamBody{chunks: streamChunks(streamStrings(chunks), 512)})
	if stage.inspector == nil {
		t.Fatal("a parameterised event-stream content type built no inspector")
	}
}

func TestStreamStageReportsReadError(t *testing.T) {
	readErr := fmt.Errorf("upstream went away")
	chunks, _ := streamLoopBody("/v1/chat/completions")
	stage, recorder := streamStageFor("/v1/chat/completions", streamConfig(loopModeAbort),
		"text/event-stream", &streamBody{chunks: streamChunks(streamStrings(chunks[:1]), 512), err: readErr})
	if _, err := stage.Run(); !errors.Is(err, readErr) {
		t.Fatalf("error = %v, want the read failure", err)
	}
	// What reached the client before the failure stays: an error must not retract
	// bytes that are already on the wire.
	if recorder.written.String() != chunks[0] {
		t.Fatalf("released %q before the failure, want the first frame", recorder.written.String())
	}
}

func TestStreamStageSplicesTheContinuationInspectorsWithheldTail(t *testing.T) {
	// A continuation is inspected like the first leg, so it can end with frames its own
	// inspector is still withholding: a suspicion that never became a verdict holds them
	// until the stream ends. The answer the client is owed includes them.
	frames, marker := streamUnprovenContinuation(t)
	chunks, _ := streamLoopBody("/v1/chat/completions")
	stage, recorder := streamSpliceStage("/v1/chat/completions", streamConfig(loopModeRecover),
		&streamBody{chunks: streamChunks(streamStrings(chunks), 512)},
		streamAnswered(streamStrings(frames), new(int)))

	if _, err := stage.Run(); err != nil {
		t.Fatal(err)
	}
	out := recorder.written.String()
	if got := strings.Count(out, marker); got != 2 {
		t.Fatalf("client saw the repeated block %d times, want the one the leg withheld as well", got)
	}
	if stage.recovery.Splices != 1 {
		t.Fatalf("splices = %d, want the leg's text spliced in", stage.recovery.Splices)
	}
	if !streamClosed(out, "/v1/chat/completions") {
		t.Fatal("the spliced response does not end with the terminal sequence")
	}
}

// streamUnprovenContinuation is a continuation leg whose text makes the detector
// withhold frames without ever confirming a repetition: it generates enough text to arm
// detection, ends with a 64-byte block it already wrote a few hundred bytes earlier, and
// stops there. The candidate is still unproven when the stream ends.
func streamUnprovenContinuation(t *testing.T) (frames []string, marker string) {
	t.Helper()
	frame := streamFrameBuilder("/v1/chat/completions")
	marker = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	if len(marker) != 64 {
		t.Fatalf("marker is %d bytes, want the 64 the detector probes", len(marker))
	}
	for i := range 24 {
		frames = append(frames, frame(fmt.Sprintf(
			"Paragraph %d takes the argument somewhere else entirely, along line %d of the draft.\n", i, i*7+1)))
	}
	frames = append(frames, frame(marker))
	for i := range 3 {
		frames = append(frames, frame(fmt.Sprintf("Then a sentence about %d unrelated details follows.\n", i*13+2)))
	}
	frames = append(frames, frame(marker))
	return frames, marker
}

func TestStreamStageMessagesClosesTheBlockTheClientHasOpen(t *testing.T) {
	// A response opens a thinking block, closes it, and streams its text into block 1. A
	// cut in that text has to close block 1: the thinking block was closed by the provider
	// and block 0 is not what the client is in.
	var body strings.Builder
	body.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
	body.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	body.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	delta := func(text string) string {
		return `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":` +
			loopQuoted(text) + `}}` + "\n\n"
	}
	body.WriteString(delta("Here is the summary you asked for. "))
	line := "The answer stays the same because the reasoning keeps circling back. "
	for range 40 {
		body.WriteString(delta(line))
	}
	stage, recorder := streamStageFor("/v1/messages", streamConfig(loopModeAbort),
		"text/event-stream", &streamBody{chunks: streamChunks(body.String(), 512)})

	verdict, err := stage.Run()
	if err != nil {
		t.Fatal(err)
	}
	if verdict == nil {
		t.Fatal("no verdict for a looping stream")
	}
	out := recorder.written.String()
	if !strings.Contains(out, `data: {"type":"content_block_stop","index":1}`) {
		t.Fatalf("the terminal does not close the block the client has open:\n%q", out[max(0, len(out)-200):])
	}
	if strings.Contains(out[max(0, len(out)-200):], `"index":0}`) {
		t.Fatalf("the terminal closes a block the provider already closed:\n%q", out[max(0, len(out)-200):])
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Fatalf("the terminal reports no stop reason:\n%q", out[max(0, len(out)-200):])
	}
}

func TestStreamStageRecoverSplicesAToolCallContinuation(t *testing.T) {
	// The continuation can answer with a tool call instead of the prose the cut
	// interrupted. The client has to receive the call's identity and arguments, and the
	// closing frame has to name that reason rather than claim the model stopped.
	chunks, _ := streamLoopBody("/v1/chat/completions")
	leg := `data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.go\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":6,"total_tokens":6}}` + "\n\n" +
		"data: [DONE]\n\n"
	stage, recorder := streamSpliceStage("/v1/chat/completions", streamConfig(loopModeRecover),
		&streamBody{chunks: streamChunks(streamStrings(chunks), 512)},
		streamAnswered(leg, new(int)))

	if _, err := stage.Run(); err != nil {
		t.Fatal(err)
	}
	out := recorder.written.String()
	if !strings.Contains(out, `"id":"call_1"`) {
		t.Fatal("the client never learned which tool call it was handed")
	}
	if !strings.Contains(out, `{\"path\":\"a.go\"}`) {
		t.Fatal("the tool call's arguments never reached the client")
	}
	if got := strings.Count(out, `"finish_reason":"tool_calls"`); got != 1 {
		t.Fatalf("%d tool-call finish frames, want exactly one", got)
	}
	if strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatal("the terminal claims the model stopped when it asked for a tool call")
	}
	if !strings.Contains(out, `"completion_tokens":6`) {
		t.Fatal("the terminal does not report the continuation's usage")
	}
	if stage.recovery.Splices != 1 {
		t.Fatalf("splices = %d, want the tool call counted as a splice", stage.recovery.Splices)
	}
}
