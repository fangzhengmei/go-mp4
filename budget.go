package mp4

import (
	"errors"
	"fmt"
	"io"
)

var (
	// ErrReadBudgetBytesExceeded is returned when the cumulative number of bytes
	// read from the underlying reader exceeds ReadBudget.MaxReadBytes.
	// Seek operations do not consume the byte budget; only bytes actually
	// transferred by Read calls are counted.
	ErrReadBudgetBytesExceeded = errors.New("mp4 read budget exceeded: cumulative read bytes")

	// ErrReadBudgetDepthExceeded is returned when a box would be parsed at a
	// nesting depth greater than ReadBudget.MaxDepth. Root-level boxes are at
	// depth 1, their children are at depth 2, and so on.
	ErrReadBudgetDepthExceeded = errors.New("mp4 read budget exceeded: nesting depth")

	// ErrReadBudgetBoxesExceeded is returned when the number of parsed boxes
	// would exceed ReadBudget.MaxBoxes.
	ErrReadBudgetBoxesExceeded = errors.New("mp4 read budget exceeded: box count")
)

// ReadBudget limits the work performed by ReadBoxStructureWithBudget.
// The zero value of every field disables the corresponding limit, so a nil
// *ReadBudget or an empty ReadBudget imposes no restrictions at all.
type ReadBudget struct {
	// MaxReadBytes limits the cumulative number of bytes read from the
	// underlying reader. Bytes re-read after a Seek are counted again, while
	// Seek operations themselves consume no budget.
	MaxReadBytes uint64

	// MaxDepth limits the nesting depth of boxes. Root-level boxes have depth 1.
	MaxDepth uint64

	// MaxBoxes limits the total number of boxes that are parsed and delivered
	// to the handler.
	MaxBoxes uint64
}

// readBudgetState carries the mutable counters shared by the whole recursive
// box-structure traversal. A nil pointer means that budgeting is disabled and
// every call must behave exactly like the non-budgeted code path.
type readBudgetState struct {
	budget ReadBudget

	readBytes uint64
	boxCount  uint64
}

// checkDepth reports ErrReadBudgetDepthExceeded when parsing a box at depth
// would cross the configured depth limit.
func (s *readBudgetState) checkDepth(depth uint64) error {
	if s == nil {
		return nil
	}
	if s.budget.MaxDepth == 0 || depth <= s.budget.MaxDepth {
		return nil
	}
	return fmt.Errorf("%w: depth=%d limit=%d", ErrReadBudgetDepthExceeded, depth, s.budget.MaxDepth)
}

// checkBoxCount reserves one more parsed box and reports
// ErrReadBudgetBoxesExceeded when the box-count limit would be crossed.
func (s *readBudgetState) checkBoxCount() error {
	if s == nil {
		return nil
	}
	if s.budget.MaxBoxes == 0 || s.boxCount < s.budget.MaxBoxes {
		s.boxCount++
		return nil
	}
	return fmt.Errorf("%w: count=%d limit=%d", ErrReadBudgetBoxesExceeded, s.boxCount, s.budget.MaxBoxes)
}

// countingReadSeeker wraps an io.ReadSeeker and accumulates the number of
// bytes transferred through Read. It enforces the byte budget at the actual
// Read boundary so that truncated inputs and budget exhaustion remain
// distinguishable while every BoxInfo offset keeps being derived from real
// file positions.
type countingReadSeeker struct {
	r     io.ReadSeeker
	state *readBudgetState
}

func (c *countingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return c.r.Seek(offset, whence)
}

func (c *countingReadSeeker) Read(p []byte) (int, error) {
	if c.state == nil || c.state.budget.MaxReadBytes == 0 {
		return c.r.Read(p)
	}

	remaining := c.state.budget.MaxReadBytes - c.state.readBytes
	if remaining == 0 {
		return 0, c.probeExhausted()
	}

	if uint64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := c.r.Read(p)
	c.state.readBytes += uint64(n)
	if c.state.readBytes >= c.state.budget.MaxReadBytes && err != nil {
		// The budget became exhausted on this call. A short read indicates real
		// truncation, while a fully-satisfied read is resolved on the next call
		// by probeExhausted so an exact fit still ends cleanly at EOF.
		if n < len(p) {
			return n, err
		}
		err = nil
	}
	return n, err
}

// probeExhausted distinguishes a clean end-of-input from byte-budget
// exhaustion once the whole budget has been consumed. If no further byte can
// be read, io.EOF is returned (the root loop treats that as the end of the
// container). Otherwise the consumed byte is rewound and the budget error is
// returned without disturbing any media offset.
func (c *countingReadSeeker) probeExhausted() error {
	var one [1]byte
	n, err := c.r.Read(one[:])
	if n > 0 {
		if _, seekErr := c.r.Seek(-int64(n), io.SeekCurrent); seekErr != nil {
			return seekErr
		}
		return fmt.Errorf("%w: limit=%d", ErrReadBudgetBytesExceeded, c.state.budget.MaxReadBytes)
	}
	if err == io.ErrUnexpectedEOF {
		return io.EOF
	}
	if err == nil {
		return io.EOF
	}
	return err
}

// ReadBoxStructureWithBudget works like ReadBoxStructure but enforces the
// given ReadBudget. A nil budget keeps the legacy behavior of
// ReadBoxStructure unchanged. When any budget is exhausted, the handler is
// not invoked for the offending box (nor for any later box), and one of
// ErrReadBudgetBytesExceeded, ErrReadBudgetDepthExceeded or
// ErrReadBudgetBoxesExceeded is returned; callers can distinguish the cause
// with errors.Is.
func ReadBoxStructureWithBudget(r io.ReadSeeker, budget *ReadBudget, handler ReadHandler, params ...interface{}) ([]interface{}, error) {
	if budget == nil {
		return ReadBoxStructure(r, handler, params...)
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	state := &readBudgetState{budget: *budget}
	cr := &countingReadSeeker{r: r, state: state}
	return readBoxStructure(cr, 0, true, nil, Context{}, state, handler, params)
}
