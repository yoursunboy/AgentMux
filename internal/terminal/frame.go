package terminal

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Binary frame types.
//
// Terminal output is the only thing that travels in a binary frame, and there
// are exactly two shapes of it: a run of bytes to append to what is already on
// screen, and a screen to replace what is on screen. They are different enough
// that a client must not be able to confuse them - applying a snapshot as an
// append would duplicate it, and applying an append as a snapshot would erase
// everything before it - so they are different frame types rather than one
// type with a flag.
const (
	// FrameOutput appends bytes to the terminal.
	FrameOutput = 0x01

	// FrameSnapshot replaces the terminal's contents.
	FrameSnapshot = 0x02
)

// frameVersion is the version stamped into every binary frame.
//
// It is repeated in each frame, and in the first byte where it costs one byte
// to check, because binary frames are the part of this protocol that a client
// cannot meaningfully validate by reading: JSON that says "protocol": 1 is
// self-describing, and a run of bytes is not. A client that finds a version it
// does not know knows to stop rather than to draw.
const frameVersion = 1

// Snapshot flags.
const (
	// FlagAlternateScreen means the screen was captured from the alternate
	// buffer, which is the one a full-screen program draws in.
	FlagAlternateScreen uint8 = 1 << 0
)

// Header sizes, excluding the project identifier.
const (
	// outputHeaderBytes is version, type, two sequence numbers, and the
	// identifier length.
	outputHeaderBytes = 1 + 1 + 8 + 8 + 2

	// snapshotHeaderBytes adds the geometry and the flags.
	snapshotHeaderBytes = outputHeaderBytes + 2 + 2 + 1
)

// Frame decoding errors.
var (
	// ErrShortFrame means the frame ended before its header did.
	ErrShortFrame = errors.New("terminal: frame is shorter than its header")

	// ErrFrameVersion means the frame's first byte is not a version this
	// decoder knows.
	ErrFrameVersion = errors.New("terminal: unknown frame version")

	// ErrFrameType means the frame's type byte is not one defined here.
	ErrFrameType = errors.New("terminal: unknown frame type")

	// ErrFrameProjectID means the identifier in the header is unusable.
	ErrFrameProjectID = errors.New("terminal: frame project id is malformed")

	// ErrFrameSequence means the two sequence numbers are not in order.
	ErrFrameSequence = errors.New("terminal: frame sequence range is inverted")
)

// Frame is a decoded binary frame.
//
// It exists for the decoder, which exists for the tests and for the reader of
// the protocol documentation. The server encodes and never decodes: nothing in
// the product path reads a binary frame, because no client sends one.
//
// # The sequence pair
//
// Every frame carries two sequence numbers, and the type byte says how to read
// them:
//
//   - An output frame's payload is the concatenation of a contiguous run of
//     chunks, so FirstSequence and LastSequence are the ends of that run. The
//     run is contiguous because the server re-synchronises rather than forward
//     anything it received out of order; the pair is what lets a client check
//     that for itself instead of trusting it.
//   - A snapshot's payload is not a run of chunks at all, so both numbers are
//     the boundary: the screen renders the terminal as of LastSequence.
type Frame struct {
	Type uint8

	// FirstSequence and LastSequence bound the output this frame accounts for.
	// On a snapshot they are equal.
	FirstSequence uint64
	LastSequence  uint64

	ProjectID string

	// Cols, Rows and Flags are set on a snapshot and zero on output.
	Cols  int
	Rows  int
	Flags uint8

	// Payload is the terminal bytes: escape sequences and text for output, a
	// rendered screen for a snapshot.
	Payload []byte
}

// IsAlternate reports whether a snapshot was taken from the alternate screen.
func (f Frame) IsAlternate() bool { return f.Flags&FlagAlternateScreen != 0 }

// Accounts reports whether a frame accounts for the output sequence after
// last: that is, whether a client that has drawn everything up to last should
// draw this frame next, or has lost something.
//
// It is the rule the client applies, written once here so that it can be
// tested rather than re-derived in TypeScript. A snapshot accounts for
// everything, and so is never a gap.
func (f Frame) Accounts(last uint64) bool {
	if f.Type == FrameSnapshot {
		// A snapshot is a fresh statement about the whole terminal. It
		// supersedes whatever a client has, including nothing.
		return true
	}
	return f.FirstSequence == last+1
}

// EncodeOutput builds an output frame covering a contiguous run of chunks.
//
// The payload is not copied. A frame is written once and then dropped, so
// copying every screenful of a build's output to own bytes that are already
// owned by the chunks it came from would be a copy per frame for no reader's
// benefit.
func EncodeOutput(projectID string, first, last uint64, payload []byte) ([]byte, error) {
	if last < first {
		return nil, fmt.Errorf("%w: %d..%d", ErrFrameSequence, first, last)
	}
	header, err := frameHeader(outputHeaderBytes, FrameOutput, projectID, first, last, nil)
	if err != nil {
		return nil, err
	}
	frame := make([]byte, 0, len(header)+len(payload))
	frame = append(frame, header...)
	frame = append(frame, payload...)
	return frame, nil
}

// EncodeSnapshot builds a snapshot frame.
//
// The payload is a Screen's rendered bytes, which already switch the client to
// the right screen buffer and place its cursor. The geometry is carried
// separately because the client has to size its terminal to it *before*
// writing those bytes: a screen rendered for 120 columns, written into an
// 80-column terminal, wraps in the wrong places and cannot be recovered.
func EncodeSnapshot(projectID string, boundary uint64, cols, rows int, flags uint8, payload []byte) ([]byte, error) {
	if cols < 0 || cols > MaxCols || rows < 0 || rows > MaxRows {
		return nil, fmt.Errorf("terminal: snapshot geometry %dx%d does not fit the protocol", cols, rows)
	}
	var geometry [5]byte
	binary.BigEndian.PutUint16(geometry[0:2], uint16(cols))
	binary.BigEndian.PutUint16(geometry[2:4], uint16(rows))
	geometry[4] = flags

	header, err := frameHeader(snapshotHeaderBytes, FrameSnapshot, projectID, boundary, boundary, geometry[:])
	if err != nil {
		return nil, err
	}
	frame := make([]byte, 0, len(header)+len(payload))
	frame = append(frame, header...)
	frame = append(frame, payload...)
	return frame, nil
}

// frameHeader builds the common part of a frame, with the type-specific middle
// already appended.
func frameHeader(size int, frameType uint8, projectID string, first, last uint64, middle []byte) ([]byte, error) {
	if projectID == "" || len(projectID) > MaxProjectIDLen {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameProjectID, len(projectID))
	}
	header := make([]byte, 0, size+len(projectID))
	header = append(header, frameVersion, frameType)
	header = binary.BigEndian.AppendUint64(header, first)
	header = binary.BigEndian.AppendUint64(header, last)
	header = binary.BigEndian.AppendUint16(header, uint16(len(projectID)))
	header = append(header, projectID...)
	header = append(header, middle...)
	return header, nil
}

// DecodeFrame parses a binary frame.
//
// It reads the whole frame rather than returning a header to be followed by a
// payload, because the payload is everything to the end: there is no length
// field to skip to, so a decoder that wanted to defer reading it would have to
// keep the frame's bytes around while claiming not to have read them.
func DecodeFrame(data []byte) (Frame, error) {
	if len(data) < outputHeaderBytes {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrShortFrame, len(data))
	}
	if data[0] != frameVersion {
		return Frame{}, fmt.Errorf("%w: %d", ErrFrameVersion, data[0])
	}
	frame := Frame{
		Type:          data[1],
		FirstSequence: binary.BigEndian.Uint64(data[2:10]),
		LastSequence:  binary.BigEndian.Uint64(data[10:18]),
	}
	if frame.LastSequence < frame.FirstSequence {
		return Frame{}, fmt.Errorf("%w: %d..%d", ErrFrameSequence, frame.FirstSequence, frame.LastSequence)
	}
	idLen := int(binary.BigEndian.Uint16(data[18:20]))
	if idLen == 0 || idLen > MaxProjectIDLen {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrameProjectID, idLen)
	}
	body := data[outputHeaderBytes:]
	if len(body) < idLen {
		return Frame{}, fmt.Errorf("%w: declares %d bytes of id, has %d", ErrShortFrame, idLen, len(body))
	}
	frame.ProjectID = string(body[:idLen])
	body = body[idLen:]

	switch frame.Type {
	case FrameOutput:
		frame.Payload = body
	case FrameSnapshot:
		if len(body) < 5 {
			return Frame{}, fmt.Errorf("%w: snapshot header needs 5 bytes, has %d", ErrShortFrame, len(body))
		}
		frame.Cols = int(binary.BigEndian.Uint16(body[0:2]))
		frame.Rows = int(binary.BigEndian.Uint16(body[2:4]))
		frame.Flags = body[4]
		frame.Payload = body[5:]
	default:
		return Frame{}, fmt.Errorf("%w: 0x%02x", ErrFrameType, frame.Type)
	}
	return frame, nil
}
