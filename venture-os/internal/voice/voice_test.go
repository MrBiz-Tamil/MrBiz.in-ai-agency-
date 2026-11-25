package voice_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/voice"
)

// ───────────────────────────── fakes ─────────────────────────────

type fakeSpeech struct{ ch chan voice.Transcript }

func newSpeech() *fakeSpeech { return &fakeSpeech{ch: make(chan voice.Transcript, 16)} }
func (f *fakeSpeech) Recognize(context.Context, <-chan []byte) (<-chan voice.Transcript, error) {
	return f.ch, nil
}
func (f *fakeSpeech) say(text string)  { f.ch <- voice.Transcript{Text: text, Final: true} }
func (f *fakeSpeech) partial(t string) { f.ch <- voice.Transcript{Text: t} }
func (f *fakeSpeech) hangUp()          { close(f.ch) }

type fakeSynth struct {
	mu         sync.Mutex
	firstDelay time.Duration
	chunks     int
	gap        time.Duration
	spoken     []string
	empty      bool
}

func (f *fakeSynth) Speak(ctx context.Context, text string) (<-chan []byte, error) {
	f.mu.Lock()
	f.spoken = append(f.spoken, text)
	first, n, gap, empty := f.firstDelay, f.chunks, f.gap, f.empty
	f.mu.Unlock()
	out := make(chan []byte)
	go func() {
		defer close(out)
		if empty {
			return
		}
		for i := 0; i < n; i++ {
			d := gap
			if i == 0 {
				d = first
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
			select {
			case <-ctx.Done():
				return
			case out <- []byte(fmt.Sprintf("%s#%d", text, i)):
			}
		}
	}()
	return out, nil
}
func (f *fakeSynth) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.spoken...)
}

type fakeLine struct {
	mu      sync.Mutex
	audio   chan []byte
	played  []string
	stops   int
	hangups []string
}

func newLine() *fakeLine                 { return &fakeLine{audio: make(chan []byte)} }
func (l *fakeLine) Audio() <-chan []byte { return l.audio }
func (l *fakeLine) Play(_ context.Context, c []byte) error {
	l.mu.Lock()
	l.played = append(l.played, string(c))
	l.mu.Unlock()
	return nil
}
func (l *fakeLine) Stop() { l.mu.Lock(); l.stops++; l.mu.Unlock() }
func (l *fakeLine) Hangup(reason string) {
	l.mu.Lock()
	l.hangups = append(l.hangups, reason)
	l.mu.Unlock()
}
func (l *fakeLine) snapshot() (played []string, stops int, hangups []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.played...), l.stops, append([]string(nil), l.hangups...)
}

func reply(text string) voice.Responder {
	return voice.ResponderFunc(func(context.Context, voice.CallInfo, string) (voice.Reply, error) {
		return voice.Reply{Text: text}, nil
	})
}

type call struct {
	sp *fakeSpeech
	sy *fakeSynth
	ln *fakeLine
	s  *voice.Session
}

func newCall(r voice.Responder, cfg voice.Config) call {
	c := call{sp: newSpeech(), sy: &fakeSynth{chunks: 3, gap: time.Millisecond}, ln: newLine()}
	c.s = &voice.Session{Info: voice.CallInfo{Tenant: "t1", CallID: "c1", From: "+910000000000"}, Line: c.ln, Speech: c.sp, Synth: c.sy, Responder: r, Config: cfg}
	return c
}

func (c call) run(t *testing.T) voice.Summary {
	t.Helper()
	done := make(chan voice.Summary, 1)
	go func() {
		sum, err := c.s.Run(context.Background())
		if err != nil {
			t.Error(err)
		}
		done <- sum
	}()
	select {
	case s := <-done:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not end")
		return voice.Summary{}
	}
}

// ───────────────────────────── tests ─────────────────────────────

func TestATurnIsHeardAnsweredAndTimed(t *testing.T) {
	c := newCall(reply("Your shipment leaves Friday."), voice.Config{})
	c.sy.firstDelay = 30 * time.Millisecond
	c.sp.say("when does my shipment leave")
	go func() { time.Sleep(200 * time.Millisecond); c.sp.hangUp() }()
	sum := c.run(t)
	played, _, hangups := c.ln.snapshot()
	if len(played) != 3 || played[0] != "Your shipment leaves Friday.#0" || played[2] != "Your shipment leaves Friday.#2" {
		t.Fatalf("played = %v", played)
	}
	// The caller ended the call, so there is no line left for us to drop.
	if len(sum.Turns) != 1 || sum.Turns[0].Turn != 1 || sum.EndReason != "caller hung up" || len(hangups) != 0 {
		t.Fatalf("summary = %+v hangups=%v", sum, hangups)
	}
	if l := sum.Turns[0].Latency; l < 30*time.Millisecond || l > 400*time.Millisecond || sum.Turns[0].OverBudget {
		t.Fatalf("latency %v should be about the 30ms synthesis delay and within the 800ms budget", l)
	}
	if sum.P50 == 0 || sum.P95 < sum.P50 {
		t.Fatalf("percentiles = %v %v", sum.P50, sum.P95)
	}
}

func TestSlowFirstAudioIsCountedAgainstTheBudget(t *testing.T) {
	c := newCall(reply("ok"), voice.Config{Budget: 20 * time.Millisecond})
	c.sy.firstDelay = 80 * time.Millisecond
	c.sp.say("hello")
	go func() { time.Sleep(250 * time.Millisecond); c.sp.hangUp() }()
	sum := c.run(t)
	if !sum.Turns[0].OverBudget || sum.OverBudget != 1 {
		t.Fatalf("a turn slower than the budget must be flagged: %+v", sum)
	}
}

func TestTheCallerCanInterruptAndPlaybackStopsAtOnce(t *testing.T) {
	c := newCall(reply("This is a long answer with many words."), voice.Config{})
	c.sy.chunks, c.sy.gap = 40, 15*time.Millisecond // about 600 ms of speech
	c.sp.say("tell me everything")
	go func() {
		time.Sleep(120 * time.Millisecond)
		c.sp.partial("wait") // the caller talks over the agent
		time.Sleep(150 * time.Millisecond)
		c.sp.hangUp()
	}()
	sum := c.run(t)
	played, stops, _ := c.ln.snapshot()
	if !sum.Turns[0].Interrupted || sum.Interrupts != 1 || stops != 1 {
		t.Fatalf("barge-in was not detected or did not flush the line: %+v stops=%d", sum, stops)
	}
	if len(played) >= 25 || len(played) == 0 {
		t.Fatalf("%d of 40 chunks were played; speech must stop soon after the interruption", len(played))
	}
	before := len(played)
	time.Sleep(60 * time.Millisecond)
	if after, _, _ := c.ln.snapshot(); len(after) != before {
		t.Fatal("audio kept flowing after the interruption")
	}
}

func TestANewUtteranceReplacesWhateverIsStillBeingSaid(t *testing.T) {
	var n int
	var mu sync.Mutex
	r := voice.ResponderFunc(func(_ context.Context, _ voice.CallInfo, u string) (voice.Reply, error) {
		mu.Lock()
		n++
		mu.Unlock()
		return voice.Reply{Text: "answer to " + u}, nil
	})
	c := newCall(r, voice.Config{})
	c.sy.chunks, c.sy.gap = 30, 10*time.Millisecond
	c.sp.say("first")
	go func() {
		time.Sleep(80 * time.Millisecond)
		c.sp.say("second")
		time.Sleep(700 * time.Millisecond) // long enough for all 30 chunks (about 300 ms) to play
		c.sp.hangUp()
	}()
	sum := c.run(t)
	if len(sum.Turns) != 2 {
		t.Fatalf("turns = %+v", sum.Turns)
	}
	played, _, _ := c.ln.snapshot()
	var first, second int
	for _, p := range played {
		if strings.HasPrefix(p, "answer to first") {
			first++
		}
		if strings.HasPrefix(p, "answer to second") {
			second++
		}
	}
	if first >= 30 || second != 30 {
		t.Fatalf("the first answer must be cut off (%d of 30 played) and the second played in full (%d)", first, second)
	}
}

func TestOnlyFinalTranscriptsReachTheResponder(t *testing.T) {
	var got []string
	var mu sync.Mutex
	r := voice.ResponderFunc(func(_ context.Context, _ voice.CallInfo, u string) (voice.Reply, error) {
		mu.Lock()
		got = append(got, u)
		mu.Unlock()
		return voice.Reply{Text: "ok"}, nil
	})
	c := newCall(r, voice.Config{})
	c.sp.partial("wh")
	c.sp.partial("when does")
	c.sp.say("   ") // silence
	c.sp.say("when does it ship")
	c.sp.say(strings.Repeat("x", 5000))
	go func() { time.Sleep(200 * time.Millisecond); c.sp.hangUp() }()
	c.run(t)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "when does it ship" || len([]rune(got[1])) != 1000 {
		t.Fatalf("the responder saw %d utterances (%q ...): only final, non-blank, bounded text may reach it", len(got), got[0])
	}
}

func TestASlowResponderGetsAFillerThenItsRealAnswer(t *testing.T) {
	slow := voice.ResponderFunc(func(ctx context.Context, _ voice.CallInfo, _ string) (voice.Reply, error) {
		time.Sleep(150 * time.Millisecond)
		return voice.Reply{Text: "Here is the answer."}, nil
	})
	c := newCall(slow, voice.Config{Filler: "One moment please.", FillerAfter: 30 * time.Millisecond})
	c.sp.say("what is the status")
	go func() { time.Sleep(400 * time.Millisecond); c.sp.hangUp() }()
	sum := c.run(t)
	spoken := c.sy.texts()
	if len(spoken) != 2 || spoken[0] != "One moment please." || spoken[1] != "Here is the answer." {
		t.Fatalf("spoken = %v", spoken)
	}
	if !sum.Turns[0].Filler || sum.Turns[0].Latency > 100*time.Millisecond {
		t.Fatalf("the filler is the first audio the caller hears, so latency is measured to it: %+v", sum.Turns[0])
	}
}

func TestAFailingOrTimedOutResponderBecomesAnApologyAndAHandoff(t *testing.T) {
	for name, r := range map[string]voice.Responder{
		"error": voice.ResponderFunc(func(context.Context, voice.CallInfo, string) (voice.Reply, error) {
			return voice.Reply{}, errors.New("crm down")
		}),
		"timeout": voice.ResponderFunc(func(ctx context.Context, _ voice.CallInfo, _ string) (voice.Reply, error) {
			<-ctx.Done()
			return voice.Reply{}, ctx.Err()
		}),
		"nothing to say": voice.ResponderFunc(func(context.Context, voice.CallInfo, string) (voice.Reply, error) { return voice.Reply{}, nil }),
	} {
		c := newCall(r, voice.Config{ResponderTimeout: 60 * time.Millisecond, Apology: "Sorry, a person will call you back."})
		c.sp.say("hello")
		sum := c.run(t)
		_, _, hangups := c.ln.snapshot()
		if sum.EndReason != "handoff" || len(hangups) != 1 || hangups[0] != "handoff" || sum.Turns[0].Failed == "" {
			t.Errorf("%s: %+v hangups=%v", name, sum, hangups)
		}
		if sp := c.sy.texts(); len(sp) == 0 || sp[len(sp)-1] != "Sorry, a person will call you back." {
			t.Errorf("%s: the caller must hear an apology, not dead air: %v", name, sp)
		}
	}
}

func TestAHangupReplyIsSpokenInFullBeforeTheCallEnds(t *testing.T) {
	c := newCall(voice.ResponderFunc(func(context.Context, voice.CallInfo, string) (voice.Reply, error) {
		return voice.Reply{Text: "Goodbye and thank you.", Hangup: true}, nil
	}), voice.Config{})
	c.sy.chunks, c.sy.gap = 5, 10*time.Millisecond
	c.sp.say("that is all")
	sum := c.run(t)
	played, _, hangups := c.ln.snapshot()
	if len(played) != 5 || sum.EndReason != "completed" || len(hangups) != 1 || hangups[0] != "completed" {
		t.Fatalf("the caller must hear the whole goodbye before the line drops: played=%d %+v %v", len(played), sum, hangups)
	}
}

func TestLimitsEndAStuckCall(t *testing.T) {
	c := newCall(reply("ok"), voice.Config{MaxTurns: 3})
	for i := 0; i < 6; i++ {
		c.sp.say(fmt.Sprintf("question %d", i))
	}
	sum := c.run(t)
	_, _, hangups := c.ln.snapshot()
	if len(sum.Turns) != 3 || sum.EndReason != "turn limit" || len(hangups) != 1 || hangups[0] != "turn limit" {
		t.Fatalf("turn limit: %+v %v", sum, hangups)
	}

	d := newCall(reply("ok"), voice.Config{MaxDuration: 80 * time.Millisecond})
	sum = d.run(t) // nobody speaks and nobody hangs up
	_, _, hangups = d.ln.snapshot()
	if sum.EndReason != "call length limit" || len(hangups) != 1 {
		t.Fatalf("duration limit: %+v %v", sum, hangups)
	}
}

func TestASynthesiserThatProducesNothingIsRecordedNotSilentlyIgnored(t *testing.T) {
	c := newCall(reply("hello"), voice.Config{})
	c.sy.empty = true
	c.sp.say("hi")
	go func() { time.Sleep(100 * time.Millisecond); c.sp.hangUp() }()
	sum := c.run(t)
	if !strings.Contains(sum.Turns[0].Failed, "no audio") {
		t.Fatalf("a turn with no audio must say so: %+v", sum.Turns[0])
	}
}

func TestResponderSeesTheCallContextAndTheTurnNumber(t *testing.T) {
	var seen []voice.CallInfo
	var mu sync.Mutex
	r := voice.ResponderFunc(func(_ context.Context, c voice.CallInfo, _ string) (voice.Reply, error) {
		mu.Lock()
		seen = append(seen, c)
		mu.Unlock()
		return voice.Reply{Text: "ok"}, nil
	})
	c := newCall(r, voice.Config{})
	c.s.Info.Verified = false
	c.sp.say("one")
	c.sp.say("two")
	go func() { time.Sleep(150 * time.Millisecond); c.sp.hangUp() }()
	c.run(t)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0].Turn != 1 || seen[1].Turn != 2 || seen[0].Tenant != "t1" || seen[0].Verified {
		t.Fatalf("seen = %+v: a caller is unverified unless the telephony layer proved otherwise", seen)
	}
}

func TestACallLeavesNoGoroutinesBehind(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		c := newCall(reply("a fairly long answer"), voice.Config{})
		c.sy.chunks, c.sy.gap = 20, 5*time.Millisecond
		c.sp.say("hello")
		go func() {
			time.Sleep(30 * time.Millisecond)
			c.sp.partial("stop")
			time.Sleep(30 * time.Millisecond)
			c.sp.hangUp()
		}()
		c.run(t)
	}
	time.Sleep(150 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines grew from %d to %d: calls are leaking", before, after)
	}
}
