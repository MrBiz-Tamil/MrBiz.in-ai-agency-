// Package voice is the call orchestration core: it runs one phone call as a loop of listen, think, speak, with the
// turn-taking behaviour a natural conversation needs and the measurements that tell you whether it is fast enough.
//
// WHAT THIS IS, AND IS NOT. It is the part that is the same whichever speech and telephony providers are used:
//
//   - barge-in: when the caller starts speaking over the agent, playback stops immediately;
//   - latency: for every turn it measures the time from the caller finishing to the first audio byte going out, and
//     counts the turns that missed the budget (default 800 ms), so "is it fast enough" is a number, not a feeling;
//   - filler: if the responder is slow, a short "one moment" is spoken instead of silence;
//   - failure: a responder error or timeout becomes an apology and a hand-off to a person, never dead air or a guess;
//   - limits: a cap on turns and on call length, so a stuck or abusive call ends.
//
// It is NOT speech recognition, speech synthesis or telephony: those are the interfaces below (Speech, Synth, Line),
// and a real deployment needs an adapter for each provider chosen (for example a streaming recognizer, a streaming
// voice, and a telephony or WebRTC leg), with that provider's account. No adapter exists yet, and nothing here has
// been run against a real phone call. The latency figures in tests are for the orchestration only; the real figure
// depends on the providers' own latency, which has to be measured.
//
// SECURITY. A caller's words are untrusted input and a phone number is not proof of identity (caller ID can be
// spoofed). CallInfo.Verified says whether the telephony layer actually verified the caller; a Responder must not
// disclose one customer's information to an unverified caller.
package voice

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Transcript is a piece of recognised speech. Partial transcripts arrive while the caller is still speaking; the final
// one marks the end of an utterance.
type Transcript struct {
	Text  string
	Final bool
}

// Speech recognises caller audio as it arrives.
type Speech interface {
	// Recognize consumes audio until it closes or ctx ends, emitting transcripts, and closes the returned channel when
	// it is done.
	Recognize(ctx context.Context, audio <-chan []byte) (<-chan Transcript, error)
}

// Synth turns text into audio as a stream, so playback can begin before the whole sentence is synthesised.
type Synth interface {
	// Speak returns audio chunks; it stops and closes the channel when ctx is cancelled. The first chunk should arrive
	// quickly: it is what the latency budget measures.
	Speak(ctx context.Context, text string) (<-chan []byte, error)
}

// Line is one call leg.
type Line interface {
	// Audio is the caller's audio; it closes when the caller hangs up.
	Audio() <-chan []byte
	// Play sends audio to the caller.
	Play(ctx context.Context, chunk []byte) error
	// Stop discards any audio queued for the caller (barge-in).
	Stop()
	// Hangup ends the call.
	Hangup(reason string)
}

// CallInfo says who is calling and what is known about them.
type CallInfo struct {
	Tenant string
	CallID string
	From   string // the caller's number as presented; spoofable
	// Verified is true only if the telephony layer proved the caller's identity (not merely that a number was shown).
	Verified bool
	Turn     int
}

// Reply is what to say next.
type Reply struct {
	Text string
	// Hangup ends the call after the reply has been spoken.
	Hangup bool
	// Handoff says the call needs a person; the reason is recorded and the call ends after the reply.
	Handoff bool
}

// Responder decides what to say. It receives only FINAL transcripts and must treat the text as data, not instructions.
type Responder interface {
	Respond(ctx context.Context, c CallInfo, utterance string) (Reply, error)
}

// ResponderFunc adapts a function to Responder.
type ResponderFunc func(ctx context.Context, c CallInfo, utterance string) (Reply, error)

func (f ResponderFunc) Respond(ctx context.Context, c CallInfo, u string) (Reply, error) {
	return f(ctx, c, u)
}

// Config tunes a session. Zero values pick the defaults.
type Config struct {
	Budget           time.Duration // latency target, final transcript to first audio byte; default 800 ms
	Filler           string        // spoken when the responder is slow; empty disables
	FillerAfter      time.Duration // default 600 ms
	ResponderTimeout time.Duration // default 5 s; on timeout the call is handed off
	MaxTurns         int           // default 40
	MaxDuration      time.Duration // default 15 minutes
	Apology          string        // spoken on a responder failure
	Now              func() time.Time
}

// TurnStats describes one turn.
type TurnStats struct {
	Turn        int
	Utterance   string // truncated; for diagnosis, not for logs that must not hold customer speech
	Latency     time.Duration
	OverBudget  bool
	Filler      bool
	Interrupted bool // the caller spoke over the reply
	Failed      string
}

// Summary is the outcome of a call.
type Summary struct {
	Turns      []TurnStats
	P50, P95   time.Duration
	OverBudget int
	Interrupts int
	EndReason  string
}

// Session runs one call.
type Session struct {
	Info      CallInfo
	Line      Line
	Speech    Speech
	Synth     Synth
	Responder Responder
	Config    Config
}

const maxUtterance = 1000

var errNoAudio = errors.New("voice: the synthesiser produced no audio")

func (c *Config) defaults() {
	if c.Budget == 0 {
		c.Budget = 800 * time.Millisecond
	}
	if c.FillerAfter == 0 {
		c.FillerAfter = 600 * time.Millisecond
	}
	if c.ResponderTimeout == 0 {
		c.ResponderTimeout = 5 * time.Second
	}
	if c.MaxTurns == 0 {
		c.MaxTurns = 40
	}
	if c.MaxDuration == 0 {
		c.MaxDuration = 15 * time.Minute
	}
	if c.Apology == "" {
		c.Apology = "I'm sorry, I can't help with that right now. I'll get a person to call you back."
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

type playback struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Run drives the call until the caller hangs up, the reply asks to hang up, a limit is reached or ctx ends.
func (s *Session) Run(ctx context.Context) (Summary, error) {
	cfg := s.Config
	cfg.defaults()
	ctx, cancel := context.WithTimeout(ctx, cfg.MaxDuration)
	defer cancel()

	transcripts, err := s.Speech.Recognize(ctx, s.Line.Audio())
	if err != nil {
		return Summary{}, fmt.Errorf("voice: start recognition: %w", err)
	}

	var (
		sum     Summary
		mu      sync.Mutex // guards sum.Turns and the current playback
		current *playback
		turn    int
		hangup  string
	)
	stopPlayback := func() (wasPlaying bool) {
		mu.Lock()
		p := current
		mu.Unlock()
		if p == nil {
			return false
		}
		select {
		case <-p.done:
			return false
		default:
		}
		p.cancel()
		<-p.done
		s.Line.Stop()
		return true
	}
	record := func(i int, f func(*TurnStats)) {
		mu.Lock()
		f(&sum.Turns[i])
		mu.Unlock()
	}
	finish := func(reason string) (Summary, error) {
		stopPlayback()
		sum.EndReason = reason
		sum.P50, sum.P95 = percentiles(sum.Turns)
		for _, t := range sum.Turns {
			if t.OverBudget {
				sum.OverBudget++
			}
			if t.Interrupted {
				sum.Interrupts++
			}
		}
		if hangup != "" {
			s.Line.Hangup(hangup)
		}
		return sum, nil
	}

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				hangup = "call length limit"
				return finish("call length limit")
			}
			return finish("cancelled")
		case tr, ok := <-transcripts:
			if !ok {
				return finish("caller hung up")
			}
			text := strings.TrimSpace(tr.Text)
			if text == "" {
				continue
			}
			if !tr.Final {
				// The caller is speaking: if we are talking, stop talking at once.
				if stopPlayback() {
					mu.Lock()
					if n := len(sum.Turns); n > 0 {
						sum.Turns[n-1].Interrupted = true
					}
					mu.Unlock()
				}
				continue
			}
			stopPlayback() // a new utterance replaces anything still being said
			if turn >= cfg.MaxTurns {
				hangup = "turn limit"
				return finish("turn limit")
			}
			turn++
			if utf8.RuneCountInString(text) > maxUtterance {
				text = string([]rune(text)[:maxUtterance])
			}
			mu.Lock()
			sum.Turns = append(sum.Turns, TurnStats{Turn: turn, Utterance: truncate(text, 80)})
			idx := len(sum.Turns) - 1
			mu.Unlock()
			t0 := cfg.Now() // the caller has just finished: the clock starts here

			reply, rerr := s.think(ctx, cfg, turn, text, t0, idx, record)
			if rerr != nil {
				record(idx, func(t *TurnStats) { t.Failed = rerr.Error() })
				reply = Reply{Text: cfg.Apology, Handoff: true, Hangup: true}
			}
			if reply.Handoff || reply.Hangup {
				reason := "completed"
				if reply.Handoff {
					reason = "handoff"
				}
				// Speak the last words to completion before ending: do not cut the caller off mid-sentence.
				s.speak(ctx, cfg, reply.Text, t0, idx, record, true)
				hangup = reason
				return finish(reason)
			}
			p := &playback{done: make(chan struct{})}
			pctx, pcancel := context.WithCancel(ctx)
			p.cancel = pcancel
			mu.Lock()
			current = p
			mu.Unlock()
			go func(text string) {
				defer close(p.done)
				s.speak(pctx, cfg, text, t0, idx, record, false)
			}(reply.Text)
		}
	}
}

// think asks the responder, speaking a filler if it is slow. The filler counts as the first audio of the turn.
func (s *Session) think(ctx context.Context, cfg Config, turn int, text string, t0 time.Time, idx int, record func(int, func(*TurnStats))) (Reply, error) {
	rctx, cancel := context.WithTimeout(ctx, cfg.ResponderTimeout)
	defer cancel()
	type out struct {
		r   Reply
		err error
	}
	ch := make(chan out, 1)
	go func() {
		info := s.Info
		info.Turn = turn
		r, err := s.Responder.Respond(rctx, info, text)
		ch <- out{r, err}
	}()
	var filler <-chan time.Time
	if cfg.Filler != "" {
		t := time.NewTimer(cfg.FillerAfter)
		defer t.Stop()
		filler = t.C
	}
	for {
		select {
		case o := <-ch:
			if o.err == nil && strings.TrimSpace(o.r.Text) == "" && !o.r.Hangup {
				return Reply{}, errors.New("the responder returned nothing to say")
			}
			return o.r, o.err
		case <-filler:
			filler = nil
			record(idx, func(t *TurnStats) { t.Filler = true })
			s.speak(ctx, cfg, cfg.Filler, t0, idx, record, true)
		case <-rctx.Done():
			return Reply{}, fmt.Errorf("the responder did not answer within %s", cfg.ResponderTimeout)
		}
	}
}

// speak synthesises text and plays it, recording the latency of the turn's first audio byte. With wait set it
// blocks until playback finishes.
func (s *Session) speak(ctx context.Context, cfg Config, text string, t0 time.Time, idx int, record func(int, func(*TurnStats)), wait bool) {
	chunks, err := s.Synth.Speak(ctx, text)
	if err != nil {
		record(idx, func(t *TurnStats) { t.Failed = "speech synthesis: " + err.Error() })
		return
	}
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-chunks:
			if !ok {
				if first {
					record(idx, func(t *TurnStats) { t.Failed = errNoAudio.Error() })
				}
				return
			}
			if first {
				first = false
				lat := cfg.Now().Sub(t0)
				record(idx, func(t *TurnStats) {
					if t.Latency == 0 { // the filler, if any, was the first audio the caller heard
						t.Latency, t.OverBudget = lat, lat > cfg.Budget
					}
				})
			}
			if err := s.Line.Play(ctx, c); err != nil {
				return
			}
		}
	}
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func percentiles(turns []TurnStats) (p50, p95 time.Duration) {
	var ds []time.Duration
	for _, t := range turns {
		if t.Latency > 0 {
			ds = append(ds, t.Latency)
		}
	}
	if len(ds) == 0 {
		return 0, 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	at := func(p float64) time.Duration {
		i := int(p*float64(len(ds))+0.5) - 1 // nearest rank
		if i < 0 {
			i = 0
		}
		if i >= len(ds) {
			i = len(ds) - 1
		}
		return ds[i]
	}
	return at(0.50), at(0.95)
}
