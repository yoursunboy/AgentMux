package claude

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// The stream reader: newline-delimited JSON read one line at a time, and what
// happens to a line that cannot be read.

func TestAMultiLineStreamIsReadOneMessageAtATime(t *testing.T) {
	// The shape `--output-format stream-json --include-hook-events` produces.
	// Only the control messages have fields in streamMessage; the assistant
	// message in the middle is decoded and discarded.
	h := newHarness(t)
	ch := h.subscribe()

	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-multi","cwd":"/tmp/x"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"the response"}]}}`,
		`{"type":"system","subtype":"hook_started","hook_name":"Stop:1"}`,
		`{"type":"system","subtype":"hook_response","hook_name":"Stop:1","output":"ok"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-multi"}`,
	}, "\n")

	if err := h.adapter.ConsumeStream(context.Background(), strings.NewReader(stream)); err != nil {
		t.Fatalf("ConsumeStream: %v", err)
	}

	// One row: the result envelope. The hook lifecycle messages are published
	// and not recorded, and the assistant message is not an observation at all.
	if got := h.recorder.types(); len(got) != 1 || got[0] != TypeAgentCompleted {
		t.Fatalf("recorded %v; want exactly [%s]", got, TypeAgentCompleted)
	}

	kinds := map[Kind]int{}
	for _, ev := range drain(ch) {
		kinds[ev.Kind]++
	}
	want := map[Kind]int{KindInitialized: 1, KindHookStarted: 1, KindHookResponse: 1, KindResult: 1}
	if len(kinds) != len(want) {
		t.Fatalf("the subscriber saw %v; want %v", kinds, want)
	}
	for kind, count := range want {
		if kinds[kind] != count {
			t.Errorf("the subscriber saw %d %s observations; want %d", kinds[kind], kind, count)
		}
	}
}

func TestEveryLineIsReadWhateverTheStreamDoesAroundIt(t *testing.T) {
	// Blank lines, a malformed line, a line with no trailing newline. None of
	// them may cost the result envelope that follows, because that envelope is
	// the only thing that reports how the turn ended.
	h := newHarness(t)

	stream := strings.Join([]string{
		``,
		`   `,
		`this line is not JSON at all`,
		`{"type":"system","subtype":"init","session_id":"sess-noise"}`,
		`{"type":"result",`,
		`{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","session_id":"sess-noise"}`,
	}, "\n") // deliberately no trailing newline

	if err := h.adapter.ConsumeStream(context.Background(), strings.NewReader(stream)); err != nil {
		t.Fatalf("ConsumeStream: %v", err)
	}

	if got := h.recorder.types(); len(got) != 1 || got[0] != TypeAgentFailed {
		t.Fatalf("recorded %v; want exactly [%s]", got, TypeAgentFailed)
	}
	// The last line carried no newline and was still read, and it carried no
	// session id - the init message's session was carried forward to it.
	binding, ok := h.adapter.SessionFor("sess-noise")
	if !ok {
		t.Fatal("the session was not correlated")
	}
	if binding.Events != 2 {
		t.Errorf("the session counted %d observations; want 2", binding.Events)
	}
}

func TestAnOversizedLineIsSkippedAndTheStreamContinues(t *testing.T) {
	// A line past the bound is discarded as it is read and reported as skipped.
	// The alternative - ending the read - would turn one long message on stdout
	// into a session whose outcome is never recorded.
	const limit = 64
	reader := bufio.NewReaderSize(strings.NewReader(strings.Repeat("a", 200)+"\n"+`{"ok":true}`+"\n"), 16)

	line, err := readStreamLine(reader, limit)
	if !errors.Is(err, errLineTooLong) {
		t.Fatalf("readStreamLine on an oversized line = %v, %v; want %v", line, err, errLineTooLong)
	}
	if line != nil {
		t.Errorf("the oversized line was returned anyway: %q", line)
	}

	// The reader is positioned at the start of the next line.
	line, err = readStreamLine(reader, limit)
	if err != nil {
		t.Fatalf("the line after an oversized one = %v", err)
	}
	if string(line) != `{"ok":true}` {
		t.Errorf("the line after an oversized one = %q; want the line that followed", line)
	}
}

func TestTheStreamBoundClearsALargeMessage(t *testing.T) {
	// The bound exists so that a large assistant message or tool result does not
	// end the read, so this feeds one that is larger than the bufio buffer it is
	// read through and asserts the result that follows is still recorded.
	h := newHarness(t)

	huge := `{"type":"assistant","message":{"content":[{"type":"text","text":"` +
		strings.Repeat("x", streamLineMax+16) + `"}]}}`
	stream := huge + "\n" + `{"type":"result","subtype":"success","is_error":false,"session_id":"sess-huge"}` + "\n"

	if err := h.adapter.ConsumeStream(context.Background(), strings.NewReader(stream)); err != nil {
		t.Fatalf("ConsumeStream: %v", err)
	}
	if got := h.recorder.types(); len(got) != 1 || got[0] != TypeAgentCompleted {
		t.Fatalf("recorded %v; want the result envelope after the oversized line", got)
	}
}

func TestTheLineReaderTrimsLineEndings(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"a unix line", "{\"a\":1}\n", `{"a":1}`},
		{"a windows line", "{\"a\":1}\r\n", `{"a":1}`},
		{"a final line with no newline", `{"a":1}`, `{"a":1}`},
		{"a line that is only a newline", "\n", ``},
		{"a line longer than the read buffer", strings.Repeat("z", 300) + "\n", strings.Repeat("z", 300)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := bufio.NewReaderSize(strings.NewReader(tc.input), 16)
			line, err := readStreamLine(reader, 4096)
			if err != nil {
				t.Fatalf("readStreamLine: %v", err)
			}
			if string(line) != tc.want {
				t.Errorf("readStreamLine = %q; want %q", line, tc.want)
			}
		})
	}
}

func TestTheLineReaderReportsTheEndOfInput(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(""))
	if _, err := readStreamLine(reader, 4096); !errors.Is(err, io.EOF) {
		t.Errorf("readStreamLine on an empty reader = %v; want EOF", err)
	}

	// Once the end has been reached, it keeps saying so rather than looping.
	line, err := readStreamLine(reader, 4096)
	if !errors.Is(err, io.EOF) || line != nil {
		t.Errorf("a second read past the end = %q, %v; want nil, EOF", line, err)
	}
}

func TestConsumeStreamNeedsARunningAdapter(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	err := adapter.ConsumeStream(context.Background(), strings.NewReader(""))
	if !IsCode(err, CodeNotStarted) {
		t.Fatalf("ConsumeStream before Start = %v; want %q", err, CodeNotStarted)
	}
}

func TestConsumeStreamNeedsAStream(t *testing.T) {
	h := newHarness(t)
	if err := h.adapter.ConsumeStream(context.Background(), nil); !IsCode(err, CodeStreamBroken) {
		t.Errorf("ConsumeStream(nil) = %v; want %q", err, CodeStreamBroken)
	}
}

func TestConsumeStreamReturnsWhenTheInputEnds(t *testing.T) {
	h := newHarness(t)
	if err := h.adapter.ConsumeStream(context.Background(), strings.NewReader("")); err != nil {
		t.Errorf("ConsumeStream on an empty stream = %v; want nil", err)
	}
}

func TestConsumeStreamStopsWhenTheContextEnds(t *testing.T) {
	// The caller asking the read to stop is not a fault in the stream, so it is
	// not reported as one - and the loop does not read on after being told.
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := h.adapter.ConsumeStream(ctx, strings.NewReader(
		`{"type":"result","is_error":false,"session_id":"sess-cancelled"}`+"\n"))
	if err != nil {
		t.Fatalf("ConsumeStream on a cancelled context = %v; want nil", err)
	}
	if got := h.recorder.types(); len(got) != 0 {
		t.Errorf("a cancelled read recorded %v", got)
	}
}

func TestABrokenReaderIsReportedAsABrokenStream(t *testing.T) {
	// A read that failed is a fault, unlike one that ended: something that was
	// supposed to deliver the stream stopped delivering it, and the caller has
	// to hear about it rather than see a quiet end.
	h := newHarness(t)
	err := h.adapter.ConsumeStream(context.Background(), brokenReader{})
	if !IsCode(err, CodeStreamBroken) {
		t.Fatalf("ConsumeStream on a failing reader = %v; want %q", err, CodeStreamBroken)
	}
}

// brokenReader is a reader whose underlying stream failed.
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("the pipe went away") }

func TestTheStreamNeverHoldsTheConversation(t *testing.T) {
	// §十四. The stream carries the whole conversation, and with
	// `--include-hook-events` it also carries the verbatim output of every hook.
	// None of it has a field in streamMessage, and this is the test that the
	// absence holds end to end: not in an observation, not in a recorded row.
	const (
		assistantText = "the assistant said something private"
		hookOutput    = "the hook printed a token-like string"
	)
	h := newHarness(t)
	ch := h.subscribe()

	stream := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + assistantText + `"}]}}`,
		`{"type":"user","message":{"content":"the operator typed something private"}}`,
		`{"type":"system","subtype":"init","session_id":"sess-quiet","cwd":"/home/someone/private"}`,
		`{"type":"system","subtype":"hook_response","hook_name":"Stop:1","output":"` + hookOutput + `","stdout":"` + hookOutput + `"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"` + assistantText + `","session_id":"sess-quiet"}`,
	}, "\n")

	if err := h.adapter.ConsumeStream(context.Background(), strings.NewReader(stream)); err != nil {
		t.Fatalf("ConsumeStream: %v", err)
	}

	for _, ev := range drain(ch) {
		text := string(ev.Payload)
		for _, secret := range []string{assistantText, hookOutput, "the operator typed", "/home/someone"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s carried %q: %s", ev.Kind, secret, text)
			}
		}
	}
	for _, recorded := range h.recorder.all() {
		text := string(recorded.Raw)
		for _, secret := range []string{assistantText, hookOutput, "the operator typed", "/home/someone"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s recorded %q: %s", recorded.Type, secret, text)
			}
		}
	}
}

func TestASystemSubtypeThisPhaseDoesNotReadIsIgnored(t *testing.T) {
	// Claude Code adds subtypes between versions. One that is not read is an
	// observation that does not exist, not a failure of the stream.
	h := newHarness(t)
	stream := strings.Join([]string{
		`{"type":"system","subtype":"something_from_a_later_version","session_id":"sess-new"}`,
		`{"type":"system","subtype":"status","status":"compacting","session_id":"sess-new"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-new"}`,
	}, "\n")

	if err := h.adapter.ConsumeStream(context.Background(), strings.NewReader(stream)); err != nil {
		t.Fatalf("ConsumeStream: %v", err)
	}
	if got := h.recorder.types(); len(got) != 1 || got[0] != TypeAgentCompleted {
		t.Fatalf("recorded %v; want only the result envelope", got)
	}
}
