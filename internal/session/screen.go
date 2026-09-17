package session

import (
	"bytes"
	"strconv"
)

// This file is the recovery half of the terminal contract.
//
// The live stream is a byte stream: it is what the program wrote, in the order
// it wrote it. A screen is not. A terminal's screen is the *result* of applying
// that stream, and the two are not interchangeable - you cannot reconstruct one
// from a window of the other, because a screen is a fixed-size grid that has
// forgotten everything that scrolled off it.
//
// That distinction is why a client that has just connected, or one whose stream
// was interrupted, is given a Screen rather than a replay. Replaying an output
// window would show whatever happened to be in the buffer, at the wrong size,
// with the cursor in the wrong place.
//
// # What a Screen is not
//
// A Screen is not a snapshot of the *program*. It is the visible pane, plus
// enough geometry to draw it. Terminal emulator state that tmux does not
// report through capture-pane - the client's own scrollback, bracketed-paste
// mode, mouse reporting, the alternate screen's separate saved cursor - is not
// in it. See docs/TERMINAL.md for the exact list of what survives.
type Screen struct {
	// Data is the pane's rows, oldest first, in terminal byte form: escape
	// sequences intact and rows separated by CRLF. It is ready to write to a
	// terminal that has been reset. Use Render rather than writing it directly,
	// because a screen also has to say which buffer it is on and where the
	// cursor is.
	Data []byte

	// Cols and Rows are the pane's size at capture time.
	Cols int
	Rows int

	// CursorX and CursorY are zero-based, and are relative to the *visible*
	// pane - that is, to the last Rows rows of Data - not to the start of the
	// captured history.
	CursorX int
	CursorY int

	// Alternate reports whether the pane is on the alternate screen. A
	// full-screen program is; a shell at a prompt is not. A client that draws
	// this screen must be on the same buffer, or the program's next full
	// repaint will land on the wrong one.
	Alternate bool
}

// Escape sequences Render uses. They are named because each one is a decision
// rather than a detail.
const (
	// enterAlt and leaveAlt select the alternate screen buffer. Sending the
	// matching one first makes the result independent of whatever the client
	// was showing before, which is the point of a resync.
	enterAlt = "\x1b[?1049h"
	leaveAlt = "\x1b[?1049l"

	// resetScreen clears attributes, erases the display, and homes the cursor.
	//
	// It is CSI 2J rather than CSI 3J: 2J erases the visible screen and leaves
	// the client's scrollback alone, which matters because scrollback is local
	// to each client (docs/TERMINAL.md §Scroll). Erasing it here would let one
	// client's resync destroy another's history - and its own.
	resetScreen = "\x1b[0m\x1b[2J\x1b[H"
)

// Render returns the byte sequence that reproduces this screen on a terminal,
// starting from any state.
//
// It is self-contained on purpose: it selects the buffer, clears, draws, and
// places the cursor. A client writes it and is then showing exactly what the
// pane is showing.
func (s Screen) Render() []byte {
	var buf bytes.Buffer
	buf.Grow(len(s.Data) + 64)

	if s.Alternate {
		buf.WriteString(enterAlt)
	} else {
		buf.WriteString(leaveAlt)
	}
	buf.WriteString(resetScreen)
	buf.Write(s.Data)

	// The cursor is placed after the content rather than being implied by it.
	// capture-pane does not emit a cursor, and the last row of a full screen
	// does not end with a newline, so without this the client would leave the
	// cursor wherever the final byte happened to put it - which for a
	// full-screen program is never where its input line is.
	if s.Rows > 0 && s.Cols > 0 {
		buf.WriteString(cursorTo(s.CursorY+1, s.CursorX+1))
	}
	return buf.Bytes()
}

// cursorTo builds a CSI cursor-position sequence. Both arguments are one-based,
// which is what the sequence itself uses.
func cursorTo(row, col int) string {
	return "\x1b[" + strconv.Itoa(row) + ";" + strconv.Itoa(col) + "H"
}

// rowsToCRLF converts a captured pane into terminal byte form.
//
// It exists because capture-pane's output is not a byte stream. It is a
// line-oriented rendering of a grid, and its "\n" is the separator between two
// rows - not a byte the pane ever wrote. Writing a bare LF to a terminal moves
// down a row and keeps the column, so a screen captured this way and written
// unchanged staircases across the display.
//
// Two things follow, and both are deliberate:
//
//   - The trailing newline after the last row is dropped. It terminates the
//     last row rather than separating it from another one, and keeping it would
//     scroll the whole screen up by one line.
//   - \r is not escaped, because a captured row cannot contain one. capture-pane
//     renders cells; a carriage return is not a cell.
//
// The bytes *within* a row are passed through untouched, including every escape
// sequence and every byte >= 0x80.
func rowsToCRLF(captured []byte) []byte {
	if len(captured) == 0 {
		return nil
	}
	rows := bytes.Split(captured, []byte{'\n'})
	if len(rows) > 0 && len(rows[len(rows)-1]) == 0 {
		rows = rows[:len(rows)-1]
	}
	return bytes.Join(rows, []byte("\r\n"))
}
