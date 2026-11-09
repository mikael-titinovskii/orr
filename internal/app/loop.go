package app

import (
	"bytes"
	"encoding/json"
	"strings"
)

// loopChannel names the streamed field a repetition was found in. Detection is
// per channel: a model that loops in its reasoning has not necessarily looped in
// its answer, and the text of one channel cannot repeat the text of another.
type loopChannel int

const (
	loopChannelNone loopChannel = iota
	loopChannelContent
	loopChannelReasoning
	loopChannelToolArgs
)

// loopVerdict is a confirmed self-repetition of one channel.
//
// Period is the length in bytes of the repeated unit, Repeat how many times it
// repeats at the end of the text, and Suffix the bytes that periodicity covers:
// Suffix == Period*Repeat. Prefix is the channel's text before the repetition
// began, because a recovery re-sends it as the assistant's own earlier turn. For a
// tool-argument loop the prefix is capped at the 4 KiB tool-call window and can be a
// mid-JSON fragment, so a recovery must not re-send it as an assistant turn.
type loopVerdict struct {
	Channel loopChannel
	Period  int
	Repeat  int
	Suffix  int
	Prefix  string
	// Generated reports that the response has already produced text the client can read
	// or act on — answer prose, or a tool call's arguments. A thinking loop is restarted
	// rather than continued, which is only safe while nothing has been generated,
	// because a restart would append a second answer, or a second tool call, to what the
	// client already holds. The policy that decides whether to spend a continuation
	// reads it.
	Generated bool
	// ToolCall names the tool call whose arguments looped, for the tool-argument
	// channel. Parallel calls interleave, so the release has to charge only that
	// call's text - the same subset the withheld count was opened with.
	ToolCall int
	// DroppedText is how many bytes of the clean prefix the client will not
	// receive, because releasing them would have meant releasing part of one
	// frame that also carried the repetition. A continuation must resume from the
	// prefix the client actually has, so the caller trims this much off the end.
	DroppedText int
}

// loopLimits tunes the detector. Zero values fall back to the defaults below, so
// a caller only has to name what it wants to change.
type loopLimits struct {
	// MinPeriod is the shortest repeated unit, in bytes, worth reporting. Shorter
	// units are ordinary language ("the the", "ha ha") rather than a loop.
	MinPeriod int
	// MinRepeat is the minimum periodic suffix, in bytes, that confirms a loop:
	// how far back an exactly repeating run has to reach. It is what bounds the
	// detector's memory, because a suffix longer than the ring it is measured in
	// can never be confirmed. Values below loopMinSuffixBytes are raised to it.
	MinRepeat int
	// CheckEvery is how many new bytes a channel generates between tier-1
	// probes. Tier 2 runs on every delta once a candidate is being verified.
	CheckEvery int
}

const (
	loopMinPeriodDefault  = 32
	loopCheckEveryDefault = 64
	// loopMinRepeatCount is how many times the unit has to repeat whatever the
	// byte threshold allows: a block that appears once is not a loop.
	loopMinRepeatCount = 4

	// loopArmBytes is how much a channel must generate before tier 1 looks for
	// repetition at all: a short answer cannot carry a 512-byte periodic suffix
	// anyway, so early text is never held back.
	loopArmBytes = 1 << 10
	// loopProbeBytes is the tail tier 1 searches an earlier copy of. A 64-byte
	// match is too specific to happen by chance in a line of prose.
	loopProbeBytes = 64
	// loopMinSuffixBytes is the shortest exactly periodic suffix tier 2 accepts,
	// whatever the period. Below this a repeated phrase is still just prose.
	loopMinSuffixBytes = 512
	// loopMaxPeriod bounds the repeated unit tier 1 considers. A longer unit is
	// not the same-lines-until-max_tokens loop this detector exists for.
	loopMaxPeriod = 2 << 10

	loopRingBytes    = 8 << 10
	loopPrefixBytes  = 32 << 10
	loopHoldBytes    = 256 << 10
	loopPendingBytes = 128 << 10
	// loopToolCallStates is how many tool calls may stream arguments at once with
	// their own text state. Parallel tool calls are interleaved, so they cannot
	// share a ring, and this is the detector's real limit: with more calls in
	// flight than this, every delta stages a new index and evicts the least
	// recently used one, so no call's ring ever reaches loopArmBytes and
	// tool-argument detection is off for all of them — not just for the evicted
	// call. Eight covers the parallel calls agentic clients are seen to emit.
	loopToolCallStates = 8
	// loopToolCallPrefixBytes is the prefix window of a staged tool call. Its
	// arguments only have to be kept long enough to report as Prefix, so it is
	// smaller than a channel's; the ring stays loopRingBytes because tier 2
	// measures the ring.
	loopToolCallPrefixBytes = 4 << 10
	// loopHoldGraceBytes is how much text the candidate's own channel may generate
	// after tier 1 became suspicious without tier 2 confirming it. Past that the
	// candidate is abandoned and the withheld frames are released. Only the
	// candidate's own text is charged: tier 2 can only ever be satisfied by that
	// text, so a parallel tool call's arguments or the other channel's prose say
	// nothing about whether this candidate is a loop. A false candidate that resolves
	// just before a real loop is inherent to the design: the real loop's first frames
	// were withheld with it and go out when it withdraws, so up to one grace window of
	// loop text can reach the client before the next verdict.
	loopHoldGraceBytes = 4 << 10
	// loopQuietGraceBytes is how much text every other channel may generate while a
	// candidate generates none before that candidate is abandoned too. It measures
	// silence, not traffic: the counter is reset whenever the candidate's own channel
	// produces text. Without it a candidate whose channel went quiet — a reasoning
	// candidate followed only by content, a tool call that stopped streaming — would
	// keep its frames withheld until the queue evicted them. It is far larger than
	// loopHoldGraceBytes because parallel tool calls interleave a round apart: a round
	// of seven deltas must not end a candidate that is still repeating. It is set
	// below loopHoldLagBytes because a withheld frame is always larger than the text it
	// carries: with lean frames this bound fires first and ends a candidate that has
	// been silent while others talked, and with padded frames the byte bound does.
	loopQuietGraceBytes = 48 << 10
	// loopHoldLagBytes is how many bytes may be withheld from the client before the
	// candidate holding them is abandoned. The quiet budget bounds how much other
	// text a candidate may outlive; this bounds how long the client sees nothing,
	// which is what keeps a quiet candidate from freezing a slow stream for minutes.
	// It is well under loopHoldBytes on purpose: the queue's own eviction is the last
	// resort, not the rule, because the client receiving withheld frames late beats
	// the client receiving them at EOF.
	loopHoldLagBytes = 64 << 10
)

// loopInspector watches one upstream response as it streams past. It is fed the
// raw bytes the proxy read and answers with the bytes that may now be written to
// the client, so a confirmed loop can be cut before it reaches the client.
//
// Detection has two tiers. Tier 1 is cheap and looks for an earlier copy of the
// text's tail; a candidate period it finds makes the inspector withhold every
// frame from that point on. Tier 2 proves exact periodicity of a bounded text
// ring for that candidate and only then reports a verdict. A candidate tier 2
// never confirms is withdrawn, and the withheld frames are released again, so
// text that merely resembles a loop is still delivered in full.
//
// Memory is bounded per inspector and does not grow with the response. Live window
// limits are three channel states (loopRingBytes + loopPrefixBytes = 40 KiB),
// eight tool-call states (loopRingBytes + loopToolCallPrefixBytes = 12 KiB), the
// withheld-frame queue of loopHoldBytes and the 128 KiB reassembly buffer: 600 KiB
// in all. A textWindow compacts once half of it is dead, so a window holds at most
// 1.5 times its limit plus the text being appended, and the reassembly slice can
// hold twice its cap while it grows, which puts the ceiling at about 0.95 MiB. No
// response text is logged or persisted here.
type loopInspector struct {
	path    string
	limits  loopLimits
	enabled bool

	// pending is the bytes of the frame being reassembled, and deltas the reused
	// scratch of the last extract call.
	pending []byte
	deltas  []loopDelta

	channels [3]loopChannelState
	// toolCalls is one text state per staged tool call. Parallel tool calls
	// stream interleaved, so each call's arguments need their own ring; the least
	// recently used one is reused when a response opens more calls at once than
	// loopToolCallStates, which is where detection of tool-argument loops stops.
	toolCalls [loopToolCallStates]loopToolCallState
	toolSeq   uint64

	// held is the bytes withheld from the client while a candidate is unproven.
	held        textWindow
	holding     bool
	holdingText int
	// frameText counts the text this frame has already produced, keyed by channel and -
	// for tool arguments - the exact call it belongs to: parallel calls interleave inside
	// one frame and must never be added together, and folding a call onto a shared bucket
	// charges one call's arguments to another. Cleared per frame and reused.
	frameText map[loopTextKey]int
	// candidateToolCall is the tool call whose arguments armed the candidate, so the
	// verdict can name the bucket the release must charge.
	candidateToolCall int
	// releasedBefore counts the clean-prefix text releaseBefore has already
	// handed back, so a second call cannot re-derive the boundary and release
	// loop text the client must never see.
	releasedBefore int
	// quietText counts text generated by everything except the candidate since the
	// candidate last generated text, so a candidate whose own channel went quiet is
	// still abandoned and a candidate that keeps generating is not.
	quietText   int
	candidate   int
	candidateCh loopChannel
	// candidateOn is the state that proposed the candidate. Parallel tool calls
	// interleave, so confirmation has to run against that state rather than
	// against whichever channel the current delta happens to belong to.
	candidateOn *loopChannelState

	headChecked bool
	// generated counts the bytes the response has produced that a client can read or act
	// on — answer text and tool-call arguments — whether or not they have been released
	// yet. A thinking loop in a response that already produced any of it cannot be
	// restarted, so the count is monotonic: withdrawing a candidate or dropping an
	// oversized frame forgets text, not the fact that the client has something.
	generated int
}

// loopDelta is one piece of generated text lifted out of one SSE frame.
type loopDelta struct {
	channel  loopChannel
	text     string
	toolCall int
	numbered bool
}

// slot is the bucket a delta's text belongs to inside one frame: its channel, plus the
// tool call it carries. Parallel tool calls stream in the same frame, so their texts are
// separate and must not be summed into one another.
// loopTextKey names one accounting bucket: a channel, plus the tool call whose arguments
// it carries. The exact index is kept rather than folded onto a shared slot, so two
// calls can never be charged to each other - including calls past the staging cap.
type loopTextKey struct {
	channel loopChannel
	call    int
}

func (d loopDelta) key() loopTextKey {
	if d.channel == loopChannelToolArgs {
		return loopTextKey{channel: d.channel, call: d.toolCall}
	}
	return loopTextKey{channel: d.channel}
}

// verdictKey is the bucket a verdict's withheld text belongs to.
func (v *loopVerdict) verdictKey() loopTextKey {
	if v.Channel == loopChannelToolArgs {
		return loopTextKey{channel: v.Channel, call: v.ToolCall}
	}
	return loopTextKey{channel: v.Channel}
}

// loopToolCallState is one staged tool call's text state.
type loopToolCallState struct {
	loopChannelState
	index  int
	used   uint64
	staged bool
}

// loopChannelState is one channel's bounded view of its generated text.
type loopChannelState struct {
	// ring is the recent text periodicity is looked for in.
	ring textWindow
	// prefix is the whole text, capped from the front, kept only so a verdict can
	// report what came before the repetition.
	prefix textWindow
	// since counts bytes generated since the last tier-1 probe, and cooldown the
	// bytes still owed before tier 1 may arm again after a withdrawn candidate.
	since    int
	cooldown int
}

func newLoopChannelState(prefixBytes int) loopChannelState {
	return loopChannelState{
		ring:   textWindow{limit: loopRingBytes},
		prefix: textWindow{limit: prefixBytes},
	}
}

// newLoopInspector returns an inspector for the deltas of the streaming API at
// path. A path without a known delta shape gets an inspector that passes
// everything straight through, so callers never have to special-case a body they
// cannot inspect. The returned value must not be nil.
func newLoopInspector(path string, limits loopLimits) *loopInspector {
	if limits.MinPeriod <= 0 {
		limits.MinPeriod = loopMinPeriodDefault
	}
	if limits.MinRepeat <= 0 {
		limits.MinRepeat = loopMinSuffixBytes
	}
	if limits.CheckEvery <= 0 {
		limits.CheckEvery = loopCheckEveryDefault
	}
	i := &loopInspector{path: path, limits: limits, enabled: mutablePaths[path]}
	for ch := range i.channels {
		i.channels[ch] = newLoopChannelState(loopPrefixBytes)
	}
	for n := range i.toolCalls {
		i.toolCalls[n].loopChannelState = newLoopChannelState(loopToolCallPrefixBytes)
	}
	i.held.limit = loopHoldBytes
	i.frameText = make(map[loopTextKey]int, 4)
	return i
}

// Suspect reports whether tier 1 is holding frames back while tier 2 decides.
func (i *loopInspector) Suspect() bool {
	return i.holding
}

// Feed takes the next raw upstream bytes and returns the bytes that may now be
// written to the client, plus a verdict when a repetition was just confirmed.
//
// The returned bytes are always whole SSE frames in the order they arrived. A
// clean stream is released as it arrives, one frame at a time, with no added
// lag. Once a frame makes tier 1 suspicious, that frame and everything after it
// are withheld, so the release lags the input until the candidate is confirmed
// (the verdict is returned and the withheld frames stay behind) or abandoned (the
// withheld frames are returned here, in order).
//
// After a verdict the inspector stops inspecting. A caller that keeps feeding it
// — the observe behaviour — gets the withheld frames back with the next Feed, so
// the whole stream is still delivered; a caller that stops feeding drops them,
// which is what cutting a loop means.
//
// A body that is not SSE, and any frame without a delta this package knows, pass
// straight through without detection or buffering. A frame too large to reassemble
// is released and detection resumes behind it, rather than being switched off for
// the rest of the response. Feed(nil) flushes what is still buffered: an
// incomplete trailing frame, and frames still withheld.
func (i *loopInspector) Feed(chunk []byte) ([]byte, *loopVerdict) {
	if len(chunk) == 0 {
		return i.flush(), nil
	}
	if !i.enabled {
		if withheld := i.takeHeld(); len(withheld) > 0 {
			return append(withheld, chunk...), nil
		}
		return chunk, nil
	}

	var release []byte
	i.pending = append(i.pending, chunk...)
	consumed := 0
	for consumed < len(i.pending) {
		length := cutFrame(i.pending[consumed:])
		if length == 0 {
			break
		}
		frame := i.pending[consumed : consumed+length]
		consumed += length
		released, verdict := i.processFrame(frame)
		release = append(release, released...)
		if verdict != nil {
			// Nothing after a confirmed loop may reach the client: the rest of
			// this chunk joins what was withheld, and inspection ends here.
			i.held.add(i.pending[consumed:])
			i.pending = nil
			i.enabled = false
			return release, verdict
		}
	}
	if consumed > 0 {
		i.pending = append(i.pending[:0], i.pending[consumed:]...)
	}
	if !i.headChecked {
		if sse, decided := sseHead(i.pending); decided {
			i.headChecked = true
			if !sse {
				// Not an event stream: an error page, a plain-text body, a JSON
				// document. Nothing to align frames on, so it is handed over now
				// rather than at EOF.
				return append(release, i.stopInspection()...), nil
			}
		}
	}
	if len(i.pending) > loopPendingBytes {
		// A frame larger than this has not been reassembled yet. Release what is
		// buffered and keep inspecting: one oversized delta must not silently
		// disable detection for the rest of the response.
		release = append(release, i.overflow()...)
	}
	return release, nil
}

// processFrame folds one whole SSE frame's text into its channel and then either
// releases the frame or withholds it.
func (i *loopInspector) processFrame(frame []byte) ([]byte, *loopVerdict) {
	var released []byte
	var verdict *loopVerdict
	clear(i.frameText)
	for _, delta := range i.extract(frame) {
		freed, found := i.observe(delta)
		released = append(released, freed...)
		if found != nil {
			verdict = found
			break
		}
		i.frameText[delta.key()] += len(delta.text)
	}
	if verdict != nil || i.holding {
		released = append(released, i.hold(frame)...)
		if verdict == nil && i.held.size() > loopHoldLagBytes {
			// The queue's own bound: a candidate that has taken this much from the
			// client gives it back, whatever the other channels are doing.
			released = append(released, i.withdraw()...)
		}
		return released, verdict
	}
	return append(released, frame...), verdict
}

// observe folds one delta's text into its channel and runs the detector over it.
// Bytes it returns are withheld frames that may now be released, because a
// candidate that could not be confirmed was withdrawn.
func (i *loopInspector) observe(delta loopDelta) ([]byte, *loopVerdict) {
	state := i.stateFor(delta)
	state.add(delta.text)
	switch {
	case delta.channel == loopChannelContent:
		// Blank text is not an answer: a model that emitted a paragraph break before
		// looping in its thinking has left the reader nothing for a second answer to
		// follow.
		if strings.TrimSpace(delta.text) != "" {
			i.generated += len(delta.text)
		}
	case delta.channel == loopChannelToolArgs:
		// A tool call the client already holds is an action it may be taking, so a restart
		// must not ask for a second one to go with it.
		i.generated += len(delta.text)
	}
	if i.holding {
		if state == i.candidateOn {
			// Only the candidate's own text can satisfy tier 2, so only that text is
			// charged to the budget a candidate gets to confirm in. Its own text also
			// ends the silence the quiet budget measures: a response whose other
			// channels are busy cannot spend that budget while the candidate keeps up.
			i.holdingText += len(delta.text)
			i.quietText = 0
			if verdict := i.confirm(i.candidateCh, i.candidateOn); verdict != nil {
				release := i.releaseBefore(verdict)
				i.holding = false
				return release, verdict
			}
			if i.holdingText > loopHoldGraceBytes {
				return i.withdraw(), nil
			}
			return nil, nil
		}
		// Everything else counts against the quiet budget instead, which measures how
		// much text arrives while the candidate generates none: a candidate whose
		// channel stopped generating must not keep frames withheld for ever.
		i.quietText += len(delta.text)
		if i.quietText > loopQuietGraceBytes {
			return i.withdraw(), nil
		}
		return nil, nil
	}
	if state.cooldown > 0 {
		state.cooldown -= len(delta.text)
		if state.cooldown > 0 {
			return nil, nil
		}
	}
	if state.ring.size() < loopArmBytes || state.since < i.limits.CheckEvery {
		return nil, nil
	}
	state.since = 0
	period := i.candidatePeriod(state)
	if period == 0 {
		return nil, nil
	}
	i.candidate, i.candidateCh, i.candidateOn = period, delta.channel, state
	i.candidateToolCall = delta.toolCall
	// The arming frame is withheld with the candidate, so its text is part of what the
	// client has not seen: counting from zero would under-report the withheld region and
	// leave a trimmed prefix past the bytes the client actually received.
	i.holding, i.holdingText, i.quietText, i.releasedBefore = true,
		i.frameText[delta.key()]+len(delta.text), 0, 0
	if verdict := i.confirm(i.candidateCh, i.candidateOn); verdict != nil {
		release := i.releaseBefore(verdict)
		i.holding = false
		return release, verdict
	}
	return nil, nil
}

// stateFor is the text state a delta belongs to. Tool-call arguments are one
// document per call, so parallel tool calls streaming interleaved each keep a
// ring of their own.
func (i *loopInspector) stateFor(delta loopDelta) *loopChannelState {
	if !delta.numbered {
		return &i.channels[delta.channel-1]
	}
	return i.toolState(delta.toolCall)
}

// toolState returns the staged state of one tool call, staging it when it is new.
// When every slot is taken the least recently used call is reused: keeping a
// state per index a response names would let a response's memory cost grow with
// the response. Continuous interleaving of more calls than there are slots reuses
// a slot on every delta, so no call accumulates the text tier 1 needs before it
// looks — the honest boundary is loopToolCallStates concurrent calls.
func (i *loopInspector) toolState(index int) *loopChannelState {
	i.toolSeq++
	slot := -1
	for n := range i.toolCalls {
		if i.toolCalls[n].staged && i.toolCalls[n].index == index {
			slot = n
			break
		}
	}
	if slot < 0 {
		slot = 0
		for n := range i.toolCalls {
			if !i.toolCalls[n].staged {
				slot = n
				break
			}
			if i.toolCalls[n].used < i.toolCalls[slot].used {
				slot = n
			}
		}
		i.toolCalls[slot].reset()
		i.toolCalls[slot].index, i.toolCalls[slot].staged = index, true
	}
	i.toolCalls[slot].used = i.toolSeq
	return &i.toolCalls[slot].loopChannelState
}

// candidatePeriod is tier 1: the cheapest evidence that a channel's text may be
// repeating. The last loopProbeBytes of the ring are searched for in the text
// before them, and an earlier copy at distance g says the text repeated with
// period g. That is a suspicion only — ordinary wording repeats at a distance
// too — so tier 2 still has to prove exact periodicity.
func (i *loopInspector) candidatePeriod(state *loopChannelState) int {
	ring := state.ring.text()
	if len(ring) < 2*loopProbeBytes {
		return 0
	}
	probe := ring[len(ring)-loopProbeBytes:]
	if blank(probe) {
		return 0
	}
	window := ring[:len(ring)-loopProbeBytes]
	index := bytes.LastIndex(window, probe)
	if index < 0 {
		return 0
	}
	period := len(window) - index
	if period < i.limits.MinPeriod {
		// A repetition shorter than the minimum period still qualifies at a
		// multiple of itself.
		period *= (i.limits.MinPeriod + period - 1) / period
	}
	if period > loopMaxPeriod || 2*period > len(ring) {
		return 0
	}
	return period
}

// confirm is tier 2: exact periodicity of the ring's tail for the candidate tier
// 1 proposed or a divisor of it. Divisors are tried shortest first so the period
// reported is the unit that actually repeats, not a multiple of it.
func (i *loopInspector) confirm(channel loopChannel, state *loopChannelState) *loopVerdict {
	ring := state.ring.text()
	for period := i.limits.MinPeriod; period <= i.candidate; period++ {
		if i.candidate%period != 0 {
			continue
		}
		suffix := periodicSuffix(ring, period)
		if suffix < i.requiredSuffix() || suffix/period < loopMinRepeatCount || blank(ring[len(ring)-suffix:]) {
			continue
		}
		return &loopVerdict{
			Channel:   channel,
			Period:    period,
			Repeat:    suffix / period,
			Suffix:    suffix,
			Prefix:    i.prefixBefore(state, suffix),
			Generated: i.generated > 0,
			ToolCall:  i.candidateToolCall,
		}
	}
	return nil
}

// requiredSuffix is the shortest periodic suffix that confirms a loop: the
// configured byte threshold, never below loopMinSuffixBytes. It is a length in
// bytes, not a repeat count, because it is also what the caller uses to bound how
// far back detection looks.
func (i *loopInspector) requiredSuffix() int {
	return max(loopMinSuffixBytes, i.limits.MinRepeat)
}

// prefixBefore is the channel's text before the repeating suffix. The prefix
// window keeps the whole generated text, capped from the front at loopPrefixBytes
// (loopToolCallPrefixBytes for a tool call), so this is truncated only by a
// channel that generated more than that before looping.
func (i *loopInspector) prefixBefore(state *loopChannelState, suffix int) string {
	prefix := state.prefix.text()
	if suffix >= len(prefix) {
		return ""
	}
	return string(prefix[:len(prefix)-suffix])
}

// hold withholds one frame until the candidate resolves, and returns any bytes
// the queue had to give up to stay bounded. What it returns is the oldest
// withheld bytes, so the client still sees the stream in order.
func (i *loopInspector) hold(frame []byte) []byte {
	i.held.add(frame)
	return i.held.evict()
}

// withdraw abandons an unconfirmed candidate and hands the withheld frames back.
// releaseBefore hands back the withheld frames whose text lies before the confirmed
// repetition. A candidate can arm on a coincidental repeat well before the real loop,
// so the withheld region holds legitimate prose as well as the periodic suffix; the
// client must receive that prose, because Prefix ends at the loop and the continuation
// resumes from it — dropping the prose would continue an answer the client never saw.
func (i *loopInspector) releaseBefore(verdict *loopVerdict) []byte {
	if verdict == nil {
		return nil
	}
	// Bytes of the candidate's own text the client has not seen yet, minus the
	// repetition itself: what is left predates the loop.
	pending := i.holdingText - verdict.Suffix - i.releasedBefore
	if pending <= 0 {
		return nil
	}
	if i.held.size() == 0 {
		// One delta armed the candidate and confirmed it, so its frame joins the
		// queue only after this call returns: nothing can be released here, and the
		// whole clean prefix of that frame is owed to the continuation's trim.
		verdict.DroppedText = pending
		return nil
	}
	held := i.held.text()
	split, text := 0, 0
	for start := 0; start < len(held); {
		end := cutFrame(held[start:])
		if end == 0 {
			break
		}
		frameText := 0
		want := verdict.verdictKey()
		for _, delta := range i.extract(held[start : start+end]) {
			// Charge only this verdict's own bucket: parallel calls share the
			// tool-argument channel but never the same key, and no index is folded.
			if delta.key() == want {
				frameText += len(delta.text)
			}
		}
		if text+frameText > pending {
			// This frame straddles the boundary: its text begins in the clean prefix
			// and ends inside the repetition, so releasing it would put the first
			// bytes of the loop into the repaired answer. It stays withheld, and the
			// prefix is trimmed instead so the continuation still resumes from text
			// the client actually has.
			break
		}
		text += frameText
		start += end
		split = start
	}
	i.releasedBefore += text
	verdict.DroppedText = pending - text
	if split == 0 {
		return nil
	}
	release := append([]byte(nil), held[:split]...)
	rest := append([]byte(nil), held[split:]...)
	i.held.reset()
	i.held.add(rest)
	return release
}

func (i *loopInspector) withdraw() []byte {
	held := i.takeHeld()
	if i.candidateOn != nil {
		// Give the channel room to move past the text that misled tier 1 before
		// arming again, so a candidate that keeps failing cannot hold back every
		// frame of a long answer.
		i.candidateOn.cooldown = loopHoldGraceBytes
	}
	i.holding, i.holdingText, i.quietText, i.releasedBefore, i.candidate, i.candidateCh = false, 0, 0, 0, 0, loopChannelNone
	i.candidateOn = nil
	return held
}

// stopInspection ends detection and hands back everything still buffered, in
// order: frames withheld by a candidate, then an incomplete trailing frame.
func (i *loopInspector) stopInspection() []byte {
	release := i.withdraw()
	i.enabled = false
	if len(i.pending) > 0 {
		release = append(release, i.pending...)
		i.pending = nil
	}
	return release
}

// overflow releases a reassembly buffer that grew past loopPendingBytes and
// restarts the text state behind it. A frame that large is generated text this
// detector cannot classify, so it is handed to the client untouched and detection
// resumes with what follows, rather than being abandoned for the whole response.
func (i *loopInspector) overflow() []byte {
	release := i.withdraw()
	release = append(release, i.pending...)
	i.pending = nil
	for ch := range i.channels {
		i.channels[ch].reset()
	}
	for n := range i.toolCalls {
		i.toolCalls[n].reset()
		i.toolCalls[n].staged, i.toolCalls[n].index = false, 0
	}
	return release
}

// flush ends a stream: whatever is still buffered is handed over, because there
// is nothing left to inspect.
func (i *loopInspector) flush() []byte {
	release := i.takeHeld()
	if len(i.pending) > 0 {
		release = append(release, i.pending...)
		i.pending = nil
	}
	return release
}

// takeHeld empties the withheld-frame queue and returns its contents.
func (i *loopInspector) takeHeld() []byte {
	if i.held.size() == 0 {
		return nil
	}
	release := append([]byte(nil), i.held.text()...)
	i.held.reset()
	return release
}

// extract lifts the deltas out of one SSE frame. Frames it cannot classify — role
// and usage frames, pings, events of a type this package does not know — yield no
// delta, and the caller releases them untouched.
func (i *loopInspector) extract(frame []byte) []loopDelta {
	i.deltas = i.deltas[:0]
	data := sseData(frame)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return i.deltas
	}
	switch i.path {
	case "/v1/chat/completions":
		i.extractChat(data)
	case "/v1/responses":
		i.extractResponses(data)
	case "/v1/messages":
		i.extractMessages(data)
	}
	return i.deltas
}

// loopReasoningDetail is one structured reasoning item a chat delta can carry: the
// thought as text, as a summary, or as an encrypted blob. An encrypted item is opaque
// to everything but the provider that made it, so it is neither measured nor re-sent:
// text answers nothing for it.
type loopReasoningDetail struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Summary string `json:"summary"`
}

// text is the prose the item carries, if any.
func (d loopReasoningDetail) text() string {
	switch d.Type {
	case "reasoning.text":
		return d.Text
	case "reasoning.summary":
		return d.Summary
	}
	return ""
}

func (i *loopInspector) extractChat(data []byte) {
	var payload struct {
		Choices []struct {
			Delta struct {
				Content          json.RawMessage       `json:"content"`
				Reasoning        string                `json:"reasoning"`
				ReasoningContent string                `json:"reasoning_content"`
				ReasoningDetails []loopReasoningDetail `json:"reasoning_details"`
				ToolCalls        []struct {
					Index    int `json:"index"`
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	for _, choice := range payload.Choices {
		if text := contentText(choice.Delta.Content); text != "" {
			i.deltas = append(i.deltas, loopDelta{channel: loopChannelContent, text: text})
		}
		if choice.Delta.ReasoningContent != "" {
			i.deltas = append(i.deltas, loopDelta{channel: loopChannelReasoning, text: choice.Delta.ReasoningContent})
		}
		if choice.Delta.Reasoning != "" {
			i.deltas = append(i.deltas, loopDelta{channel: loopChannelReasoning, text: choice.Delta.Reasoning})
		}
		for _, detail := range choice.Delta.ReasoningDetails {
			// An encrypted item keeps its thought to itself, so it is skipped rather
			// than fed to the ring: no verdict can be built from bytes no model wrote.
			if text := detail.text(); text != "" {
				i.deltas = append(i.deltas, loopDelta{channel: loopChannelReasoning, text: text})
			}
		}
		for _, call := range choice.Delta.ToolCalls {
			if call.Function.Arguments == "" {
				continue
			}
			i.deltas = append(i.deltas, loopDelta{
				channel: loopChannelToolArgs, text: call.Function.Arguments,
				toolCall: call.Index, numbered: true,
			})
		}
	}
}

func (i *loopInspector) extractResponses(data []byte) {
	var payload struct {
		Type        string `json:"type"`
		Delta       string `json:"delta"`
		OutputIndex int    `json:"output_index"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Delta == "" {
		return
	}
	switch payload.Type {
	case "response.output_text.delta":
		i.deltas = append(i.deltas, loopDelta{channel: loopChannelContent, text: payload.Delta})
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		i.deltas = append(i.deltas, loopDelta{channel: loopChannelReasoning, text: payload.Delta})
	case "response.function_call_arguments.delta":
		i.deltas = append(i.deltas, loopDelta{
			channel: loopChannelToolArgs, text: payload.Delta,
			toolCall: payload.OutputIndex, numbered: true,
		})
	}
}

func (i *loopInspector) extractMessages(data []byte) {
	var payload struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Type != "content_block_delta" {
		return
	}
	switch payload.Delta.Type {
	case "text_delta":
		if payload.Delta.Text != "" {
			i.deltas = append(i.deltas, loopDelta{channel: loopChannelContent, text: payload.Delta.Text})
		}
	case "thinking_delta":
		if payload.Delta.Thinking != "" {
			i.deltas = append(i.deltas, loopDelta{channel: loopChannelReasoning, text: payload.Delta.Thinking})
		}
	case "input_json_delta":
		if payload.Delta.PartialJSON != "" {
			i.deltas = append(i.deltas, loopDelta{
				channel: loopChannelToolArgs, text: payload.Delta.PartialJSON,
				toolCall: payload.Index, numbered: true,
			})
		}
	}
}

// sseData is the data payload of one SSE frame, with multi-line data joined the
// way the event-stream format defines it.
func sseData(frame []byte) []byte {
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if len(data) > 0 {
			data = append(data, '\n')
		}
		data = append(data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))...)
	}
	return data
}

// cutFrame is the length of the first complete SSE event in data, including its
// terminating blank line, or 0 when data holds only part of one. Line endings may
// be LF or CRLF; a frame split by either is reassembled from the next read.
func cutFrame(data []byte) int {
	bare := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	switch {
	case bare < 0 && crlf < 0:
		return 0
	case crlf < 0 || (bare >= 0 && bare+2 <= crlf+4):
		return bare + 2
	default:
		return crlf + 4
	}
}

// sseFields are the field names an event stream can begin with, plus the comment
// prefix. A body that starts with one of them is a stream; a body that starts with
// a proper prefix of one is not decided yet.
var sseFields = [...][]byte{[]byte("data:"), []byte("event:"), []byte("id:"), []byte("retry:"), []byte(":")}

// sseHead decides from the first bytes of the body whether it is an event stream.
// It answers undecided while what has been read is a proper prefix of a field name,
// so a plain-text error that happens to start with "e" or "d" is not mistaken for a
// stream and held until EOF. The lookahead is bounded by the longest field name, so
// at most six bytes are ever held back to make the decision.
func sseHead(data []byte) (sse, decided bool) {
	head := data[leadingSpace(data):]
	if len(head) == 0 {
		return false, false
	}
	for _, field := range sseFields {
		if bytes.HasPrefix(head, field) {
			return true, true
		}
		if len(head) < len(field) && bytes.HasPrefix(field, head) {
			return false, false
		}
	}
	return false, true
}

// leadingSpace is how many bytes of the body precede its first non-whitespace byte.
func leadingSpace(data []byte) int {
	for i, b := range data {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return i
	}
	return len(data)
}

// contentText reads a chat delta's content, which providers send either as a
// string or as a list of typed parts.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var joined strings.Builder
	for _, part := range parts {
		joined.WriteString(part.Text)
	}
	return joined.String()
}

// periodicSuffix is the length of the longest suffix of text that consists of
// exact repeats of a period-byte unit. Walking backwards one unit at a time costs
// one ring length in the worst case, which is why the ring is bounded.
func periodicSuffix(text []byte, period int) int {
	if period <= 0 || len(text) < 2*period {
		return 0
	}
	end := len(text)
	if !bytes.Equal(text[end-period:end], text[end-2*period:end-period]) {
		return 0
	}
	suffix := 2 * period
	for suffix+period <= end && bytes.Equal(text[end-suffix-period:end-suffix], text[end-period:end]) {
		suffix += period
	}
	return suffix
}

func blank(text []byte) bool {
	return len(bytes.TrimSpace(text)) == 0
}

func (s *loopChannelState) add(text string) {
	s.ring.add([]byte(text))
	s.ring.trim()
	s.prefix.add([]byte(text))
	s.prefix.trim()
	s.since += len(text)
}

func (s *loopChannelState) reset() {
	s.ring.reset()
	s.prefix.reset()
	s.since, s.cooldown = 0, 0
}

// textWindow keeps the last limit bytes appended to it without copying the window
// on every write, and can hand back what it had to drop so a caller that must
// deliver every byte still can.
type textWindow struct {
	buf   []byte
	off   int
	limit int
}

func (w *textWindow) add(p []byte) {
	w.compact()
	w.buf = append(w.buf, p...)
}

// compact returns the live window to the start of the buffer once the dead prefix
// is at least half a window. Without it a window would keep a whole limit of dead
// bytes, which is what made the detector's stated memory bound understate the real
// one; with it a window holds at most 1.5 limits plus the text being appended.
func (w *textWindow) compact() {
	if w.off < w.limit/2 {
		return
	}
	copy(w.buf, w.buf[w.off:])
	w.buf = w.buf[:len(w.buf)-w.off]
	w.off = 0
}

func (w *textWindow) text() []byte {
	return w.buf[w.off:]
}

func (w *textWindow) size() int {
	return len(w.buf) - w.off
}

// trim forgets the bytes that no longer fit.
func (w *textWindow) trim() {
	if over := w.size() - w.limit; over > 0 {
		w.off += over
	}
}

// evict forgets the bytes that no longer fit and returns them, in order.
func (w *textWindow) evict() []byte {
	over := w.size() - w.limit
	if over <= 0 {
		return nil
	}
	dropped := append([]byte(nil), w.buf[w.off:w.off+over]...)
	w.off += over
	return dropped
}

func (w *textWindow) reset() {
	w.buf, w.off = w.buf[:0], 0
}
