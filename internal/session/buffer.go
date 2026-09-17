package session

// chunkBuffer keeps the most recent output of one runtime, so that a client
// which reconnects can ask for what it missed instead of being handed a
// screenful and wished luck.
//
// It is bounded twice over - by chunk count and by total bytes - because
// either limit alone can be defeated: a build that prints one enormous line
// defeats a chunk count, and a program that emits a byte at a time defeats a
// byte count.
//
// This is the reason sequence numbers exist. Without them a client that
// reconnects cannot tell a gap from a pause; with them, "everything after
// sequence N" is a question with an exact answer.
type chunkBuffer struct {
	maxChunks int
	maxBytes  int

	ring  []Chunk
	start int
	count int
	bytes int
}

// newChunkBuffer builds a buffer, substituting a default for any limit that is
// not positive so that a misconfiguration cannot produce an unbounded buffer.
func newChunkBuffer(maxChunks, maxBytes int) *chunkBuffer {
	if maxChunks <= 0 {
		maxChunks = DefaultHistoryChunks
	}
	if maxBytes <= 0 {
		maxBytes = DefaultHistoryBytes
	}
	return &chunkBuffer{
		maxChunks: maxChunks,
		maxBytes:  maxBytes,
		ring:      make([]Chunk, maxChunks),
	}
}

// DefaultHistoryChunks and DefaultHistoryBytes bound one runtime's in-memory
// output history.
const (
	DefaultHistoryChunks = 2048
	DefaultHistoryBytes  = 4 << 20
)

// append adds a chunk, discarding the oldest output once a limit is reached.
//
// The chunk's data is stored by reference. Callers must treat what they read
// back as read-only: it is the same slice handed to every other reader.
func (b *chunkBuffer) append(c Chunk) {
	if b.maxChunks == 0 {
		return
	}
	if b.count == b.maxChunks {
		// Full: the oldest chunk makes room for this one.
		old := b.ring[b.start]
		b.bytes -= len(old.Data)
		b.ring[b.start] = Chunk{}
		b.start = (b.start + 1) % b.maxChunks
		b.count--
	}
	b.ring[(b.start+b.count)%b.maxChunks] = c
	b.count++
	b.bytes += len(c.Data)

	for b.bytes > b.maxBytes && b.count > 1 {
		old := b.ring[b.start]
		b.bytes -= len(old.Data)
		b.ring[b.start] = Chunk{}
		b.start = (b.start + 1) % b.maxChunks
		b.count--
	}
}

// since returns the buffered chunks whose sequence is greater than seq, oldest
// first.
//
// A request for everything after the newest chunk gets nothing and allocates
// nothing. That case is not hypothetical: it is what a subscriber asks for
// when it wants to register for what comes next and has a screen that already
// covers what came before, and it is the shape of every reconnect.
func (b *chunkBuffer) since(seq uint64) []Chunk {
	if seq >= b.latest() {
		return nil
	}
	out := make([]Chunk, 0, b.count)
	for i := 0; i < b.count; i++ {
		c := b.ring[(b.start+i)%b.maxChunks]
		if c.Sequence > seq {
			out = append(out, c)
		}
	}
	return out
}

// latest returns the highest buffered sequence, or 0 when empty.
func (b *chunkBuffer) latest() uint64 {
	if b.count == 0 {
		return 0
	}
	return b.ring[(b.start+b.count-1)%b.maxChunks].Sequence
}

// reset discards every buffered chunk. It is used when a runtime is destroyed
// and its sequence numbering starts again.
func (b *chunkBuffer) reset() {
	for i := range b.ring {
		b.ring[i] = Chunk{}
	}
	b.start = 0
	b.count = 0
	b.bytes = 0
}
