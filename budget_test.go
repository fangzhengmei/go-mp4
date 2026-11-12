package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func smallBox(boxType string, payload []byte) []byte {
	b := make([]byte, SmallHeaderSize+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], boxType)
	copy(b[SmallHeaderSize:], payload)
	return b
}

func largeBox(boxType string, payload []byte) []byte {
	b := make([]byte, LargeHeaderSize+len(payload))
	binary.BigEndian.PutUint32(b, 1)
	copy(b[4:], boxType)
	binary.BigEndian.PutUint64(b[SmallHeaderSize:], uint64(len(b)))
	copy(b[LargeHeaderSize:], payload)
	return b
}

// buildBudgetSample builds:
//
//	[moov [trak [mdia [free][zzzz(unknown)]]][free]]
//
// all boxes use 8-byte headers, so every delivered callback costs exactly
// eight read bytes when the handler only expands containers.
func buildBudgetSample() []byte {
	leafFree := smallBox("free", nil)
	leafUnknown := smallBox("zzzz", nil)
	mdia := smallBox("mdia", append(append([]byte{}, leafFree...), leafUnknown...))
	trak := smallBox("trak", mdia)
	rootFree := smallBox("free", nil)
	return append(append([]byte{}, smallBox("moov", trak)...), rootFree...)
}

var budgetContainers = map[BoxType]bool{
	BoxTypeMoov(): true,
	BoxTypeTrak(): true,
	BoxTypeMdia(): true,
}

// scanWithBudget records every delivered box and expands known containers.
func scanWithBudget(t *testing.T, data []byte, budget *ReadBudget) ([]BoxInfo, int, error) {
	t.Helper()

	var seen []BoxInfo
	handler := func(h *ReadHandle) (interface{}, error) {
		seen = append(seen, h.BoxInfo)
		if budgetContainers[h.BoxInfo.Type] {
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	if budget == nil {
		_, err := ReadBoxStructure(bytes.NewReader(data), handler)
		return seen, len(seen), err
	}
	_, err := ReadBoxStructureWithBudget(bytes.NewReader(data), budget, handler)
	return seen, len(seen), err
}

func TestReadBudgetDisabledKeepsLegacyBehavior(t *testing.T) {
	data := buildBudgetSample()

	legacy, nLegacy, errLegacy := scanWithBudget(t, data, nil)
	require.NoError(t, errLegacy)

	nilBudget, nNil, errNil := scanWithBudget(t, data, nil)
	require.NoError(t, errNil)

	zeroBudget, nZero, errZero := scanWithBudget(t, data, &ReadBudget{})
	require.NoError(t, errZero)

	require.Equal(t, 6, nLegacy)
	assert.Equal(t, nLegacy, nNil)
	assert.Equal(t, nLegacy, nZero)
	for i := range legacy {
		assert.Equal(t, legacy[i].Offset, nilBudget[i].Offset)
		assert.Equal(t, legacy[i].Size, nilBudget[i].Size)
		assert.Equal(t, legacy[i].Offset, zeroBudget[i].Offset)
		assert.Equal(t, legacy[i].Size, zeroBudget[i].Size)
	}
}

func TestReadBudgetMaxBoxes(t *testing.T) {
	data := buildBudgetSample()

	// Exact fit must succeed and deliver every box.
	_, n, err := scanWithBudget(t, data, &ReadBudget{MaxBoxes: 6})
	require.NoError(t, err)
	assert.Equal(t, 6, n)

	full, _, err := scanWithBudget(t, data, nil)
	require.NoError(t, err)

	// One short: the offending box and all later ones must not be delivered.
	limited, nLimited, err := scanWithBudget(t, data, &ReadBudget{MaxBoxes: 5})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReadBudgetBoxesExceeded))
	assert.False(t, errors.Is(err, ErrReadBudgetDepthExceeded))
	assert.False(t, errors.Is(err, ErrReadBudgetBytesExceeded))
	assert.Equal(t, 5, nLimited)

	// All delivered boxes must keep the same media offsets as the full run.
	for i, bi := range limited {
		assert.Equal(t, full[i].Offset, bi.Offset)
		assert.Equal(t, full[i].Size, bi.Size)
	}

	// A zero field disables the limit, consistent with ReadBudget semantics.
	_, nZero, err := scanWithBudget(t, data, &ReadBudget{MaxBoxes: 0, MaxDepth: 1})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReadBudgetDepthExceeded))
	assert.Equal(t, 1, nZero)
}

func TestReadBudgetMaxDepth(t *testing.T) {
	data := buildBudgetSample()

	// moov(1) > trak(2) > mdia(3) > free/zzzz(4)
	_, n, err := scanWithBudget(t, data, &ReadBudget{MaxDepth: 4})
	require.NoError(t, err)
	assert.Equal(t, 6, n)

	limited, nLimited, err := scanWithBudget(t, data, &ReadBudget{MaxDepth: 3})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReadBudgetDepthExceeded))
	assert.Equal(t, 3, nLimited) // moov, trak, mdia; deeper boxes are rejected

	// Depth-4 boxes must never be delivered.
	for _, bi := range limited {
		assert.NotEqual(t, StrToBoxType("zzzz"), bi.Type)
	}

	// Offsets of delivered boxes stay accurate.
	full, _, err := scanWithBudget(t, data, nil)
	require.NoError(t, err)
	for i, bi := range limited {
		assert.Equal(t, full[i].Offset, bi.Offset)
	}
}

func TestReadBudgetMaxReadBytes(t *testing.T) {
	data := buildBudgetSample()

	// Six small headers => only 48 bytes are read when containers are expanded.
	const totalHeaderBytes = 6 * SmallHeaderSize

	_, n, err := scanWithBudget(t, data, &ReadBudget{MaxReadBytes: totalHeaderBytes})
	require.NoError(t, err)
	assert.Equal(t, 6, n)

	limited, nLimited, err := scanWithBudget(t, data, &ReadBudget{MaxReadBytes: totalHeaderBytes - 1})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReadBudgetBytesExceeded))
	assert.False(t, errors.Is(err, io.EOF))
	// The sixth header cannot be completed, so only five boxes are delivered.
	assert.Equal(t, 5, nLimited)

	full, _, err := scanWithBudget(t, data, nil)
	require.NoError(t, err)
	for i, bi := range limited {
		assert.Equal(t, full[i].Offset, bi.Offset)
		assert.Equal(t, full[i].Size, bi.Size)
	}
}

func TestReadBudgetTruncatedInput(t *testing.T) {
	data := buildBudgetSample()

	// A partial header at the root level is treated as end-of-input by the
	// legacy parser. The budgeted path must behave identically and must keep
	// every delivered offset accurate rather than blaming the byte budget.
	partialHeader := []byte{0x00, 0x00, 0x00, 0x10, 'f', 'r', 'e'}
	truncated := append(append([]byte{}, data...), partialHeader...)

	legacySeen, legacyN, legacyErr := func() ([]BoxInfo, int, error) {
		var seen []BoxInfo
		_, err := ReadBoxStructure(bytes.NewReader(truncated), func(h *ReadHandle) (interface{}, error) {
			seen = append(seen, h.BoxInfo)
			if budgetContainers[h.BoxInfo.Type] {
				if _, err := h.Expand(); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		return seen, len(seen), err
	}()

	seen, n, err := scanWithBudget(t, truncated, &ReadBudget{MaxReadBytes: 1 << 20})
	require.Equal(t, legacyErr, err)
	assert.False(t, errors.Is(err, ErrReadBudgetBytesExceeded))
	assert.Equal(t, legacyN, n)
	for i, bi := range seen {
		assert.Equal(t, legacySeen[i].Offset, bi.Offset)
		assert.Equal(t, legacySeen[i].Size, bi.Size)
	}

	// When the stream genuinely ends exactly on the byte-budget boundary, the
	// complete prefix is delivered with accurate offsets and the run ends
	// cleanly, instead of confusing clean EOF with budget exhaustion.
	seen, n, err = scanWithBudget(t, data, &ReadBudget{MaxReadBytes: uint64(len(data))})
	require.NoError(t, err)
	assert.Equal(t, 6, n)

	full, _, err := scanWithBudget(t, data, nil)
	require.NoError(t, err)
	for i, bi := range seen {
		assert.Equal(t, full[i].Offset, bi.Offset)
		assert.Equal(t, full[i].Size, bi.Size)
	}

	// Give enough budget to read part of the trailing partial header, then cut
	// it off: the budget must be the distinguishable failure while the six
	// complete boxes keep their exact media offsets.
	limited, nLimited, err := scanWithBudget(t, truncated, &ReadBudget{MaxReadBytes: uint64(len(data)) + 4})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReadBudgetBytesExceeded))
	assert.False(t, errors.Is(err, io.EOF))
	assert.Equal(t, 6, nLimited)
	full2, _, err := scanWithBudget(t, data, nil)
	require.NoError(t, err)
	for i, bi := range limited {
		assert.Equal(t, full2[i].Offset, bi.Offset)
		assert.Equal(t, full2[i].Size, bi.Size)
	}
}

func TestReadBudgetZeroSizeAndExtendedBoxes(t *testing.T) {
	// A size-zero box extends to EOF; it must report the resolved size/offset
	// accurately even while budgets are active.
	var stream []byte
	stream = append(stream, smallBox("free", nil)...)
	stream = append(stream, []byte{0, 0, 0, 0, 'm', 'd', 'a', 't'}...)

	seen, n, err := scanWithBudget(t, stream, &ReadBudget{MaxReadBytes: 1 << 20, MaxBoxes: 10, MaxDepth: 10})
	require.NoError(t, err)
	require.Equal(t, 2, n)
	assert.Equal(t, uint64(SmallHeaderSize), seen[1].Offset)
	assert.Equal(t, uint64(len(stream)-SmallHeaderSize), seen[1].Size)
	assert.True(t, seen[1].ExtendToEOF)

	// A 64-bit largesize box keeps a 16-byte header and accurate offsets, and
	// the extended-length bytes are charged against the byte budget.
	payload := make([]byte, 4)
	large := largeBox("free", payload)
	require.Len(t, large, LargeHeaderSize+len(payload))

	var largeSeen []BoxInfo
	_, err = ReadBoxStructureWithBudget(bytes.NewReader(large), &ReadBudget{MaxReadBytes: uint64(len(large))}, func(h *ReadHandle) (interface{}, error) {
		largeSeen = append(largeSeen, h.BoxInfo)
		return nil, nil
	})
	require.NoError(t, err)
	require.Len(t, largeSeen, 1)
	assert.Equal(t, uint64(0), largeSeen[0].Offset)
	assert.Equal(t, uint64(len(large)), largeSeen[0].Size)
	assert.Equal(t, uint64(LargeHeaderSize), largeSeen[0].HeaderSize)

	// One byte short of the large header + payload must trip the byte budget
	// before the 16-byte header can be completed, so no handler runs and the
	// box geometry is never misreported.
	_, err = ReadBoxStructureWithBudget(bytes.NewReader(large), &ReadBudget{MaxReadBytes: LargeHeaderSize - 1}, func(h *ReadHandle) (interface{}, error) {
		t.Fatal("handler must not be invoked when the header cannot be completed")
		return nil, nil
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReadBudgetBytesExceeded))
}

func TestReadBudgetUnknownType(t *testing.T) {
	data := buildBudgetSample()

	// DFS order is moov, trak, mdia, free, zzzz(unknown), root-free.
	seen, n, err := scanWithBudget(t, data, &ReadBudget{MaxDepth: 4})
	require.NoError(t, err)
	require.Equal(t, 6, n)

	unknown := seen[4]
	assert.Equal(t, StrToBoxType("zzzz"), unknown.Type)
	assert.False(t, unknown.IsSupportedType())

	full, _, err := scanWithBudget(t, data, nil)
	require.NoError(t, err)
	assert.Equal(t, full[4].Offset, unknown.Offset)
	assert.Equal(t, full[4].Size, unknown.Size)

	// Limiting boxes so the unknown one cannot be parsed yields the box-count
	// sentinel and no callback for the unknown box.
	limited, nLimited, err := scanWithBudget(t, data, &ReadBudget{MaxBoxes: 4})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReadBudgetBoxesExceeded))
	assert.Equal(t, 4, nLimited)
	for _, bi := range limited {
		assert.NotEqual(t, StrToBoxType("zzzz"), bi.Type)
	}
}

func TestReadBudgetSentinelsAreDistinguishable(t *testing.T) {
	assert.NotEqual(t, ErrReadBudgetBytesExceeded, ErrReadBudgetDepthExceeded)
	assert.NotEqual(t, ErrReadBudgetBytesExceeded, ErrReadBudgetBoxesExceeded)
	assert.NotEqual(t, ErrReadBudgetDepthExceeded, ErrReadBudgetBoxesExceeded)

	data := buildBudgetSample()

	_, _, err := scanWithBudget(t, data, &ReadBudget{MaxReadBytes: 1})
	assert.True(t, errors.Is(err, ErrReadBudgetBytesExceeded))

	_, _, err = scanWithBudget(t, data, &ReadBudget{MaxDepth: 1})
	assert.True(t, errors.Is(err, ErrReadBudgetDepthExceeded))

	_, _, err = scanWithBudget(t, data, &ReadBudget{MaxBoxes: 1})
	assert.True(t, errors.Is(err, ErrReadBudgetBoxesExceeded))
}

func TestReadBudgetWithSampleMP4(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	// The sample file contains 57 boxes (see TestReadBoxStructure).
	var callbacks int
	handler := func(h *ReadHandle) (interface{}, error) {
		callbacks++
		if h.BoxInfo.IsSupportedType() {
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	_, err = ReadBoxStructureWithBudget(f, &ReadBudget{MaxBoxes: 57}, handler)
	require.NoError(t, err)
	assert.Equal(t, 57, callbacks)

	_, err = f.Seek(0, io.SeekStart)
	require.NoError(t, err)
	callbacks = 0
	_, err = ReadBoxStructureWithBudget(f, &ReadBudget{MaxBoxes: 10}, handler)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReadBudgetBoxesExceeded))
	assert.Equal(t, 10, callbacks)

	// 32 bytes cover the 8-byte ftyp header and its 24-byte payload; the next
	// header read must trip the byte budget after exactly one callback.
	_, err = f.Seek(0, io.SeekStart)
	require.NoError(t, err)
	callbacks = 0
	_, err = ReadBoxStructureWithBudget(f, &ReadBudget{MaxReadBytes: 32}, handler)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReadBudgetBytesExceeded))
	assert.Equal(t, 1, callbacks)
}
