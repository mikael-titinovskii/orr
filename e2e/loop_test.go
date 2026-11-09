package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// loopE2ELine is the unit the fake upstream repeats once it starts looping. It
// holds no character JSON has to escape, so it can be counted in what the client
// received.
const loopE2ELine = "The answer stays the same because the reasoning keeps circling back. "

// loopE2EFrame is one chat completion delta.
func loopE2EFrame(content string) string {
	quoted, err := json.Marshal(content)
	if err != nil {
		panic(err)
	}
	return `data: {"choices":[{"delta":{"content":` + string(quoted) + `}}]}` + "\n\n"
}

// loopE2EStream starts normally and then repeats one line for ever, the way a
// model that has lost the thread does, and ends the message the way a healthy
// stream does.
func loopE2EStream(repeats int) string {
	var body strings.Builder
	body.WriteString(`data: {"model":"author/model","provider":"preferred"}` + "\n\n")
	body.WriteString(loopE2EFrame("Here is the summary you asked for. "))
	for range repeats {
		body.WriteString(loopE2EFrame(loopE2ELine))
	}
	body.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	body.WriteString("data: [DONE]\n\n")
	return body.String()
}

// loopE2ECleanStream is an ordinary answer that never repeats itself.
func loopE2ECleanStream(sentences int) string {
	var body strings.Builder
	body.WriteString(`data: {"model":"author/model","provider":"preferred"}` + "\n\n")
	for i := range sentences {
		body.WriteString(loopE2EFrame(fmt.Sprintf("Sentence %d says something new about point %d. ", i, i*i+3)))
	}
	body.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	body.WriteString("data: [DONE]\n\n")
	return body.String()
}

// loopE2EFrames cuts a body at its event boundaries, so the fake upstream sends
// it the way a provider does rather than in one write.
func loopE2EFrames(body string) []string {
	var frames []string
	for {
		index := strings.Index(body, "\n\n")
		if index < 0 {
			if body != "" {
				frames = append(frames, body)
			}
			return frames
		}
		frames = append(frames, body[:index+2])
		body = body[index+2:]
	}
}

// loopE2EContinuationMark is part of the intervention turn orr appends when it asks
// a model to continue a cut response. The fake upstream recognises it to tell a
// continuation from the request it continues, which carries the same client marker.
const loopE2EContinuationMark = "degenerated into a repetition"

// loopE2ERestartMark is part of the instruction a restart appends when the repetition
// was in the model's thinking: there is no answer to continue, so the request asks for
// the answer itself.
const loopE2ERestartMark = "degenerated into a repeating thought"

// loopE2EReasoningFrame is one chat completion reasoning delta, the channel a
// thinking loop appears in.
func loopE2EReasoningFrame(text string) string {
	quoted, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return `data: {"choices":[{"delta":{"reasoning":` + string(quoted) + `}}]}` + "\n\n"
}

// loopE2EThinkingStream loops in the reasoning channel and never produces answer text,
// which is where a model that keeps re-planning the same sentence degenerates.
func loopE2EThinkingStream(repeats int) string {
	var body strings.Builder
	body.WriteString(`data: {"model":"author/model","provider":"preferred"}` + "\n\n")
	body.WriteString(loopE2EReasoningFrame("Checking the comments first.\n\n"))
	for range repeats {
		body.WriteString(loopE2EReasoningFrame(loopE2ELine + "\n\n"))
	}
	body.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	body.WriteString("data: [DONE]\n\n")
	return body.String()
}

// loopE2EUpstream serves either the looping or the clean body, chosen by the
// marker the client sent, so one upstream can back every case. A continuation
// request is answered with a clean stream, which is what makes recovery observable
// end to end: the client's response is the looping answer cut short and continued.
func loopE2EUpstream(t *testing.T, looping, clean string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEndpointCatalog(w, "preferred")
			return
		}
		request, _ := io.ReadAll(r.Body)
		body := clean
		switch {
		case strings.Contains(string(request), loopE2EContinuationMark),
			strings.Contains(string(request), loopE2ERestartMark):
			body = loopE2ECleanStream(60)
		case strings.Contains(string(request), `"user":"loop"`):
			body = looping
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, frame := range loopE2EFrames(body) {
			if _, err := io.WriteString(w, frame); err != nil {
				// orr cut the stream and closed the connection: the rest of the
				// repetition is never generated for it.
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// loopE2EPost sends one streaming chat completion and returns the client body.
// An empty marker asks the upstream for its clean stream.
func loopE2EPost(t *testing.T, baseURL, marker string) string {
	t.Helper()
	// The turn is what makes the request continuable: recovery re-sends the
	// conversation with the clean prefix appended, and a body without messages has
	// nothing to continue from.
	const turn = `"messages":[{"role":"user","content":"write a story"}]`
	payload := `{"model":"author/model","stream":true,` + turn + `}`
	if marker != "" {
		payload = fmt.Sprintf(`{"model":"author/model","stream":true,`+turn+`,"user":%q}`, marker)
	}
	response, err := http.Post(baseURL+"/v1/chat/completions", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the cut response to stay 200: %s", response.StatusCode, body)
	}
	return string(body)
}

func TestLoopDetectionCutsARepeatingStream(t *testing.T) {
	const repeats = 400
	looping := loopE2EStream(repeats)
	clean := loopE2ECleanStream(60)
	upstream := loopE2EUpstream(t, looping, clean)

	// Recover is the shipped default. Both modes stop the repetition in place and
	// close the response exactly once; what recover adds on top of the cut is the
	// splice, which its own test proves.
	for _, mode := range []string{"abort", "recover"} {
		t.Run(mode, func(t *testing.T) {
			orr := startServerWithEnv(t, upstream.URL+"/api/v1", configuredProviders,
				map[string]string{"ORR_LOOP_DETECTION": mode})

			body := loopE2EPost(t, orr.baseURL, "loop")
			if count := strings.Count(body, "data: [DONE]"); count != 1 {
				t.Fatalf("%d done sentinels, want exactly one terminal sequence", count)
			}
			if count := strings.Count(body, `"finish_reason":"stop"`); count != 1 {
				t.Fatalf("%d finish_reason frames, want exactly one", count)
			}
			if got := strings.Count(body, loopE2ELine); got == 0 || got >= repeats/4 {
				t.Fatalf("client received %d of %d repetitions, want the loop started and cut before it fills the budget", got, repeats)
			}
			if len(body) >= len(looping)/2 {
				t.Fatalf("client received %d of %d bytes, want the repetition cut short", len(body), len(looping))
			}

			// A clean response is untouched: the stage changes no byte and adds no
			// frame to a stream that never loops.
			if got := loopE2EPost(t, orr.baseURL, ""); got != clean {
				t.Fatalf("clean stream: client received %d bytes, want the whole %d-byte response unchanged", len(got), len(clean))
			}
		})
	}
}

func TestLoopObserveDeliversTheWholeStream(t *testing.T) {
	looping := loopE2EStream(400)
	upstream := loopE2EUpstream(t, looping, loopE2ECleanStream(60))
	orr := startServerWithEnv(t, upstream.URL+"/api/v1", configuredProviders,
		map[string]string{"ORR_LOOP_DETECTION": "observe"})

	if got := loopE2EPost(t, orr.baseURL, "loop"); got != looping {
		t.Fatalf("observe changed the bytes on the wire: received %d of %d bytes", len(got), len(looping))
	}
}

func TestLoopRecoverySplicesAContinuation(t *testing.T) {
	const repeats = 400
	looping := loopE2EStream(repeats)
	upstream := loopE2EUpstream(t, looping, loopE2ECleanStream(60))
	orr := startServerWithEnv(t, upstream.URL+"/api/v1", configuredProviders,
		map[string]string{"ORR_LOOP_DETECTION": "recover"})

	body := loopE2EPost(t, orr.baseURL, "loop")

	// One answer: the cut leg's finish frame never reaches the client, and the one
	// terminator at the end of the body belongs to the spliced response.
	if count := strings.Count(body, "data: [DONE]"); count != 1 {
		t.Fatalf("%d done sentinels, want exactly one for the whole response", count)
	}
	if count := strings.Count(body, `"finish_reason":"stop"`); count != 1 {
		t.Fatalf("%d finish_reason frames, want exactly one", count)
	}
	// One identity: the continuation opens a response of its own upstream, and none
	// of that preamble is passed on to a client that already has one.
	if count := strings.Count(body, `"provider":"preferred"`); count != 1 {
		t.Fatalf("%d response preambles, want the one the first leg sent", count)
	}
	// The repetition is cut short, and the continuation's own text follows it, so the
	// client reads a single continuous answer instead of a stalled one.
	if got := strings.Count(body, loopE2ELine); got == 0 || got >= repeats/4 {
		t.Fatalf("client received %d of %d repetitions, want the loop cut short", got, repeats)
	}
	const continuationText = "Sentence 0 says something new about point 3. "
	spliced := strings.Index(body, continuationText)
	if spliced < 0 {
		t.Fatalf("the continuation never reached the client:\n%s", body[max(0, len(body)-400):])
	}
	if cut := strings.LastIndex(body, loopE2ELine); cut > spliced {
		t.Fatal("the continuation's text arrived before the cut, so the splice is out of order")
	}
}

func TestLoopRecoveryRestartsAThinkingLoop(t *testing.T) {
	const repeats = 400
	looping := loopE2EThinkingStream(repeats)
	upstream := loopE2EUpstream(t, looping, loopE2ECleanStream(60))
	orr := startServerWithEnv(t, upstream.URL+"/api/v1", configuredProviders,
		map[string]string{"ORR_LOOP_DETECTION": "recover"})

	body := loopE2EPost(t, orr.baseURL, "loop")

	// The cut leg generated no answer text at all, so the only way the continuation's
	// text can be in this response is the restart path: a resumable verdict needs a
	// clean prefix, and there is none here.
	const continuationText = "Sentence 0 says something new about point 3. "
	spliced := strings.Index(body, continuationText)
	if spliced < 0 {
		t.Fatalf("the restarted answer never reached the client:\n%s", body[max(0, len(body)-400):])
	}
	// One response: one terminator and one preamble, with the repetition cut short so
	// the answer arrives instead of a thought that never ends.
	if count := strings.Count(body, "data: [DONE]"); count != 1 {
		t.Fatalf("%d done sentinels, want exactly one for the whole response", count)
	}
	if count := strings.Count(body, `"provider":"preferred"`); count != 1 {
		t.Fatalf("%d response preambles, want the one the first leg sent", count)
	}
	if got := strings.Count(body, loopE2ELine); got == 0 || got >= repeats/4 {
		t.Fatalf("client received %d of %d repetitions, want the loop cut short", got, repeats)
	}
	if cut := strings.LastIndex(body, loopE2ELine); cut > spliced {
		t.Fatal("the restarted answer arrived before the cut, so the splice is out of order")
	}
}
