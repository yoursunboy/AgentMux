package terminal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// These tests are about the binary frame: the only part of the protocol a
// client cannot validate by reading it.
//
// Control messages are JSON, and JSON that says `"protocol": 1` describes
// itself. A run of terminal bytes does not: the version, the type and the two
// sequence numbers have to be where the documentation says they are, and a
// client that reads them from the wrong offsets draws something wrong rather
// than failing. That is why the layout is tested byte by byte here rather than
// only through the round trip - a round trip through a matched pair of encoder
// and decoder passes whatever offsets both of them happen to use.

const frameProject = "p_0123456789abcdef0123"

// TestAnOutputFrameRoundTrips is the base case.
func TestAnOutputFrameRoundTrips(t *testing.T) {
	payload := []byte("\x1b[32mok\x1b[0m\r\n中文\r\n")

	encoded, err := EncodeOutput(frameProject, 7, 9, payload)
	if err != nil {
		t.Fatalf("EncodeOutput returned an error: %v", err)
	}
	frame, err := DecodeFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeFrame returned an error: %v", err)
	}

	if frame.Type != FrameOutput {
		t.Errorf("type = %#x, want %#x", frame.Type, FrameOutput)
	}
	if frame.FirstSequence != 7 || frame.LastSequence != 9 {
		t.Errorf("sequence range = %d..%d, want 7..9", frame.FirstSequence, frame.LastSequence)
	}
	if frame.ProjectID != frameProject {
		t.Errorf("project = %q, want %q", frame.ProjectID, frameProject)
	}
	if !bytes.Equal(frame.Payload, payload) {
		t.Errorf("payload = %q, want %q", frame.Payload, payload)
	}
	// An output frame carries no geometry; a client that read cols and rows off
	// one would resize its terminal to zero.
	if frame.Cols != 0 || frame.Rows != 0 || frame.Flags != 0 {
		t.Errorf("an output frame carries geometry: %dx%d flags %#x", frame.Cols, frame.Rows, frame.Flags)
	}
}

// TestTheFrameLayoutIsWhereTheDocumentationSaysItIs pins the header offsets.
//
// The decoder is checked against bytes written by hand rather than against the
// encoder, because a decoder tested only against its own encoder would agree
// with it about any layout at all.
func TestTheFrameLayoutIsWhereTheDocumentationSaysItIs(t *testing.T) {
	id := "p_abc"

	manual := []byte{1, FrameOutput}
	manual = binary.BigEndian.AppendUint64(manual, 42)
	manual = binary.BigEndian.AppendUint64(manual, 43)
	manual = binary.BigEndian.AppendUint16(manual, uint16(len(id)))
	manual = append(manual, id...)
	manual = append(manual, "bytes"...)

	frame, err := DecodeFrame(manual)
	if err != nil {
		t.Fatalf("DecodeFrame returned an error: %v", err)
	}
	if frame.Type != FrameOutput || frame.FirstSequence != 42 || frame.LastSequence != 43 {
		t.Errorf("decoded %#x %d..%d, want %#x 42..43", frame.Type, frame.FirstSequence, frame.LastSequence, FrameOutput)
	}
	if frame.ProjectID != id || string(frame.Payload) != "bytes" {
		t.Errorf("decoded project %q payload %q, want %q and %q", frame.ProjectID, frame.Payload, id, "bytes")
	}

	// And the encoder writes the same bytes for the same frame.
	rebuilt, err := EncodeOutput(id, 42, 43, []byte("bytes"))
	if err != nil {
		t.Fatalf("EncodeOutput returned an error: %v", err)
	}
	if !bytes.Equal(rebuilt, manual) {
		t.Errorf("the encoder wrote:\n% x\nwant:\n% x", rebuilt, manual)
	}
}

// TestASnapshotFrameRoundTrips checks the extra fields a screen needs.
func TestASnapshotFrameRoundTrips(t *testing.T) {
	screen := []byte("\x1b[0m\x1b[2J\x1b[Hhello\x1b[3;4H")

	encoded, err := EncodeSnapshot(frameProject, 1234, 120, 30, FlagAlternateScreen, screen)
	if err != nil {
		t.Fatalf("EncodeSnapshot returned an error: %v", err)
	}
	frame, err := DecodeFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeFrame returned an error: %v", err)
	}

	if frame.Type != FrameSnapshot {
		t.Errorf("type = %#x, want %#x", frame.Type, FrameSnapshot)
	}
	// Both numbers are the boundary on a snapshot. The screen is a statement
	// about the terminal as of one moment, not a range of chunks.
	if frame.FirstSequence != 1234 || frame.LastSequence != 1234 {
		t.Errorf("boundary = %d..%d, want 1234..1234", frame.FirstSequence, frame.LastSequence)
	}
	if frame.Cols != 120 || frame.Rows != 30 {
		t.Errorf("geometry = %dx%d, want 120x30", frame.Cols, frame.Rows)
	}
	if !frame.IsAlternate() {
		t.Error("the snapshot lost the alternate-screen flag")
	}
	if !bytes.Equal(frame.Payload, screen) {
		t.Errorf("payload = %q, want %q", frame.Payload, screen)
	}
}

// TestTheGapRuleIsWrittenOnce is the client's rule, tested where it is defined.
//
// A client that has drawn everything up to sequence n has to be able to ask
// "does this frame follow?" without re-deriving the answer from a frame's
// internals. Getting it wrong is silent in both directions: too strict and a
// client re-synchronises constantly, too loose and it draws a terminal with a
// hole in it and never knows.
func TestTheGapRuleIsWrittenOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame Frame
		last  uint64
		want  bool
	}{
		{
			name:  "a single chunk that follows",
			frame: Frame{Type: FrameOutput, FirstSequence: 8, LastSequence: 8},
			last:  7,
			want:  true,
		},
		{
			name: "a batch that follows",
			// The case the two-sequence design exists for: a frame covering
			// chunks 8, 9 and 10 follows sequence 7 even though its last number
			// is not 8.
			frame: Frame{Type: FrameOutput, FirstSequence: 8, LastSequence: 10},
			last:  7,
			want:  true,
		},
		{
			name:  "a batch that skips",
			frame: Frame{Type: FrameOutput, FirstSequence: 9, LastSequence: 10},
			last:  7,
			want:  false,
		},
		{
			name:  "a frame already drawn",
			frame: Frame{Type: FrameOutput, FirstSequence: 5, LastSequence: 7},
			last:  7,
			want:  false,
		},
		{
			name:  "a snapshot replaces whatever is there",
			frame: Frame{Type: FrameSnapshot, FirstSequence: 900, LastSequence: 900},
			last:  7,
			want:  true,
		},
		{
			name:  "the first frame after a boundary of zero",
			frame: Frame{Type: FrameOutput, FirstSequence: 1, LastSequence: 1},
			last:  0,
			want:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.frame.Accounts(tc.last); got != tc.want {
				t.Errorf("Accounts(%d) = %v, want %v", tc.last, got, tc.want)
			}
		})
	}
}

// TestAFrameThatCannotBeDrawnIsRefused is the decoder's half of the contract.
//
// Every case here is a frame a client must not draw. The alternative to
// refusing is guessing, and a guess about where a project identifier ends is a
// guess about which terminal the bytes belong to.
func TestAFrameThatCannotBeDrawnIsRefused(t *testing.T) {
	good, err := EncodeOutput(frameProject, 1, 1, []byte("hello"))
	if err != nil {
		t.Fatalf("EncodeOutput returned an error: %v", err)
	}
	snapshot, err := EncodeSnapshot(frameProject, 1, 80, 24, 0, []byte("screen"))
	if err != nil {
		t.Fatalf("EncodeSnapshot returned an error: %v", err)
	}

	// A frame whose identifier length is legal but larger than what follows it:
	// the decoder must not read past the end looking for the rest of the id.
	//
	// The number matters. Above MaxProjectIDLen the length is refused outright,
	// which is a different check and a different error; the case worth having is
	// the one where the length is one the protocol allows and the frame simply
	// does not contain that many bytes.
	truncatedID := append([]byte(nil), good...)
	binary.BigEndian.PutUint16(truncatedID[18:20], 40)

	for _, tc := range []struct {
		name  string
		frame []byte
		want  error
	}{
		{"empty", nil, ErrShortFrame},
		{"shorter than a header", good[:outputHeaderBytes-1], ErrShortFrame},
		{
			"a version from the future",
			append([]byte{frameVersion + 1}, good[1:]...),
			ErrFrameVersion,
		},
		{
			"an unknown type",
			func() []byte {
				f := append([]byte(nil), good...)
				f[1] = 0x7f
				return f
			}(),
			ErrFrameType,
		},
		{
			"an inverted sequence range",
			func() []byte {
				f, err := EncodeOutput(frameProject, 5, 5, nil)
				if err != nil {
					t.Fatalf("EncodeOutput returned an error: %v", err)
				}
				binary.BigEndian.PutUint64(f[2:10], 9)
				return f
			}(),
			ErrFrameSequence,
		},
		{
			"an empty project identifier",
			func() []byte {
				f := append([]byte(nil), good...)
				binary.BigEndian.PutUint16(f[18:20], 0)
				return f
			}(),
			ErrFrameProjectID,
		},
		{
			"an identifier longer than the protocol allows",
			func() []byte {
				f := append([]byte(nil), good...)
				binary.BigEndian.PutUint16(f[18:20], MaxProjectIDLen+1)
				return f
			}(),
			ErrFrameProjectID,
		},
		{"an identifier longer than the frame", truncatedID, ErrShortFrame},
		{"a snapshot with no geometry", snapshot[:snapshotHeaderBytes+len(frameProject)-1], ErrShortFrame},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeFrame(tc.frame); !errors.Is(err, tc.want) {
				t.Errorf("DecodeFrame returned %v, want %v", err, tc.want)
			}
		})
	}
}

// TestAnEncoderRefusesWhatADecoderWouldRefuse keeps the two ends agreeing about
// what a frame is, so that a malformed frame is never produced in the first
// place.
func TestAnEncoderRefusesWhatADecoderWouldRefuse(t *testing.T) {
	if _, err := EncodeOutput(frameProject, 9, 5, nil); !errors.Is(err, ErrFrameSequence) {
		t.Errorf("an inverted range encoded as %v, want %v", err, ErrFrameSequence)
	}
	for _, tc := range []struct {
		name      string
		projectID string
	}{
		{"no project", ""},
		{"a project id longer than the protocol allows", string(bytes.Repeat([]byte("p"), MaxProjectIDLen+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EncodeOutput(tc.projectID, 1, 1, nil); !errors.Is(err, ErrFrameProjectID) {
				t.Errorf("EncodeOutput returned %v, want %v", err, ErrFrameProjectID)
			}
		})
	}

	// A geometry the protocol cannot carry is refused rather than truncated: a
	// client that read a wrapped uint16 would size its terminal to 5792
	// columns.
	for _, tc := range []struct{ cols, rows int }{
		{MaxCols + 1, 24},
		{80, MaxRows + 1},
		{-1, 24},
		{80, -1},
	} {
		if _, err := EncodeSnapshot(frameProject, 1, tc.cols, tc.rows, 0, nil); err == nil {
			t.Errorf("a snapshot of %dx%d encoded without complaint", tc.cols, tc.rows)
		}
	}
}

// TestARoundTripSurvivesEveryByteValue is §十三 at the frame boundary.
//
// Terminal output is not text. Every byte from 0 to 255 reaches the wire and
// comes back unchanged, including the ones that are not valid UTF-8 and the
// ones that are the frame header's own separators if anything were tempted to
// parse the payload.
func TestARoundTripSurvivesEveryByteValue(t *testing.T) {
	payload := make([]byte, 0, 256)
	for b := 0; b < 256; b++ {
		payload = append(payload, byte(b))
	}

	encoded, err := EncodeOutput(frameProject, 1, 1, payload)
	if err != nil {
		t.Fatalf("EncodeOutput returned an error: %v", err)
	}
	frame, err := DecodeFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeFrame returned an error: %v", err)
	}
	if !bytes.Equal(frame.Payload, payload) {
		t.Errorf("the payload changed on the way through: got %d bytes, want %d", len(frame.Payload), len(payload))
	}
}

// TestAFrameWithNoPayloadIsStillAFrame covers the empty output frame.
//
// It happens: tmux can emit an %output with no bytes, and a client has to be
// able to tell "nothing was produced" from "the frame was truncated", which it
// does by the frame being complete rather than by its length.
func TestAFrameWithNoPayloadIsStillAFrame(t *testing.T) {
	encoded, err := EncodeOutput(frameProject, 4, 4, nil)
	if err != nil {
		t.Fatalf("EncodeOutput returned an error: %v", err)
	}
	frame, err := DecodeFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeFrame returned an error: %v", err)
	}
	if len(frame.Payload) != 0 {
		t.Errorf("payload = %q, want nothing", frame.Payload)
	}
	if frame.FirstSequence != 4 || frame.LastSequence != 4 {
		t.Errorf("sequence range = %d..%d, want 4..4", frame.FirstSequence, frame.LastSequence)
	}
}
