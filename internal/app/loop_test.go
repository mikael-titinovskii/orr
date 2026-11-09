package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// loopQuoted is text as a JSON string literal.
func loopQuoted(text string) string {
	quoted, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return string(quoted)
}

func loopChatFrame(content string) string {
	return `data: {"choices":[{"delta":{"content":` + loopQuoted(content) + `}}]}` + "\n\n"
}

func loopChatReasoningFrame(text string) string {
	return `data: {"choices":[{"delta":{"reasoning":` + loopQuoted(text) + `}}]}` + "\n\n"
}

func loopChatReasoningContentFrame(text string) string {
	return `data: {"choices":[{"delta":{"reasoning_content":` + loopQuoted(text) + `}}]}` + "\n\n"
}

func loopChatToolFrame(index int, arguments string) string {
	return fmt.Sprintf(`data: {"choices":[{"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}}]}`+"\n\n",
		index, loopQuoted(arguments))
}

func loopThinkingFrame(text string) string {
	return `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":` +
		loopQuoted(text) + `}}` + "\n\n"
}

func loopResponsesFrame(text string) string {
	return `data: {"type":"response.output_text.delta","delta":` + loopQuoted(text) + `}` + "\n\n"
}

func loopRepeated(build func(string) string, intro, line string, repeats int) []string {
	chunks := []string{build(intro)}
	for range repeats {
		chunks = append(chunks, build(line))
	}
	return chunks
}

// loopFlush ends the stream the way a caller does at EOF.
func loopFlush(inspector *loopInspector) string {
	tail, _ := inspector.Feed(nil)
	return string(tail)
}

// loopFeedAll feeds every chunk and then flushes, returning everything released
// and the first verdict. It keeps feeding after a verdict, which is what an
// observe-only caller does, so the whole stream still has to come out.
func loopFeedAll(t *testing.T, inspector *loopInspector, chunks []string) (string, *loopVerdict) {
	t.Helper()
	var released strings.Builder
	var verdict *loopVerdict
	for _, chunk := range chunks {
		out, found := inspector.Feed([]byte(chunk))
		released.Write(out)
		if found != nil && verdict == nil {
			verdict = found
		}
	}
	tail, _ := inspector.Feed(nil)
	released.Write(tail)
	return released.String(), verdict
}

// loopFeedUntilVerdict stops feeding as soon as a verdict arrives, which is what
// an aborting or recovering caller does: everything after the verdict is dropped.
func loopFeedUntilVerdict(t *testing.T, inspector *loopInspector, chunks []string) (string, *loopVerdict) {
	t.Helper()
	var released strings.Builder
	for _, chunk := range chunks {
		out, verdict := inspector.Feed([]byte(chunk))
		released.Write(out)
		if verdict != nil {
			return released.String(), verdict
		}
	}
	return released.String(), nil
}

// The largest accepted minimum period must still leave detection reachable: a unit
// shorter than the minimum is rounded up to a multiple of it, and that multiple has to
// fit inside the stretch tier 1 considers. At the full loopMaxPeriod it does not — a
// 40-byte unit rounds to 2080 and is discarded — which is why the configured ceiling is
// half of it.
func TestLoopDetectsShortUnitAtTheConfiguredPeriodCeiling(t *testing.T) {
	const line = "Let me write the synthesis in the file.\n"
	if len(line) != 40 {
		t.Fatalf("fixture line is %d bytes, want 40", len(line))
	}
	inspector := newLoopInspector("/v1/chat/completions",
		loopLimits{MinPeriod: maxLoopMinPeriod, MinRepeat: 2 * maxLoopMinPeriod})
	chunks := loopRepeated(loopChatFrame, "writing:\n", line, 200)
	_, verdict := loopFeedUntilVerdict(t, inspector, chunks)
	if verdict == nil {
		t.Fatal("no verdict: a short unit at the largest accepted minimum period must still be detected")
	}
	if verdict.Period < len(line) {
		t.Fatalf("period = %d, want at least the %d-byte unit", verdict.Period, len(line))
	}
}

func loopCheckVerdictShape(t *testing.T, verdict *loopVerdict, period int) {
	t.Helper()
	if verdict.Period != period {
		t.Fatalf("period = %d, want %d", verdict.Period, period)
	}
	if verdict.Suffix != verdict.Period*verdict.Repeat {
		t.Fatalf("suffix = %d, want %d repeats of %d", verdict.Suffix, verdict.Repeat, verdict.Period)
	}
	if verdict.Suffix < loopMinSuffixBytes {
		t.Fatalf("suffix = %d, want at least %d", verdict.Suffix, loopMinSuffixBytes)
	}
}

// A candidate can arm on a coincidental repeat, so the withheld region can hold
// ordinary prose as well as the repetition. Only the suffix may be dropped: Prefix
// ends at the loop, and the continuation resumes from it, so a client that never
// received that prose would be handed an answer with an invisible hole.
func TestLoopReleasesProseWithheldBeforeTheConfirmedSuffix(t *testing.T) {
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{MinPeriod: 32, MinRepeat: 512})
	prose := "This prose predates the loop, and the client has not seen it yet.\n"
	line := "Let me write the synthesis in the file.\n"
	// The frames carry JSON-escaped text, so the prose is matched as a fragment.
	proseMark := "This prose predates the loop"
	lineMark := "Let me write the synthesis"
	inspector.held.add([]byte(loopChatFrame(prose)))
	inspector.held.add([]byte(loopChatFrame(line)))
	inspector.holdingText = len(prose) + len(line)
	// The confirmed suffix is the repetition; everything before it in the hold is prose.
	release := inspector.releaseBefore(&loopVerdict{
		Channel: loopChannelContent, Period: len(line), Suffix: len(line),
	})
	if !strings.Contains(string(release), proseMark) {
		t.Fatalf("released %q, want the prose the client has not seen", release)
	}
	if strings.Contains(string(release), lineMark) {
		t.Fatalf("released %q, want the confirmed repetition withheld", release)
	}
	if !strings.Contains(string(inspector.held.text()), lineMark) {
		t.Fatal("the repetition was released, want it kept for the cut")
	}
	// Nothing before the loop is left behind: a later release has nothing to hand over.
	if rest := inspector.releaseBefore(&loopVerdict{
		Channel: loopChannelContent, Period: len(line), Suffix: len(line),
	}); rest != nil {
		t.Fatalf("second release = %q, want nothing left before the suffix", rest)
	}
}

// One delta can carry the tail of the clean prefix and the start of the repetition
// together. Releasing it would put the first bytes of the loop into the repaired
// answer, so it stays withheld and the prefix is trimmed instead.
// The frame that arms a candidate is withheld with it, so its text has to count as
// withheld: a count starting at zero makes the withheld region look smaller than it is,
// and the trimmed prefix then ends past what the client actually received.
func TestLoopCountsTheArmingFrameAsWithheld(t *testing.T) {
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{MinPeriod: 32, MinRepeat: 512})
	block := strings.Repeat("K", 63) + "."
	intro := strings.Repeat("ab", 550)  // 1100 bytes, so the ring is armed
	filler := strings.Repeat("cd", 150) // 300 bytes between the two block copies
	text := intro + block + filler + block
	frame := loopChatFrame(text)
	inspector.Feed([]byte(frame))
	if !inspector.holding {
		t.Fatalf("no candidate armed; inspector = %+v", inspector)
	}
	if inspector.holdingText != len(text) {
		t.Fatalf("holdingText = %d, want the whole withheld frame's %d bytes", inspector.holdingText, len(text))
	}
}

// A single delta can arm a candidate and confirm it, in which case its frame is not in
// the withheld queue yet when the release runs. Nothing can be handed over, so the whole
// clean prefix of that frame must be reported as dropped instead of silently kept in the
// prefix the continuation resumes from.
// A frame can carry several deltas (a chat request with n>1), and the one that arms a
// candidate is not necessarily the first. The frame is withheld whole, so its earlier
// same-channel text has to open the withheld count too, or the trimmed prefix runs past
// what the client received.
// Parallel tool calls stream in one frame. Their texts belong to different calls, so the
// arming count must not add another call's arguments: over-counting would widen the
// release window and let the trim under-shoot in the other direction.
// Parallel calls share the tool-argument channel but not a bucket. The release must charge
// only the call whose arguments armed the candidate: charging every call over-prices the
// frames and discards clean text that was already accounted as withheld.
// A call index is kept exactly, past the detector's staging cap too. Folding a call onto a
// shared bucket would charge its arguments to another call's withheld budget - or, once that
// was excluded, understate the prefix the continuation is trimmed by.
func TestLoopDoesNotAliasToolCallsBeyondTheCap(t *testing.T) {
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{MinPeriod: 32, MinRepeat: 512})
	first := strings.Repeat("x", 300)
	second := strings.Repeat("cd", 500) + strings.Repeat("K", 63) + "." + strings.Repeat("K", 63) + "."
	frame, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{
			map[string]any{"index": 0, "function": map[string]any{"arguments": first}},
			map[string]any{"index": loopToolCallStates, "function": map[string]any{"arguments": second}},
		},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	inspector.Feed([]byte("data: " + string(frame) + "\n\n"))
	if !inspector.holding {
		t.Fatal("no candidate armed on the untracked call")
	}
	if inspector.holdingText != len(second) {
		t.Fatalf("holdingText = %d, want only the arming call's %d bytes (call 0 carried %d)", inspector.holdingText, len(second), len(first))
	}
}

func TestLoopReleaseChargesOnlyTheArmingToolCall(t *testing.T) {
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{MinPeriod: 32, MinRepeat: 512})
	other := strings.Repeat("y", 300)
	arming := strings.Repeat("z", 1128)
	otherFrame := loopChatToolFrame(0, other)
	armingFrame := loopChatToolFrame(1, arming)
	inspector.held.add([]byte(otherFrame))
	inspector.held.add([]byte(armingFrame))
	inspector.holdingText = len(arming)
	inspector.candidateToolCall = 1

	verdict := &loopVerdict{Channel: loopChannelToolArgs, Period: 64, Suffix: 64, ToolCall: 1}
	release := inspector.releaseBefore(verdict)
	if !strings.Contains(string(release), "yyyy") {
		t.Fatalf("released %q, want the other call's frame handed over", release)
	}
	if strings.Contains(string(release), "zzzz") {
		t.Fatalf("released %q, want the arming call's arguments withheld", release)
	}
	// Only the arming call's text was withheld from the client and not released.
	if want := len(arming) - 64; verdict.DroppedText != want {
		t.Fatalf("dropped = %d, want %d bytes of the arming call's clean text", verdict.DroppedText, want)
	}
}

func TestLoopCountsParallelToolCallsSeparately(t *testing.T) {
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{MinPeriod: 32, MinRepeat: 512})
	first := strings.Repeat("x", 300)
	second := strings.Repeat("cd", 500) + strings.Repeat("K", 63) + "." + strings.Repeat("K", 63) + "."
	frame, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{
			map[string]any{"index": 0, "function": map[string]any{"arguments": first}},
			map[string]any{"index": 1, "function": map[string]any{"arguments": second}},
		},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	inspector.Feed([]byte("data: " + string(frame) + "\n\n"))
	if !inspector.holding {
		t.Fatalf("no candidate armed; frameText = %v", inspector.frameText)
	}
	if inspector.holdingText != len(second) {
		t.Fatalf("holdingText = %d, want only the arming call's %d bytes", inspector.holdingText, len(second))
	}
}

func TestLoopCountsAFramesEarlierDeltasWhenTheSecondArms(t *testing.T) {
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{MinPeriod: 32, MinRepeat: 512})
	block := strings.Repeat("K", 63) + "."
	first := strings.Repeat("ab", 60)
	second := strings.Repeat("cd", 560) + block + block
	frame, err := json.Marshal(map[string]any{"choices": []any{
		map[string]any{"delta": map[string]any{"content": first}},
		map[string]any{"delta": map[string]any{"content": second}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	inspector.Feed([]byte("data: " + string(frame) + "\n\n"))
	if !inspector.holding {
		t.Fatalf("no candidate armed; frameText = %v", inspector.frameText)
	}
	if want := len(first) + len(second); inspector.holdingText != want {
		t.Fatalf("holdingText = %d, want the withheld frame's %d bytes", inspector.holdingText, want)
	}
}

func TestLoopReportsDroppedTextWhenOneDeltaArmsAndConfirms(t *testing.T) {
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{MinPeriod: 32, MinRepeat: 512})
	prose := "ordinary prose that the client has not seen.\n"
	line := "Let me write the synthesis in the file.\n"
	inspector.holdingText = len(prose) + len(line)

	verdict := &loopVerdict{Channel: loopChannelContent, Period: len(line), Suffix: len(line)}
	if release := inspector.releaseBefore(verdict); release != nil {
		t.Fatalf("released %q with an empty withheld queue", release)
	}
	if verdict.DroppedText != len(prose) {
		t.Fatalf("dropped = %d, want the %d bytes of prose the client will not get", verdict.DroppedText, len(prose))
	}
}

func TestLoopKeepsAStraddlingFrameOutOfTheRelease(t *testing.T) {
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{MinPeriod: 32, MinRepeat: 512})
	prose := "ordinary prose before the loop.\n"
	line := "Let me write the synthesis in the file.\n"
	inspector.held.add([]byte(loopChatFrame(prose + line)))
	inspector.holdingText = len(prose) + len(line)

	verdict := &loopVerdict{Channel: loopChannelContent, Period: len(line), Suffix: len(line)}
	if release := inspector.releaseBefore(verdict); release != nil {
		t.Fatalf("released %q, want a straddling frame withheld", release)
	}
	if verdict.DroppedText != len(prose) {
		t.Fatalf("dropped = %d, want the %d bytes of prose the client will not get", verdict.DroppedText, len(prose))
	}
}

func TestLoopDetectsChatContentLoop(t *testing.T) {
	intro := "Here is the summary you asked for.\n\n"
	line := "The answer stays the same because the reasoning keeps circling back.\n"
	chunks := []string{loopChatFrame(intro)}
	for range 30 {
		chunks = append(chunks, loopChatFrame(line))
	}
	full := strings.Join(chunks, "")

	released, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a content loop")
	}
	if verdict.Channel != loopChannelContent {
		t.Fatalf("channel = %d, want content", verdict.Channel)
	}
	loopCheckVerdictShape(t, verdict, len(line))
	if verdict.Prefix != intro {
		t.Fatalf("prefix = %q, want %q", verdict.Prefix, intro)
	}
	if !strings.HasPrefix(full, released) {
		t.Fatalf("released text is not a prefix of the stream: %q", released)
	}
	if got := strings.Count(released, line); got >= 30 {
		t.Fatalf("released %d repeats; the loop was not cut before the client", got)
	}
}

func TestLoopDetectsAnthropicThinkingLoop(t *testing.T) {
	intro := "I should look at the routing table again.\n"
	line := "Let me reconsider the same point once more, because I keep circling.\n"
	chunks := loopRepeated(loopThinkingFrame, intro, line, 30)

	_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/messages", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a thinking loop")
	}
	if verdict.Channel != loopChannelReasoning {
		t.Fatalf("channel = %d, want reasoning", verdict.Channel)
	}
	loopCheckVerdictShape(t, verdict, len(line))
	if verdict.Prefix != intro {
		t.Fatalf("prefix = %q, want %q", verdict.Prefix, intro)
	}
}

func TestLoopDetectsChatReasoningLoop(t *testing.T) {
	line := "Thinking the same thought again because the trace keeps returning.\n"
	// Both chat reasoning field names feed the same channel.
	chunks := []string{loopChatReasoningFrame("Checking the routing table.\n")}
	for i := range 30 {
		if i%2 == 0 {
			chunks = append(chunks, loopChatReasoningContentFrame(line))
			continue
		}
		chunks = append(chunks, loopChatReasoningFrame(line))
	}

	_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a chat reasoning loop")
	}
	if verdict.Channel != loopChannelReasoning {
		t.Fatalf("channel = %d, want reasoning", verdict.Channel)
	}
	loopCheckVerdictShape(t, verdict, len(line))
}

func TestLoopDetectsResponsesLoop(t *testing.T) {
	line := "The same paragraph is generated again and again until max_tokens.\n"
	chunks := loopRepeated(loopResponsesFrame, "Starting the answer.\n", line, 30)

	_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/responses", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a Responses loop")
	}
	if verdict.Channel != loopChannelContent {
		t.Fatalf("channel = %d, want content", verdict.Channel)
	}
	loopCheckVerdictShape(t, verdict, len(line))
}

func TestLoopDetectsToolArgumentLoop(t *testing.T) {
	fragment := `{"path":"internal/app/proxy.go","line":42},`
	chunks := []string{loopChatToolFrame(0, `{"edits":[`)}
	for range 40 {
		chunks = append(chunks, loopChatToolFrame(0, fragment))
	}
	chunks = append(chunks, loopChatToolFrame(0, `]}`))

	_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a tool-argument loop")
	}
	if verdict.Channel != loopChannelToolArgs {
		t.Fatalf("channel = %d, want tool arguments", verdict.Channel)
	}
	loopCheckVerdictShape(t, verdict, len(fragment))
	if verdict.Prefix != `{"edits":[` {
		t.Fatalf("prefix = %q, want the arguments before the loop", verdict.Prefix)
	}
}

func TestLoopKeepsToolCallTextSeparate(t *testing.T) {
	fragment := `{"path":"internal/app/loop.go","line":7},`
	chunks := []string{
		loopChatToolFrame(0, `{"first":[`),
		loopChatToolFrame(0, `{"unrelated":"arguments of the first call, long enough to be its own text"},`),
		loopChatToolFrame(1, `{"second":`),
	}
	for range 40 {
		chunks = append(chunks, loopChatToolFrame(1, fragment))
	}

	_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a tool-argument loop in the second call")
	}
	loopCheckVerdictShape(t, verdict, len(fragment))
	if verdict.Prefix != `{"second":` {
		t.Fatalf("prefix = %q, want only the second call's text", verdict.Prefix)
	}
}

// loopToolFragment is one tool call's repeating argument fragment: long enough to
// carry a period of its own and with no repetition inside it.
const loopToolFragment = `{"path":"internal/app/loop.go","line":7},`

// loopToolCallStream is `calls` parallel tool calls streaming interleaved in round
// robin, with call loopIndex repeating loopToolFragment and every other call
// generating arguments of its own.
func loopToolCallStream(calls, loopIndex, rounds int) []string {
	chunks := make([]string, 0, calls+calls*rounds)
	for call := range calls {
		chunks = append(chunks, loopChatToolFrame(call, `{"calls":[`))
	}
	for i := range rounds {
		for call := range calls {
			if call == loopIndex {
				chunks = append(chunks, loopChatToolFrame(call, loopToolFragment))
				continue
			}
			chunks = append(chunks, loopChatToolFrame(call, fmt.Sprintf(`{"distinct":%d},`, i)))
		}
	}
	return chunks
}

// loopToolCallStreamWith is loopToolCallStream with an explicit repetend and filler
// size: every call except loopIndex streams a `filler`-byte fragment of its own.
func loopToolCallStreamWith(calls, loopIndex, rounds, filler int, repetend string) []string {
	chunks := make([]string, 0, calls+calls*rounds)
	for call := range calls {
		chunks = append(chunks, loopChatToolFrame(call, `{"calls":[`))
	}
	for i := range rounds {
		for call := range calls {
			if call == loopIndex {
				chunks = append(chunks, loopChatToolFrame(call, repetend))
				continue
			}
			chunks = append(chunks, loopChatToolFrame(call, loopFiller(call, i, filler)))
		}
	}
	return chunks
}

func TestLoopDetectsInterleavedToolCallLoop(t *testing.T) {
	// Parallel tool calls stream interleaved, so each call keeps its own text.
	for _, loopIndex := range []int{0, 1} {
		t.Run(fmt.Sprintf("call-%d-loops", loopIndex), func(t *testing.T) {
			chunks := loopToolCallStream(2, loopIndex, 60)
			_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
			if verdict == nil {
				t.Fatalf("no verdict with interleaved tool calls and call %d looping", loopIndex)
			}
			if verdict.Channel != loopChannelToolArgs {
				t.Fatalf("channel = %d, want tool arguments", verdict.Channel)
			}
			loopCheckVerdictShape(t, verdict, len(loopToolFragment))
		})
	}
}

func TestLoopDetectsInterleavedToolCallLoopUpToStateCap(t *testing.T) {
	// How many calls stream at once is the detector's real limit: one call's text
	// state is reset as soon as its slot is reused, and a call whose ring never
	// reaches loopArmBytes is never even looked at. Five to ten parallel calls is
	// ordinary agentic traffic, so the cap has to cover it.
	for _, calls := range []int{5, 6, loopToolCallStates} {
		for _, loopIndex := range []int{0, calls - 1} {
			t.Run(fmt.Sprintf("%d-calls-call-%d-loops", calls, loopIndex), func(t *testing.T) {
				chunks := loopToolCallStream(calls, loopIndex, 60)
				_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
				if verdict == nil {
					t.Fatalf("no verdict with %d interleaved calls and call %d looping", calls, loopIndex)
				}
				if verdict.Channel != loopChannelToolArgs {
					t.Fatalf("channel = %d, want tool arguments", verdict.Channel)
				}
				loopCheckVerdictShape(t, verdict, len(loopToolFragment))
			})
		}
	}
}

func TestLoopLeaksFewCopiesWithParallelCalls(t *testing.T) {
	// Eight parallel calls streaming ~1 KiB argument deltas, with call 3 looping.
	// Text from the other calls must not count against the candidate's budget: when
	// it did, a real loop was withdrawn and re-probed repeatedly and many copies of
	// the repetend reached the client before the cut.
	repetend := loopLine(512)
	chunks := loopToolCallStreamWith(loopToolCallStates, 3, 20, 1000, repetend)

	released, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a tool-argument loop among 8 parallel calls")
	}
	loopCheckVerdictShape(t, verdict, len(repetend))
	leaked := strings.Count(released, repetend)
	t.Logf("leaked %d copies of the %d-byte repetend before the verdict", leaked, len(repetend))
	if leaked > 3 {
		t.Fatalf("leaked %d copies of the repetend before the verdict, want at most 3", leaked)
	}
}

func TestLoopLeaksFewCopiesWithLargeParallelDeltas(t *testing.T) {
	// Eight parallel calls with 4 KiB deltas put 28 KiB of foreign text into one
	// interleaved round. The quiet budget has to be measured from the candidate's own
	// last delta, not from suspicion: counted from suspicion it empties in under two
	// rounds, a candidate that is still repeating is withdrawn, and its copies go to
	// the client.
	repetend := loopLine(1200)
	chunks := loopToolCallStreamWith(loopToolCallStates, 0, 20, 4<<10, repetend)

	released, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a 1200-byte loop among 8 parallel calls with 4 KiB deltas")
	}
	loopCheckVerdictShape(t, verdict, len(repetend))
	leaked := strings.Count(released, repetend)
	t.Logf("leaked %d copies of the %d-byte repetend before the verdict", leaked, len(repetend))
	if leaked > 2 {
		t.Fatalf("leaked %d copies of the repetend before the verdict, want at most 2", leaked)
	}
}

func TestLoopWithdrawsQuietCandidate(t *testing.T) {
	// A candidate whose own channel stops generating text must still be abandoned:
	// the content channel repeats, then the model moves to reasoning and never
	// returns to content. Confirmation is made impossible here, so tier 1 stays
	// suspicious until the quiet guard withdraws it.
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{MinRepeat: loopRingBytes + loopMinSuffixBytes})
	line := "A line that repeats while the content channel is still producing text.\n"
	var chunks []string
	for range 8 {
		chunks = append(chunks, loopChatFrame(strings.Repeat(line, 4)))
	}
	for i := range 160 {
		chunks = append(chunks, loopChatReasoningFrame(loopFiller(i, 0, 1024)))
	}
	full := strings.Join(chunks, "")

	var released strings.Builder
	suspicious := false
	maxHeld := 0
	var verdict *loopVerdict
	for _, chunk := range chunks {
		out, found := inspector.Feed([]byte(chunk))
		released.Write(out)
		if found != nil {
			verdict = found
		}
		if inspector.Suspect() {
			suspicious = true
		}
		if inspector.held.size() > maxHeld {
			maxHeld = inspector.held.size()
		}
	}
	if verdict != nil {
		t.Fatalf("verdict with confirmation disabled: period %d", verdict.Period)
	}
	if !suspicious {
		t.Fatal("tier 1 never became suspicious, so the quiet guard was not exercised")
	}
	if inspector.Suspect() {
		t.Fatal("still withholding frames after the candidate's channel went quiet")
	}
	// The quiet budget is the tighter of the two: these frames are lean enough that
	// it ends the candidate before the withheld queue reaches its own cap.
	if maxHeld >= loopHoldLagBytes {
		t.Fatalf("withheld %d bytes, so the lag cap ended it rather than the quiet budget", maxHeld)
	}
	if released.String() != full {
		t.Fatalf("released %d bytes before EOF, want all %d: frames were still withheld", released.Len(), len(full))
	}
	if tail := loopFlush(inspector); tail != "" {
		t.Fatalf("flush released %d withheld bytes", len(tail))
	}
}

func TestLoopBoundsWindowMemory(t *testing.T) {
	// The loopInspector comment claims a window holds at most 1.5 times its limit
	// plus the text being appended; this checks that against every window the
	// detector can fill, and that nothing is left buffered.
	const delta = 960
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{})
	for i := range 200 {
		if _, verdict := inspector.Feed([]byte(loopChatFrame(loopFiller(0, i, delta)))); verdict != nil {
			t.Fatalf("verdict on a clean content stream at frame %d", i)
		}
	}
	for round := range 40 {
		for call := range loopToolCallStates {
			if _, verdict := inspector.Feed([]byte(loopChatToolFrame(call, loopFiller(call, round, delta)))); verdict != nil {
				t.Fatalf("verdict on clean tool-call arguments")
			}
		}
	}

	check := func(name string, window textWindow) {
		t.Helper()
		if want := 3*window.limit/2 + delta; len(window.buf) > want {
			t.Fatalf("%s buffer = %d bytes, want at most %d", name, len(window.buf), want)
		}
	}
	for ch, state := range inspector.channels {
		check(fmt.Sprintf("channel %d ring", ch), state.ring)
		check(fmt.Sprintf("channel %d prefix", ch), state.prefix)
	}
	for n, state := range inspector.toolCalls {
		check(fmt.Sprintf("tool state %d ring", n), state.ring)
		check(fmt.Sprintf("tool state %d prefix", n), state.prefix)
	}
	check("withheld frames", inspector.held)
	if len(inspector.pending) != 0 || inspector.held.size() != 0 {
		t.Fatalf("pending = %d, withheld = %d, want both empty", len(inspector.pending), inspector.held.size())
	}
}

func TestLoopToolCallStateCapIsTheLimit(t *testing.T) {
	// One call past the cap is where tracking stops: every delta stages a new
	// index and evicts the least recently used slot, so no call accumulates text
	// and none can be reported. What must still hold is that the detector does not
	// damage the stream or withhold anything for ever.
	calls := loopToolCallStates + 1
	chunks := loopToolCallStream(calls, 0, 60)
	full := strings.Join(chunks, "")

	released, verdict := loopFeedAll(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict != nil {
		t.Fatalf("verdict past the cap: channel %d, period %d", verdict.Channel, verdict.Period)
	}
	if released != full {
		t.Fatalf("released %d bytes, want all %d", len(released), len(full))
	}
}

func TestLoopSurvivesOversizedFrame(t *testing.T) {
	// One provider delta larger than the reassembly buffer, split across reads,
	// used to switch detection off for the rest of the response.
	var huge strings.Builder
	for i := 0; huge.Len() < 200<<10; i++ {
		fmt.Fprintf(&huge, "filler %d of one very large delta. ", i)
	}
	frame := loopChatFrame(huge.String())
	line := "The same repeated line follows the oversized frame until max_tokens.\n"
	var chunks []string
	for offset := 0; offset < len(frame); offset += 32 << 10 {
		chunks = append(chunks, frame[offset:min(offset+32<<10, len(frame))])
	}
	for range 40 {
		chunks = append(chunks, loopChatFrame(line))
	}
	full := strings.Join(chunks, "")

	released, verdict := loopFeedAll(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict after an oversized frame: detection was switched off")
	}
	if verdict.Channel != loopChannelContent {
		t.Fatalf("channel = %d, want content", verdict.Channel)
	}
	loopCheckVerdictShape(t, verdict, len(line))
	if released != full {
		t.Fatalf("released %d bytes, want all %d", len(released), len(full))
	}
}

func TestLoopConfirmsAtConfiguredByteThreshold(t *testing.T) {
	// ORR_LOOP_MIN_REPEAT is a minimum periodic suffix in bytes, so a configured
	// 512 must confirm a short period rather than demand 512 repeats of it.
	line := loopLine(69)
	limits := loopLimits{MinPeriod: 32, MinRepeat: 512, CheckEvery: 64}
	chunks := loopRepeated(loopChatFrame, "An answer that starts normally.\n", line, 40)

	_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", limits), chunks)
	if verdict == nil {
		t.Fatal("no verdict with a 512-byte threshold")
	}
	loopCheckVerdictShape(t, verdict, len(line))
}

// loopLine is a deterministic line of the requested length with no period of its
// own, so a stream of it can only be confirmed at that length.
func loopLine(length int) string {
	var line strings.Builder
	for i := 0; line.Len() < length; i++ {
		fmt.Fprintf(&line, "%d ", (i*7919)%1013)
	}
	return line.String()[:length]
}

// loopFiller is a non-repeating argument fragment of the requested length, so a
// parallel call streaming it can never look periodic itself.
func loopFiller(call, round, length int) string {
	var text strings.Builder
	for i := 0; text.Len() < length; i++ {
		fmt.Fprintf(&text, "%d-%d-%d,", call, round, (i*7919+round*13)%1013)
	}
	return text.String()[:length]
}

func TestLoopIgnoresOrdinaryRepetition(t *testing.T) {
	var table strings.Builder
	table.WriteString("| provider | tokens/s | ttft |\n| --- | --- | --- |\n")
	for i := range 40 {
		fmt.Fprintf(&table, "| provider-%d | %d.%d | %dms |\n", i, 40+i, i%10, 300+i)
	}
	var csv strings.Builder
	csv.WriteString("date,model,provider,requests,tokens\n")
	for i := range 40 {
		fmt.Fprintf(&csv, "2026-02-%02d,model-%d,provider-%d,%d,%d\n", i%28+1, i, i%7, 10+i, 1000+i*37)
	}
	block := "func handle%d(req *http.Request) error {\n" +
		"\tif req == nil {\n\t\treturn errNoRequest\n\t}\n" +
		"\treturn route%d(req)\n}\n\n"

	cases := []struct{ name, text string }{
		{"markdown table", table.String()},
		{"csv rows", csv.String()},
		{"repeated code block", fmt.Sprintf(block, 1, 1) + "The next helper mirrors the first.\n" +
			fmt.Sprintf(block, 2, 2) + "A third helper does the same work for the tail.\n" + fmt.Sprintf(block, 3, 3)},
		{"prose", "Orr picks a provider per model and keeps pinning it until a benchmark says otherwise. " +
			"The dashboard shows the measurement that decided the pin, and the routing table shows every " +
			"endpoint the catalog reported. None of that is repeated here, because an ordinary answer " +
			"says each thing once and moves on to the next.\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var chunks []string
			for _, line := range strings.SplitAfter(testCase.text, "\n") {
				if line != "" {
					chunks = append(chunks, loopChatFrame(line))
				}
			}
			full := strings.Join(chunks, "")
			released, verdict := loopFeedAll(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
			if verdict != nil {
				t.Fatalf("false positive: channel %d, period %d, suffix %d", verdict.Channel, verdict.Period, verdict.Suffix)
			}
			if released != full {
				t.Fatalf("released %d bytes, want all %d", len(released), len(full))
			}
		})
	}
}

func TestLoopCleanStreamReleasesEveryFrameImmediately(t *testing.T) {
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{})
	for i := range 300 {
		frame := loopChatFrame(fmt.Sprintf("Token %d continues an ordinary sentence with new words %d.\n", i, i*i+7))
		released, verdict := inspector.Feed([]byte(frame))
		if verdict != nil {
			t.Fatalf("frame %d produced a verdict", i)
		}
		if string(released) != frame {
			t.Fatalf("frame %d released %q, want it immediately", i, released)
		}
		if inspector.Suspect() {
			t.Fatalf("frame %d is suspicious", i)
		}
	}
	if tail := loopFlush(inspector); len(tail) != 0 {
		t.Fatalf("flush released %q, want nothing", tail)
	}
}

func TestLoopWithdrawsUnconfirmedSuspicion(t *testing.T) {
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{})
	var chunks []string
	for i := range 20 {
		chunks = append(chunks, loopChatFrame(fmt.Sprintf("Filling the answer with ordinary text number %d before anything repeats.\n", i)))
	}
	// Seven copies of one line look periodic enough for tier 1 but stay short of
	// the 512-byte suffix tier 2 needs.
	line := "A short line that appears seven times and then stops for good.\n"
	for range 7 {
		chunks = append(chunks, loopChatFrame(line))
	}
	for i := range 120 {
		chunks = append(chunks, loopChatFrame(fmt.Sprintf("Then the answer moves on with sentence %d, which differs from every other.\n", i)))
	}
	full := strings.Join(chunks, "")

	suspicious := false
	var released strings.Builder
	var verdict *loopVerdict
	for _, chunk := range chunks {
		out, found := inspector.Feed([]byte(chunk))
		released.Write(out)
		if found != nil {
			verdict = found
		}
		if inspector.Suspect() {
			suspicious = true
		}
	}
	released.WriteString(loopFlush(inspector))

	if verdict != nil {
		t.Fatalf("verdict for an unconfirmed candidate: period %d, suffix %d", verdict.Period, verdict.Suffix)
	}
	if !suspicious {
		t.Fatal("tier 1 never became suspicious, so the withdrawal path was not exercised")
	}
	if released.String() != full {
		t.Fatalf("released %d bytes, want all %d", released.Len(), len(full))
	}
}

func TestLoopReassemblesFramesSplitAcrossChunks(t *testing.T) {
	chunks := []string{loopChatFrame("First paragraph of the answer.\n")}
	for i := range 20 {
		chunks = append(chunks, loopChatFrame(fmt.Sprintf("Paragraph %d continues with distinct wording.\n", i)))
	}
	chunks = append(chunks, "data: [DONE]\n\n")
	full := strings.Join(chunks, "")

	inspector := newLoopInspector("/v1/chat/completions", loopLimits{})
	var released strings.Builder
	for _, chunk := range []byte(full) {
		out, verdict := inspector.Feed([]byte{chunk})
		if verdict != nil {
			t.Fatalf("verdict on a clean stream")
		}
		released.Write(out)
	}
	released.WriteString(loopFlush(inspector))
	if released.String() != full {
		t.Fatalf("released %q, want the whole stream", released.String())
	}
}

func TestLoopDetectsLoopFedOneByteAtATime(t *testing.T) {
	line := "The same sentence is written again because the loop will not stop.\n"
	chunks := []string{loopChatFrame("Intro text for the answer.\n")}
	for range 30 {
		chunks = append(chunks, loopChatFrame(line))
	}
	full := strings.Join(chunks, "")

	inspector := newLoopInspector("/v1/chat/completions", loopLimits{})
	var released strings.Builder
	var verdict *loopVerdict
	for _, chunk := range []byte(full) {
		out, found := inspector.Feed([]byte{chunk})
		released.Write(out)
		if found != nil {
			verdict = found
			break
		}
	}
	if verdict == nil {
		t.Fatal("no verdict when the stream arrives one byte at a time")
	}
	loopCheckVerdictShape(t, verdict, len(line))
}

// loopSplit cuts a body into chunks of at most size bytes, the way the proxy's
// read buffer does.
func loopSplit(body string, size int) []string {
	var chunks []string
	for len(body) > size {
		chunks = append(chunks, body[:size])
		body = body[size:]
	}
	if body != "" {
		chunks = append(chunks, body)
	}
	return chunks
}

func TestLoopPassesThroughNonSSEBody(t *testing.T) {
	var html strings.Builder
	html.WriteString("<!DOCTYPE html><html><head><title>502 Bad Gateway</title></head><body>\n")
	for i := 0; html.Len() < 15<<10; i++ {
		fmt.Fprintf(&html, "<p>upstream error detail %d from the provider edge</p>\n", i)
	}
	html.WriteString("</body></html>\n")
	var plain strings.Builder
	for i := 0; plain.Len() < 8<<10; i++ {
		fmt.Fprintf(&plain, "upstream request failed after retry %d, giving up\n", i)
	}
	var largePlain strings.Builder
	for i := 0; largePlain.Len() < 256<<10; i++ {
		fmt.Fprintf(&largePlain, "error code: 525 from the edge for request %d\n", i)
	}
	bodies := []struct{ name, body string }{
		{"json object", `{"id":"gen-1","model":"example/model","choices":[{"message":{"role":"assistant",` +
			`"content":"A non-streaming answer, which has no frames to align on."}}],` +
			`"usage":{"prompt_tokens":5,"completion_tokens":12}}`},
		{"json array", `[{"index":0,"embedding":[0.1,0.2]}]`},
		{"html error page", html.String()},
		{"plain text", plain.String()},
		{"bom json error", "\ufeff" + `{"error":{"message":"no API key configured"}}`},
		// Bodies that start with a letter an event-stream field also starts with, so
		// the head has to be read as far as the field name before it can be decided.
		{"plain text error prefix", "error: upstream request timed out after 60s"},
		{"dial error", "dial tcp 10.0.0.1:443: connect: connection refused"},
		{"large plain text", largePlain.String()},
	}
	for _, testCase := range bodies {
		t.Run(testCase.name, func(t *testing.T) {
			inspector := newLoopInspector("/v1/chat/completions", loopLimits{})
			for _, chunk := range loopSplit(testCase.body, 4<<10) {
				released, verdict := inspector.Feed([]byte(chunk))
				if verdict != nil {
					t.Fatal("verdict on a non-SSE body")
				}
				if string(released) != chunk {
					t.Fatalf("released %d of %d bytes in the same Feed call: a non-SSE body is not passed through",
						len(released), len(chunk))
				}
			}
			if inspector.Suspect() {
				t.Fatal("suspicious about a non-SSE body")
			}
			if tail := loopFlush(inspector); tail != "" {
				t.Fatalf("flush released %d bytes, want nothing left behind", len(tail))
			}
		})
	}
}

func TestLoopWithdrawsQuietCandidateAtTheLagCap(t *testing.T) {
	// A quiet candidate is abandoned by two independent bounds: text from the other
	// channels (loopQuietGraceBytes) and the bytes withheld from the client
	// (loopHoldLagBytes). Tiny foreign deltas never reach the text bound, so only the
	// lag cap can end this candidate, and it is what keeps a quiet candidate from
	// freezing the client for minutes on a slow stream.
	const lagCap = loopHoldLagBytes
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{MinRepeat: loopRingBytes + loopMinSuffixBytes})
	line := "A line that repeats while the content channel is still producing text.\n"
	var chunks, texts []string
	for range 8 {
		chunks = append(chunks, loopChatFrame(strings.Repeat(line, 4)))
		texts = append(texts, "")
	}
	for i := range 4000 {
		text := fmt.Sprintf("%d", i)
		chunks = append(chunks, loopChatReasoningFrame(text))
		texts = append(texts, text)
	}
	full := strings.Join(chunks, "")

	var released strings.Builder
	suspicious := false
	foreign := 0
	maxHeld := 0
	var verdict *loopVerdict
	for index, chunk := range chunks {
		out, found := inspector.Feed([]byte(chunk))
		released.Write(out)
		if found != nil {
			verdict = found
		}
		if inspector.Suspect() {
			suspicious = true
			foreign += len(texts[index])
		}
		if inspector.held.size() > maxHeld {
			maxHeld = inspector.held.size()
		}
	}
	if verdict != nil {
		t.Fatalf("verdict with confirmation disabled: period %d", verdict.Period)
	}
	t.Logf("withheld at most %d bytes (cap %d) with %d bytes of foreign text (budget %d)",
		maxHeld, lagCap, foreign, loopQuietGraceBytes)
	if !suspicious {
		t.Fatal("tier 1 never became suspicious, so the lag cap was not exercised")
	}
	if inspector.Suspect() {
		t.Fatal("still withholding frames after the candidate's channel went quiet")
	}
	if maxHeld > lagCap {
		t.Fatalf("withheld %d bytes before withdrawing, want at most %d", maxHeld, lagCap)
	}
	if maxHeld < lagCap/2 {
		t.Fatalf("withheld only %d bytes before withdrawing: the lag cap did not end the candidate", maxHeld)
	}
	if foreign >= loopQuietGraceBytes {
		t.Fatalf("%d bytes of foreign text is over the quiet budget, so this test does not exercise the lag cap", foreign)
	}
	if released.String() != full {
		t.Fatalf("released %d bytes before EOF, want all %d", released.Len(), len(full))
	}
	if tail := loopFlush(inspector); tail != "" {
		t.Fatalf("flush released %d withheld bytes", len(tail))
	}
}

func TestLoopPassesThroughUnsupportedPath(t *testing.T) {
	frame := `data: {"object":"list","data":[{"embedding":[0.1,0.2]}]}` + "\n\n"
	inspector := newLoopInspector("/v1/embeddings", loopLimits{})
	released, verdict := inspector.Feed([]byte(frame))
	if verdict != nil || string(released) != frame {
		t.Fatalf("released %q (verdict %v), want the frame straight through", released, verdict)
	}
}

func TestLoopIgnoresWhitespaceRepetition(t *testing.T) {
	chunks := []string{}
	for range 2000 {
		chunks = append(chunks, loopChatFrame("\n"))
	}
	full := strings.Join(chunks, "")
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{})
	released, verdict := loopFeedAll(t, inspector, chunks)
	if verdict != nil {
		t.Fatalf("verdict for repeated blank lines: period %d", verdict.Period)
	}
	if released != full {
		t.Fatalf("released %d bytes, want all %d", len(released), len(full))
	}
}

func TestLoopObservedStreamIsDeliveredInFull(t *testing.T) {
	line := "This line repeats for ever and the client should still see all of it.\n"
	chunks := []string{loopChatFrame("An answer that starts normally.\n")}
	for range 30 {
		chunks = append(chunks, loopChatFrame(line))
	}
	full := strings.Join(chunks, "")

	released, verdict := loopFeedAll(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a content loop")
	}
	if released != full {
		t.Fatalf("released %d bytes, want all %d in observe mode", len(released), len(full))
	}
}

func TestLoopTerminatesOnDoneFrame(t *testing.T) {
	chunks := []string{
		loopChatFrame("A short answer.\n"),
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	full := strings.Join(chunks, "")
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{})
	released, verdict := loopFeedAll(t, inspector, chunks)
	if verdict != nil {
		t.Fatal("verdict at the end of a short stream")
	}
	if released != full {
		t.Fatalf("released %q, want the whole stream", released)
	}
	if tail := loopFlush(inspector); len(tail) != 0 {
		t.Fatalf("flush released %q after [DONE], want nothing", tail)
	}
}

func TestLoopHandlesCRLFFrames(t *testing.T) {
	line := "Carriage returns separate these frames but the loop is the same one.\n"
	chunks := []string{strings.ReplaceAll(loopChatFrame("Intro.\n"), "\n", "\r\n")}
	for range 30 {
		chunks = append(chunks, strings.ReplaceAll(loopChatFrame(line), "\n", "\r\n"))
	}
	_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a CRLF-delimited loop")
	}
	loopCheckVerdictShape(t, verdict, len(line))
}

func TestLoopLimitsOverrideDefaults(t *testing.T) {
	line := "Short line of forty characters or so.\n"
	chunks := loopRepeated(loopChatFrame, "An intro line that sets the scene.\n", line, 40)
	inspector := newLoopInspector("/v1/chat/completions", loopLimits{})
	_, verdict := loopFeedUntilVerdict(t, inspector, chunks)
	if verdict == nil {
		t.Fatal("no verdict with the default limits")
	}
	loopCheckVerdictShape(t, verdict, len(line))

	// A minimum period above the repeated unit is still satisfiable, because a
	// multiple of a period is a period; the reported one must respect the floor.
	strict := newLoopInspector("/v1/chat/completions", loopLimits{MinPeriod: len(line) + 16})
	_, floored := loopFeedUntilVerdict(t, strict, chunks)
	if floored == nil {
		t.Fatal("no verdict with a minimum period above the repeated unit")
	}
	if floored.Period < len(line)+16 || floored.Period%len(line) != 0 {
		t.Fatalf("period = %d, want a multiple of %d at least %d", floored.Period, len(line), len(line)+16)
	}

	// Requiring more periodic bytes than the ring can hold rules the loop out: a
	// suffix longer than the ring is never measured.
	greedy := newLoopInspector("/v1/chat/completions", loopLimits{MinRepeat: loopRingBytes + loopMinSuffixBytes})
	if _, found := loopFeedUntilVerdict(t, greedy, chunks); found != nil {
		t.Fatalf("verdict with a %d-byte threshold over an %d-byte ring", loopRingBytes+loopMinSuffixBytes, loopRingBytes)
	}
}

// loopChatReasoningDetailFrame is one structured reasoning item, the shape a client
// that asks for reasoning passthrough is answered with.
func loopChatReasoningDetailFrame(text string) string {
	return `data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","text":` +
		loopQuoted(text) + `}]}}]}` + "\n\n"
}

// loopChatEncryptedReasoningFrame is a thought the provider keeps encrypted: the
// client is handed a blob it can only give back, never prose.
func loopChatEncryptedReasoningFrame(data string) string {
	return `data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":` +
		loopQuoted(data) + `}]}}]}` + "\n\n"
}

// loopResponsesSummaryFrame is one reasoning summary delta, which is how the
// Responses API streams a thought it will not send in full.
func loopResponsesSummaryFrame(text string) string {
	return `data: {"type":"response.reasoning_summary_text.delta","delta":` + loopQuoted(text) + `}` + "\n\n"
}

func TestLoopDetectsStructuredReasoningLoop(t *testing.T) {
	intro := "Checking the routing table again.\n"
	line := "Thinking the same thought again because the trace keeps returning.\n"
	chunks := loopRepeated(loopChatReasoningDetailFrame, intro, line, 30)

	_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a structured reasoning loop")
	}
	if verdict.Channel != loopChannelReasoning {
		t.Fatalf("channel = %d, want reasoning", verdict.Channel)
	}
	loopCheckVerdictShape(t, verdict, len(line))
	if verdict.Prefix != intro {
		t.Fatalf("prefix = %q, want %q", verdict.Prefix, intro)
	}
}

func TestLoopDetectsReasoningSummaryLoop(t *testing.T) {
	line := "The same summary keeps coming back because the trace keeps returning.\n"
	chunks := loopRepeated(loopResponsesSummaryFrame, "Working through the diff.\n", line, 30)

	_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/responses", loopLimits{}), chunks)
	if verdict == nil {
		t.Fatal("no verdict for a reasoning-summary loop")
	}
	if verdict.Channel != loopChannelReasoning {
		t.Fatalf("channel = %d, want reasoning", verdict.Channel)
	}
	loopCheckVerdictShape(t, verdict, len(line))
}

func TestLoopIgnoresEncryptedReasoning(t *testing.T) {
	// An encrypted thought is opaque: the same blob arriving forty times is not a
	// repetition of anything the detector can read, and measuring it would put base64
	// in the ring the prose is compared in.
	blob := "gAAAAABm5T0Xr0m0b2R5IGNoYW5nZWQgdGhpcyB0ZXh0IGludG8gYSBibG9iIGZvciB0aGUgdGVzdA=="
	chunks := make([]string, 0, 40)
	for range 40 {
		chunks = append(chunks, loopChatEncryptedReasoningFrame(blob))
	}

	released, verdict := loopFeedAll(t, newLoopInspector("/v1/chat/completions", loopLimits{}), chunks)
	if verdict != nil {
		t.Fatalf("verdict on encrypted reasoning: %d bytes repeated %d times", verdict.Period, verdict.Repeat)
	}
	// The frames still reach the client untouched: ignoring the text is not dropping it.
	if got := strings.Count(released, blob); got != 40 {
		t.Fatalf("released %d of 40 encrypted frames", got)
	}
}

func TestLoopVerdictReportsWhatTheClientHolds(t *testing.T) {
	// Generated decides whether a thinking loop can be restarted, so blank prose has to
	// count as nothing and a delivered tool call as something.
	line := "Thinking the same thought again because the trace keeps returning.\n"
	thinkingLoop := func() []string {
		chunks := []string{loopChatReasoningFrame("Checking the routing table.\n")}
		for range 30 {
			chunks = append(chunks, loopChatReasoningFrame(line))
		}
		return chunks
	}

	blank := append([]string{loopChatFrame("\n\n"), loopChatFrame("   ")}, thinkingLoop()...)
	_, verdict := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), blank)
	if verdict == nil {
		t.Fatal("no verdict for the thinking loop")
	}
	if verdict.Generated {
		t.Fatal("blank answer text counted as something the client holds, so the thinking loop could never be restarted")
	}

	answered := append([]string{loopChatFrame("Here is the beginning of the answer.\n")}, thinkingLoop()...)
	_, withAnswer := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), answered)
	if withAnswer == nil {
		t.Fatal("no verdict for the thinking loop after the answer began")
	}
	if !withAnswer.Generated {
		t.Fatal("answer text was not reported, so a restart would append a second answer")
	}

	// A tool call the client already holds is an action it may be taking: a restart there
	// would ask for a second call to go with it.
	tooled := append([]string{loopChatToolFrame(0, `{"path":"internal/app/loop.go"}`)}, thinkingLoop()...)
	_, withCall := loopFeedUntilVerdict(t, newLoopInspector("/v1/chat/completions", loopLimits{}), tooled)
	if withCall == nil {
		t.Fatal("no verdict for the thinking loop after a tool call")
	}
	if !withCall.Generated {
		t.Fatal("a delivered tool call was not reported, so a restart would ask for a second one")
	}
}
