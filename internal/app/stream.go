package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// streamTailBytes is how much of a response the stage keeps for the parsers that
// read its end: usage, cost, and provider refusals. Every byte read from
// upstream goes in, released to the client or not, so those parsers see exactly
// what they saw when the copy loop owned this path.
const streamTailBytes = 64 << 10

// streamKeepalive is what a splice sends while it is waiting for the continuation:
// an event-stream comment, which the format requires every parser to ignore and
// which no client mistakes for a frame. Without it a client with an idle-read
// timeout reads the cut as a stalled stream and drops it, and the continuation
// lands in a connection nobody is reading. It is never a data frame: an empty one
// would be a frame a client has to make sense of, and it would carry nothing.
var streamKeepalive = []byte(": \n\n")

// streamKeepaliveInterval is how long a splice waits before telling the client it
// is still there. It stays well inside the idle-read timeouts clients use, which
// are measured in tens of seconds.
const streamKeepaliveInterval = 10 * time.Second

// errStreamWrite reports that the client's own stream failed mid-splice. Such a
// cut needs no terminal sequence: there is no client left to close a response for,
// and the failure is reported to the caller instead of being papered over.
var errStreamWrite = errors.New("loop: client stream write failed")

// loopRecovery is what a recover-mode cut reports to the caller that owns the
// request record. A stage that never cut reports nothing.
type loopRecovery struct {
	// Splices is how many continuation legs were spliced into the client's
	// response: one when the cut was continued, none when the cut is where the
	// answer ended.
	Splices int
	// EstimatedTokens is the continuation leg's own output, estimated from the text
	// that was spliced in. It is what a Messages client is told when the leg
	// reported no usage of its own, because that protocol carries the count in the
	// sequence that ends the message.
	EstimatedTokens int
	// Reason says why the cut was not continued, or why a continuation ended early.
	// It names a failure — a status, a channel, a closed stream — never any of the
	// text involved.
	Reason string
}

// streamSink is where a forwarded response goes: the client, plus the flush that
// puts each frame on the wire. A writer that cannot flush gets a no-op.
type streamSink interface {
	io.Writer
	Flush()
}

// flushSink writes to a client that can flush.
type flushSink struct {
	io.Writer
	flusher http.Flusher
}

func (s flushSink) Flush() { s.flusher.Flush() }

// noFlushSink is a sink for a writer that cannot flush.
type noFlushSink struct{ io.Writer }

func (noFlushSink) Flush() {}

// newStreamSink adapts the client's writer. Flushing is what makes a stream a
// stream, so it is part of the sink rather than something the stage negotiates
// on every write.
func newStreamSink(w http.ResponseWriter) streamSink {
	if flusher, ok := w.(http.Flusher); ok {
		return flushSink{Writer: w, flusher: flusher}
	}
	return noFlushSink{w}
}

// streamStage is the write path of one forwarded response. It copies the
// upstream body to the client, keeps the bounded tail the usage and refusal
// parsers read, and — when the response is an event stream on a generation path
// — runs loop detection over the frames on their way past.
type streamStage struct {
	path string
	cfg  loopConfig
	// body is the upstream response body. A cut closes it, so the provider stops
	// generating a repetition that would only be thrown away.
	body io.ReadCloser
	sink streamSink
	// onFirst runs when the first upstream bytes arrive, which is how the caller
	// times the first token without this stage owning a clock.
	onFirst func()
	// inspector is nil in off mode and for any response detection cannot align
	// frames on; such a stage copies the body through unchanged.
	inspector *loopInspector
	buf       []byte
	// tail holds the last streamTailBytes read from upstream, released or not.
	tail []byte
	// continuation sends the request that continues a cut response. It is nil
	// unless a caller that can issue one built the stage, which is what keeps this
	// stage a copy path for every other caller.
	continuation func(*loopVerdict) (io.ReadCloser, error)
	// keepalive is how long a splice waits for the continuation before sending the
	// client a comment that says the stream is still alive.
	keepalive time.Duration
	// recovery is what the cut reported, nil until the stage cuts a stream.
	recovery *loopRecovery
	// recoveryTail holds the bytes the continuation leg generated, released to the
	// client or not. Its own usage frame is dropped by the reframer, so this is
	// where the leg's cost and tokens are read from.
	recoveryTail []byte
	// blocks follows the /v1/messages content block the client has open. A cut has to
	// close the block the client is actually in, and that is only the first block of
	// the response when the response never opened another one.
	blocks *loopBlockTracker
}

// newStreamStage prepares the write path of one forwarded response. An event
// stream on a generation path is the only body loop detection can align frames
// on, so everything else — an error page, a non-streaming JSON answer, a
// pass-through path — gets a stage that copies it unchanged. Off mode builds no
// inspector at all.
func newStreamStage(path string, cfg loopConfig, resp *http.Response, sink streamSink, onFirst func()) *streamStage {
	s := &streamStage{
		path:      path,
		cfg:       cfg,
		body:      resp.Body,
		sink:      sink,
		onFirst:   onFirst,
		buf:       make([]byte, 32*1024),
		keepalive: streamKeepaliveInterval,
	}
	if !mutablePaths[path] || !isEventStream(resp.Header.Get("Content-Type")) {
		return s
	}
	// The block the client has open is tracked in every mode, because any cut has to
	// close the block it landed in. Only the Messages protocol has content blocks.
	if path == "/v1/messages" {
		s.blocks = &loopBlockTracker{}
	}
	// Only a mode that acts on a loop inspects anything: off builds no inspector
	// at all, and an unrecognized mode leaves the stream alone rather than
	// guessing what the operator meant.
	switch cfg.Mode {
	case loopModeObserve, loopModeAbort, loopModeRecover:
		s.inspector = newLoopInspector(path, loopLimits{
			MinPeriod: cfg.MinPeriod,
			MinRepeat: cfg.MinRepeat,
		})
	}
	return s
}

// isEventStream reports whether upstream answered with an event stream. The
// detector aligns frames on the blank line that terminates an event, so a body
// that does not use one would be buffered whole and never detected against; it
// is left alone instead.
func isEventStream(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
}

// Run copies the upstream body to the client until it ends or a confirmed loop
// cuts it, and returns the verdict it cut on. The caller reads the accumulated
// tail from the stage afterwards.
func (s *streamStage) Run() (*loopVerdict, error) {
	var found *loopVerdict
	for {
		n, readErr := s.body.Read(s.buf)
		if n > 0 {
			verdict, cut, err := s.feed(s.buf[:n])
			if found == nil {
				found = verdict
			}
			if err != nil {
				return found, err
			}
			if cut {
				return found, nil
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return found, readErr
		}
	}
	if s.inspector != nil {
		// EOF is also how a trailing frame that never got its blank line reaches
		// the client, and how observe hands back what it was still withholding, so
		// every stage that inspects ends here.
		trailing, _ := s.inspector.Feed(nil)
		if err := s.emit(trailing); err != nil {
			return found, err
		}
	}
	return found, nil
}

// feed hands one chunk read from upstream to the inspector and writes what may
// go out. It returns a verdict when the chunk completed a loop, and cut when the
// stage ended the stream there.
func (s *streamStage) feed(chunk []byte) (verdict *loopVerdict, cut bool, err error) {
	if s.onFirst != nil {
		s.onFirst()
		s.onFirst = nil
	}
	s.tail = appendTail(s.tail, chunk, streamTailBytes)
	if s.inspector == nil {
		return nil, false, s.emit(chunk)
	}
	release, verdict := s.inspector.Feed(chunk)
	if err := s.emit(release); err != nil {
		return verdict, false, err
	}
	if verdict == nil || s.cfg.Mode == loopModeObserve {
		// Observe changes no bytes: the frames the inspector withheld come back
		// with a later Feed, so the client still receives the whole response.
		return verdict, false, nil
	}
	// A cut stops the stream where the repetition was confirmed. The frames the
	// inspector withheld are dropped, which is what cutting a loop means.
	//
	// The upstream body is closed before the cut is finished, not after: in recover
	// mode finishing it means waiting for and streaming the entire continuation, and
	// a generation left open for that long keeps producing tokens this request has
	// already decided to throw away.
	_ = s.body.Close()
	if err := s.finishCut(verdict); err != nil {
		return verdict, true, err
	}
	return verdict, true, nil
}

// trimDroppedPrefix shortens a clean prefix by the text a cut withheld instead of
// releasing: the frame that straddled the clean-prefix boundary stays behind with the
// repetition, so the client never saw that text and a continuation resuming from it
// would skip content the reader does not have. The cut never splits a rune.
func trimDroppedPrefix(prefix string, dropped int) string {
	if dropped <= 0 {
		return prefix
	}
	if dropped >= len(prefix) {
		return ""
	}
	cut := len(prefix) - dropped
	for cut > 0 && prefix[cut]&0xC0 == 0x80 {
		cut--
	}
	return prefix[:cut]
}

// emit writes released bytes to the client and flushes them. A chunk that
// releases nothing is neither written nor flushed, so a withheld frame adds no
// empty write to the wire.
//
// It is also where the /v1/messages block tracker is fed: what emit writes is exactly
// what the client has, so a block start the cut withheld never counts as open, and the
// block the client is left in is the one the terminal closes.
func (s *streamStage) emit(release []byte) error {
	if len(release) == 0 {
		return nil
	}
	if s.blocks != nil {
		s.blocks.scan(release)
	}
	if _, err := s.sink.Write(release); err != nil {
		return err
	}
	s.sink.Flush()
	return nil
}

// openBlock is the /v1/messages content block the client has open, loopNoBlock when the
// stream already closed it. The chat and Responses protocols have no blocks and ignore
// it.
func (s *streamStage) openBlock() int {
	if s.blocks == nil {
		return 0
	}
	return s.blocks.block()
}

// cutTerminal is the closing sequence of a response that ends at the cut: nothing was
// spliced, so it reports no usage and no tool call, and it closes the block the client
// is in.
func (s *streamStage) cutTerminal() []byte {
	return loopTerminalFor(loopTerminalShape{Path: s.path, Block: s.openBlock()})
}

// finishCut ends a stream that was cut at a confirmed loop. In recover mode it is
// the seam the continuation splices into: the aborted leg's terminal frames are
// never written, the continuation's text takes their place, and the one terminal
// sequence after it closes the client's response. Every other case — abort mode, a
// verdict a continuation cannot resume from, a continuation that failed — writes
// the plain terminal sequence instead, so the client is never left without one.
func (s *streamStage) finishCut(verdict *loopVerdict) error {
	// A frame the release could not hand over straddles the boundary, so the client
	// is missing that much of the clean prefix: the continuation must resume from
	// what the client actually has, not from text it never received.
	verdict.Prefix = trimDroppedPrefix(verdict.Prefix, verdict.DroppedText)
	// Recover mode continues an answer only when it has a request sender, an
	// allowance to spend, and a verdict a continuation can resume from.
	if s.cfg.Mode != loopModeRecover || s.continuation == nil || s.cfg.MaxContinuations < 1 {
		return s.emit(s.cutTerminal())
	}
	if err := loopContinuable(verdict); err != nil {
		// The verdict names text no model can be asked to continue — a reasoning
		// channel, a mid-JSON tool-argument fragment, or nothing at all — so the cut
		// is where this answer ends.
		s.recovery = &loopRecovery{Reason: err.Error()}
		return s.emit(s.cutTerminal())
	}
	s.recovery = &loopRecovery{}
	return s.splice(verdict)
}

// streamChunk is one read of a continuation body, or the error that ended it.
type streamChunk struct {
	data []byte
	err  error
}

// splice continues a cut response inside the stream the client is already reading.
// The continuation is requested while the client is kept alive, inspected for a
// repetition of its own, reframed into the tail of the first response, and closed
// once by the same terminal sequence the abort path writes. A continuation that
// cannot be spliced ends the response at the cut, so the client always receives
// exactly one terminator.
func (s *streamStage) splice(verdict *loopVerdict) error {
	body, err := s.awaitContinuation(verdict)
	if err != nil {
		if errors.Is(err, errStreamWrite) {
			// The client is gone; there is no response left to close for it.
			return err
		}
		return s.endSplice(nil, err)
	}
	defer body.Close()

	// The continuation's own content blocks are remapped onto the client's, so the
	// block the cut landed in is the one its first text block continues.
	reframer := newContinuationReframerFor(s.path, s.openBlock())
	// The continuation is a second response, so it can repeat itself too. It is
	// inspected exactly like the first leg, and a repetition in it ends the answer:
	// asking the model to resume the text it just failed to escape would only spend
	// another request to arrive back here.
	inspector := newLoopInspector(s.path, loopLimits{MinPeriod: s.cfg.MinPeriod, MinRepeat: s.cfg.MinRepeat})
	chunks := make(chan streamChunk, 8)
	done := make(chan struct{})
	defer close(done)
	go pumpContinuation(body, chunks, done)

	ticker := time.NewTicker(s.keepaliveInterval())
	defer ticker.Stop()
	for {
		select {
		case chunk := <-chunks:
			if len(chunk.data) > 0 {
				s.recoveryTail = appendTail(s.recoveryTail, chunk.data, streamTailBytes)
				release, again := inspector.Feed(chunk.data)
				if err := s.emit(reframer.Feed(release)); err != nil {
					return err
				}
				if again != nil {
					return s.endSplice(reframer, errors.New("continuation repeated itself"))
				}
			}
			if chunk.err != nil {
				// A leg that ended early still ends the answer cleanly: what it
				// managed to say is spliced in and the response is closed. Its own
				// inspector may still be withholding frames — a suspicion that never
				// confirmed is released now — so they are reframed and spliced in
				// rather than dropped with the leg, whichever way the leg ended.
				trailing, _ := inspector.Feed(nil)
				if err := s.emit(reframer.Feed(trailing)); err != nil {
					return err
				}
				if errors.Is(chunk.err, io.EOF) {
					return s.endSplice(reframer, nil)
				}
				return s.endSplice(reframer, errors.New("continuation stream ended early"))
			}
		case <-ticker.C:
			// A provider that thinks for longer than a client's idle timeout must not
			// cost the client the response it is still owed.
			if err := s.emit(streamKeepalive); err != nil {
				return err
			}
		}
	}
}

// endSplice closes a spliced response, or a cut one whose continuation never got
// far enough to be spliced in. reframer is nil when the continuation failed before
// it produced a byte, and carries the estimate and the text that was spliced
// otherwise. cause is the failure that ended the continuation early, nil when it
// simply finished. Either way the client receives the one terminal sequence its
// protocol needs, from the same implementation the abort path uses.
func (s *streamStage) endSplice(reframer *continuationReframer, cause error) error {
	if s.recovery == nil {
		s.recovery = &loopRecovery{}
	}
	if cause != nil {
		s.recovery.Reason = cause.Error()
	}
	if reframer == nil {
		return s.emit(s.cutTerminal())
	}
	s.recovery.EstimatedTokens = reframer.EstimatedOutputTokens()
	if reframer.Spliced() {
		s.recovery.Splices = 1
	}
	return s.emit(reframer.Finish())
}

// awaitContinuation sends the continuation request for a cut response and returns
// its body. The request runs in its own goroutine because the wait for its first
// token is the one gap a splice cannot fill with generated text, and the client has
// to be told the stream is alive across it.
func (s *streamStage) awaitContinuation(verdict *loopVerdict) (io.ReadCloser, error) {
	type answer struct {
		body io.ReadCloser
		err  error
	}
	answers := make(chan answer, 1)
	go func() {
		body, err := s.continuation(verdict)
		answers <- answer{body: body, err: err}
	}()
	ticker := time.NewTicker(s.keepaliveInterval())
	defer ticker.Stop()
	for {
		select {
		case answer := <-answers:
			return answer.body, answer.err
		case <-ticker.C:
			if err := s.emit(streamKeepalive); err != nil {
				// Nothing will receive the continuation now, so its body is closed as
				// soon as the provider answers instead of being read into a dead stream.
				go func() {
					if answer := <-answers; answer.body != nil {
						_ = answer.body.Close()
					}
				}()
				return nil, fmt.Errorf("%w: %v", errStreamWrite, err)
			}
		}
	}
}

// keepaliveInterval is how long the splice waits before reassuring the client. A
// stage built without one still gets the shipped interval rather than a panic.
func (s *streamStage) keepaliveInterval() time.Duration {
	if s.keepalive > 0 {
		return s.keepalive
	}
	return streamKeepaliveInterval
}

// pumpContinuation reads a continuation body in its own goroutine, so the splice
// can keep the client alive between deltas as well as before the first one. It
// copies each read because the buffer is reused, and it always ends with a chunk
// carrying the error that stopped it.
func pumpContinuation(body io.Reader, chunks chan<- streamChunk, done <-chan struct{}) {
	buf := make([]byte, 32*1024)
	send := func(chunk streamChunk) bool {
		select {
		case chunks <- chunk:
			return true
		case <-done:
			return false
		}
	}
	for {
		n, err := body.Read(buf)
		if n > 0 && !send(streamChunk{data: append([]byte(nil), buf[:n]...)}) {
			return
		}
		if err != nil {
			send(streamChunk{err: err})
			return
		}
	}
}
