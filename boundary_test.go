package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type boundaryReadSeeker struct {
	r   io.ReadSeeker
	pos int64
}

func (r *boundaryReadSeeker) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.pos += int64(n)
	return n, err
}

func (r *boundaryReadSeeker) Seek(offset int64, whence int) (int64, error) {
	pos, err := r.r.Seek(offset, whence)
	r.pos = pos
	return pos, err
}

func be32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func be64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func fullBox(version byte, flags uint32) []byte {
	return []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
}

func TestTimestampVersionBoundaryRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		box        IImmutableBox
		newDst     func() IBox
		payload    []byte
		wantTime   uint64
		wantGetter func(IBox) uint64
	}{
		{
			name: "mdhd v0 zero and uint32 max duration",
			box: &Mdhd{
				FullBox:    FullBox{Version: 0},
				Timescale:  math.MaxUint32,
				DurationV0: math.MaxUint32,
				Language:   [3]byte{'u' - 0x60, 'n' - 0x60, 'd' - 0x60},
			},
			newDst:     func() IBox { return &Mdhd{} },
			payload:    bytes.Join([][]byte{fullBox(0, 0), be32(0), be32(0), be32(math.MaxUint32), be32(math.MaxUint32), {0x55, 0xc4, 0, 0}}, nil),
			wantTime:   math.MaxUint32,
			wantGetter: func(box IBox) uint64 { return box.(*Mdhd).GetDuration() },
		},
		{
			name: "mdhd v1 zero and uint64 max time fields",
			box: &Mdhd{
				FullBox:            FullBox{Version: 1},
				CreationTimeV1:     math.MaxUint64,
				ModificationTimeV1: math.MaxUint64,
				Timescale:          math.MaxUint32,
				DurationV1:         math.MaxUint64,
				Language:           [3]byte{'u' - 0x60, 'n' - 0x60, 'd' - 0x60},
			},
			newDst: func() IBox { return &Mdhd{} },
			payload: bytes.Join([][]byte{
				fullBox(1, 0), be64(math.MaxUint64), be64(math.MaxUint64), be32(math.MaxUint32),
				be64(math.MaxUint64), {0x55, 0xc4, 0, 0},
			}, nil),
			wantTime:   math.MaxUint64,
			wantGetter: func(box IBox) uint64 { return box.(*Mdhd).GetDuration() },
		},
		{
			name: "tfdt v0 zero",
			box: &Tfdt{
				FullBox: FullBox{Version: 0},
			},
			newDst:     func() IBox { return &Tfdt{} },
			payload:    bytes.Join([][]byte{fullBox(0, 0), be32(0)}, nil),
			wantTime:   0,
			wantGetter: func(box IBox) uint64 { return box.(*Tfdt).GetBaseMediaDecodeTime() },
		},
		{
			name: "tfdt v0 uint32 max",
			box: &Tfdt{
				FullBox:               FullBox{Version: 0},
				BaseMediaDecodeTimeV0: math.MaxUint32,
			},
			newDst:     func() IBox { return &Tfdt{} },
			payload:    bytes.Join([][]byte{fullBox(0, 0), be32(math.MaxUint32)}, nil),
			wantTime:   math.MaxUint32,
			wantGetter: func(box IBox) uint64 { return box.(*Tfdt).GetBaseMediaDecodeTime() },
		},
		{
			name: "tfdt v1 zero",
			box: &Tfdt{
				FullBox: FullBox{Version: 1},
			},
			newDst:     func() IBox { return &Tfdt{} },
			payload:    bytes.Join([][]byte{fullBox(1, 0), be64(0)}, nil),
			wantTime:   0,
			wantGetter: func(box IBox) uint64 { return box.(*Tfdt).GetBaseMediaDecodeTime() },
		},
		{
			name: "tfdt v1 uint64 max",
			box: &Tfdt{
				FullBox:               FullBox{Version: 1},
				BaseMediaDecodeTimeV1: math.MaxUint64,
			},
			newDst:     func() IBox { return &Tfdt{} },
			payload:    bytes.Join([][]byte{fullBox(1, 0), be64(math.MaxUint64)}, nil),
			wantTime:   math.MaxUint64,
			wantGetter: func(box IBox) uint64 { return box.(*Tfdt).GetBaseMediaDecodeTime() },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var encoded bytes.Buffer
			n, err := Marshal(&encoded, tc.box, Context{})
			require.NoError(t, err)
			require.Equal(t, uint64(len(tc.payload)), n)
			assert.Equal(t, tc.payload, encoded.Bytes())

			r := &boundaryReadSeeker{r: bytes.NewReader(tc.payload)}
			dst := tc.newDst()
			n, err = Unmarshal(r, uint64(len(tc.payload)), dst, Context{})
			require.NoError(t, err)
			assert.Equal(t, uint64(len(tc.payload)), n)
			assert.Equal(t, int64(len(tc.payload)), r.pos)
			assert.Equal(t, tc.box, dst)
			assert.Equal(t, tc.wantTime, tc.wantGetter(dst))

			for cut := 0; cut < len(tc.payload); cut++ {
				r := &boundaryReadSeeker{r: bytes.NewReader(tc.payload[:cut])}
				n, err := Unmarshal(r, uint64(len(tc.payload)), tc.newDst(), Context{})
				require.Error(t, err, "cut=%d", cut)
				assert.True(t, errors.Is(err, io.EOF), "cut=%d", cut)
				assert.Equal(t, uint64(0), n, "cut=%d", cut)
				assert.Equal(t, int64(cut), r.pos, "cut=%d", cut)
			}
		})
	}
}

func TestUnsupportedTimestampVersionResetsAndKeepsPayloadPosition(t *testing.T) {
	payloads := map[string][]byte{
		"mdhd": append(fullBox(2, 0), make([]byte, 28)...),
		"tfdt": append(fullBox(2, 0), make([]byte, 8)...),
	}

	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			var dst IBox
			if name == "mdhd" {
				dst = &Mdhd{}
			} else {
				dst = &Tfdt{}
			}
			r := &boundaryReadSeeker{r: bytes.NewReader(payload)}
			n, err := Unmarshal(r, uint64(len(payload)), dst, Context{})
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrUnsupportedBoxVersion))
			assert.Equal(t, uint64(0), n)
			assert.Equal(t, int64(0), r.pos)
		})
	}
}

func TestTrunDeclaredLengthFlagsAndTruncation(t *testing.T) {
	var minInt32 int32 = math.MinInt32
	tests := []struct {
		name       string
		trun       *Trun
		payload    []byte
		newDst     func() IBox
		wantOffset int64
	}{
		{
			name: "zero entries with unknown flag ignored",
			trun: &Trun{
				FullBox: FullBox{Flags: [3]byte{0x80, 0x00, 0x00}},
				Entries: []TrunEntry{},
			},
			payload:    append(fullBox(0, 0x800000), be32(0)...),
			newDst:     func() IBox { return &Trun{} },
			wantOffset: 8,
		},
		{
			name: "single entry with maximum field values and unknown flag",
			trun: &Trun{
				FullBox:          FullBox{Flags: [3]byte{0x00, 0x0f, 0x05}},
				DataOffset:       math.MaxInt32,
				FirstSampleFlags: math.MaxUint32,
				SampleCount:      1,
				Entries: []TrunEntry{{
					SampleDuration:                math.MaxUint32,
					SampleSize:                    math.MaxUint32,
					SampleFlags:                   math.MaxUint32,
					SampleCompositionTimeOffsetV0: math.MaxUint32,
				}},
			},
			newDst: func() IBox { return &Trun{} },
			payload: bytes.Join([][]byte{
				fullBox(0, 0x000f05), be32(1), be32(math.MaxInt32), be32(math.MaxUint32),
				be32(math.MaxUint32), be32(math.MaxUint32), be32(math.MaxUint32), be32(math.MaxUint32),
			}, nil),
			wantOffset: 32,
		},
		{
			name: "v1 signed composition offset min and max",
			trun: &Trun{
				FullBox:     FullBox{Version: 1, Flags: [3]byte{0x00, 0x08, 0x00}},
				SampleCount: 2,
				Entries: []TrunEntry{
					{SampleCompositionTimeOffsetV1: math.MaxInt32},
					{SampleCompositionTimeOffsetV1: math.MinInt32},
				},
			},
			newDst: func() IBox { return &Trun{} },
			payload: bytes.Join([][]byte{
				fullBox(1, 0x000800), be32(2), be32(math.MaxInt32), be32(uint32(minInt32)),
			}, nil),
			wantOffset: 16,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var encoded bytes.Buffer
			n, err := Marshal(&encoded, tc.trun, Context{})
			require.NoError(t, err)
			assert.Equal(t, uint64(len(tc.payload)), n)
			assert.Equal(t, tc.payload, encoded.Bytes())

			r := &boundaryReadSeeker{r: bytes.NewReader(tc.payload)}
			dst := tc.newDst()
			n, err = Unmarshal(r, uint64(len(tc.payload)), dst, Context{})
			require.NoError(t, err)
			assert.Equal(t, uint64(len(tc.payload)), n)
			assert.Equal(t, tc.wantOffset, r.pos)
			assert.Equal(t, tc.trun, dst)

			for cut := 0; cut < len(tc.payload); cut++ {
				r := &boundaryReadSeeker{r: bytes.NewReader(tc.payload[:cut])}
				n, err := Unmarshal(r, uint64(len(tc.payload)), tc.newDst(), Context{})
				require.Error(t, err, "cut=%d", cut)
				assert.True(t, errors.Is(err, io.EOF), "cut=%d", cut)
				assert.Equal(t, uint64(0), n, "cut=%d", cut)
				assert.Equal(t, int64(cut), r.pos, "cut=%d", cut)
			}
		})
	}
}

func TestTrunVersion1SignBitsRoundTrip(t *testing.T) {
	var negativeOneValue int32 = -1
	negativeOne := uint32(negativeOneValue)
	trun := &Trun{
		FullBox:     FullBox{Version: 1, Flags: [3]byte{0x00, 0x08, 0x00}},
		SampleCount: 2,
		Entries: []TrunEntry{
			{SampleCompositionTimeOffsetV1: 0},
			{SampleCompositionTimeOffsetV1: -1},
		},
	}
	var encoded bytes.Buffer
	n, err := Marshal(&encoded, trun, Context{})
	require.NoError(t, err)
	want := bytes.Join([][]byte{
		fullBox(1, 0x000800), be32(2), be32(0), be32(negativeOne),
	}, nil)
	assert.Equal(t, uint64(len(want)), n)
	assert.Equal(t, want, encoded.Bytes())

	dst := &Trun{}
	n, err = Unmarshal(bytes.NewReader(want), uint64(len(want)), dst, Context{})
	require.NoError(t, err)
	assert.Equal(t, uint64(len(want)), n)
	assert.Equal(t, trun, dst)
	assert.Equal(t, int64(0), dst.GetSampleCompositionTimeOffset(0))
	assert.Equal(t, int64(-1), dst.GetSampleCompositionTimeOffset(1))
}

func TestTrunDeclaredCountIsActualConsumption(t *testing.T) {
	// One declared entry requires exactly one entry; an extra table byte is
	// unused payload rather than being silently consumed as another sample.
	payload := bytes.Join([][]byte{fullBox(0, 0x000300), be32(1), be32(7), be32(11), {0xaa}}, nil)
	r := &boundaryReadSeeker{r: bytes.NewReader(payload)}
	dst := &Trun{}
	n, err := Unmarshal(r, uint64(len(payload)), dst, Context{})
	require.NoError(t, err)
	assert.Equal(t, uint64(16), n)
	assert.Equal(t, int64(16), r.pos)
	require.Len(t, dst.Entries, 1)
	assert.Equal(t, uint32(7), dst.Entries[0].SampleDuration)
	assert.Equal(t, uint32(11), dst.Entries[0].SampleSize)

	short := payload[:15]
	n, err = Unmarshal(&boundaryReadSeeker{r: bytes.NewReader(short)}, uint64(len(payload)), &Trun{}, Context{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, io.EOF))
	assert.Equal(t, uint64(0), n)
}

func TestTrunUnknownFlagDoesNotChangeDeclaredFieldWidth(t *testing.T) {
	known := bytes.Join([][]byte{
		fullBox(0, 0x000300), be32(1), be32(7), be32(11),
	}, nil)
	withUnknownFlag := bytes.Join([][]byte{
		fullBox(0, 0x800300), be32(1), be32(7), be32(11),
	}, nil)

	knownDst := &Trun{}
	n, err := Unmarshal(bytes.NewReader(known), uint64(len(known)), knownDst, Context{})
	require.NoError(t, err)
	unknownDst := &Trun{}
	n2, err := Unmarshal(bytes.NewReader(withUnknownFlag), uint64(len(withUnknownFlag)), unknownDst, Context{})
	require.NoError(t, err)

	assert.Equal(t, n, n2)
	assert.Equal(t, knownDst.SampleCount, unknownDst.SampleCount)
	assert.Equal(t, knownDst.Entries, unknownDst.Entries)
	assert.True(t, unknownDst.CheckFlag(0x800000))
}

func TestTrunMaximumDeclaredCountHonorsPayloadBoundary(t *testing.T) {
	t.Run("remaining size is not a whole entry", func(t *testing.T) {
		payload := bytes.Join([][]byte{
			fullBox(0, 0x000300), be32(math.MaxUint32), be32(1), be32(2), be32(3),
		}, nil)

		r := &boundaryReadSeeker{r: bytes.NewReader(payload)}
		dst := &Trun{}
		n, err := Unmarshal(r, uint64(len(payload)), dst, Context{})
		require.EqualError(t, err, "invalid alignment")
		assert.Equal(t, uint64(0), n)
		assert.Equal(t, int64(8), r.pos)
		assert.Equal(t, uint32(math.MaxUint32), dst.SampleCount)
	})

	t.Run("one-entry declaration is truncated after first field", func(t *testing.T) {
		payload := bytes.Join([][]byte{
			fullBox(0, 0x000300), be32(1), be32(7),
		}, nil)

		r := &boundaryReadSeeker{r: bytes.NewReader(payload)}
		dst := &Trun{}
		n, err := Unmarshal(r, uint64(20), dst, Context{})
		require.Error(t, err)
		assert.True(t, errors.Is(err, io.EOF))
		assert.Equal(t, uint64(0), n)
		assert.Equal(t, int64(len(payload)), r.pos)
		assert.Equal(t, uint32(1), dst.SampleCount)
	})
}

var boundaryVarintBoxOnce sync.Once

type boundaryVarintBox struct {
	Box
	mockBox
	Value uint64 `mp4:"0,varint"`
}

func TestDescriptorVarintBoundaryConsumption(t *testing.T) {
	boxType := StrToBoxType("bvar")
	mb := mockBox{Type: boxType}
	boundaryVarintBoxOnce.Do(func() {
		AddBoxDef(&boundaryVarintBox{mockBox: mb}, 0)
	})

	tests := []struct {
		name        string
		value       uint64
		encoded     []byte
		written     []byte
		declaredLen uint64
		wantN       uint64
		wantPos     int64
	}{
		{name: "zero", value: 0, encoded: []byte{0x80, 0x80, 0x80, 0x00}, written: []byte{0x80, 0x80, 0x80, 0x00}, declaredLen: 4, wantN: 4, wantPos: 4},
		{name: "one byte max", value: 0x7f, encoded: []byte{0x80, 0x80, 0x80, 0x7f}, written: []byte{0x80, 0x80, 0x80, 0x7f}, declaredLen: 4, wantN: 4, wantPos: 4},
		{name: "two byte min", value: 0x80, encoded: []byte{0x80, 0x80, 0x81, 0x00}, written: []byte{0x80, 0x80, 0x81, 0x00}, declaredLen: 4, wantN: 4, wantPos: 4},
		{name: "three byte min", value: 0x4000, encoded: []byte{0x80, 0x81, 0x80, 0x00}, written: []byte{0x80, 0x81, 0x80, 0x00}, declaredLen: 4, wantN: 4, wantPos: 4},
		{name: "four byte min", value: 0x200000, encoded: []byte{0x81, 0x80, 0x80, 0x00}, written: []byte{0x81, 0x80, 0x80, 0x00}, declaredLen: 4, wantN: 4, wantPos: 4},
		{name: "28 bit max", value: 0xfffffff, encoded: []byte{0xff, 0xff, 0xff, 0x7f}, written: []byte{0xff, 0xff, 0xff, 0x7f}, declaredLen: 4, wantN: 4, wantPos: 4},
		{name: "29 bit min", value: 0x10000000, encoded: []byte{0x80, 0x80, 0x80, 0x80, 0x01}, written: []byte{0x80, 0x80, 0x80, 0x80, 0x01}, declaredLen: 5, wantN: 5, wantPos: 5},
		{name: "uint32 max", value: math.MaxUint32, encoded: []byte{0xff, 0xff, 0xff, 0xff, 0x0f}, written: []byte{0xff, 0xff, 0xff, 0xff, 0x0f}, declaredLen: 5, wantN: 5, wantPos: 5},
		{name: "uint64 max", value: math.MaxUint64, encoded: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, written: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, declaredLen: 10, wantN: 10, wantPos: 10},
		{name: "trailing byte remains unconsumed", value: 0x7f, encoded: []byte{0x80, 0x80, 0x80, 0x7f, 0xaa}, written: []byte{0x80, 0x80, 0x80, 0x7f}, declaredLen: 5, wantN: 4, wantPos: 4},
		{name: "shorter accepted encoding", value: 0, encoded: []byte{0x00}, written: []byte{0x80, 0x80, 0x80, 0x00}, declaredLen: 1, wantN: 1, wantPos: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var encoded bytes.Buffer
			n, err := Marshal(&encoded, &boundaryVarintBox{mockBox: mb, Value: tc.value}, Context{})
			require.NoError(t, err)
			assert.Equal(t, uint64(len(tc.written)), n)
			assert.Equal(t, tc.written, encoded.Bytes())

			r := &boundaryReadSeeker{r: bytes.NewReader(tc.encoded)}
			dst := &boundaryVarintBox{mockBox: mb}
			n, err = Unmarshal(r, tc.declaredLen, dst, Context{})
			require.NoError(t, err)
			assert.Equal(t, tc.wantN, n)
			assert.Equal(t, tc.wantPos, r.pos)
			assert.Equal(t, tc.value, dst.Value)
		})
	}
}

func TestDescriptorVarintTruncationAndOverflowSeeds(t *testing.T) {
	boxType := StrToBoxType("bvar")
	mb := mockBox{Type: boxType}
	boundaryVarintBoxOnce.Do(func() {
		AddBoxDef(&boundaryVarintBox{mockBox: mb}, 0)
	})

	tests := []struct {
		name         string
		input        []byte
		wantPos      int64
		wantN        uint64
		wantEOF      bool
		wantOverflow bool
	}{
		{name: "truncated after first continuation", input: []byte{0x80}, wantPos: 1, wantEOF: true},
		{name: "truncated after three continuations", input: []byte{0xff, 0xff, 0xff}, wantPos: 3, wantEOF: true},
		{name: "truncated extended max uint32", input: []byte{0xff, 0xff, 0xff, 0xff}, wantPos: 4, wantEOF: true},
		{name: "ten byte max uint64", input: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, wantPos: 10, wantN: 10},
		{name: "eleven bytes beyond uint64", input: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, wantPos: 10, wantOverflow: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &boundaryReadSeeker{r: bytes.NewReader(tc.input)}
			dst := &boundaryVarintBox{mockBox: mb}
			n, err := Unmarshal(r, uint64(len(tc.input)), dst, Context{})
			assert.Equal(t, tc.wantN, n)
			assert.Equal(t, tc.wantPos, r.pos)
			if tc.wantEOF {
				require.Error(t, err)
				assert.True(t, errors.Is(err, io.EOF))
			} else if tc.wantOverflow {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrVarintOverflow))
			} else {
				require.NoError(t, err)
				assert.Equal(t, uint64(math.MaxUint64), dst.Value)
			}
		})
	}
}

func FuzzDescriptorVarint(f *testing.F) {
	boxType := StrToBoxType("bvar")
	mb := mockBox{Type: boxType}
	boundaryVarintBoxOnce.Do(func() {
		AddBoxDef(&boundaryVarintBox{mockBox: mb}, 0)
	})

	seeds := [][]byte{
		{0x00},
		{0x7f},
		{0x80, 0x80, 0x80, 0x00},
		{0x80, 0x80, 0x80, 0x04},
		{0xff, 0xff, 0xff},
		{0x80, 0x80, 0x80, 0x80, 0x01},
		{0xff, 0xff, 0xff, 0xff},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
		{0x00, 0x80},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input []byte) {
		r := &boundaryReadSeeker{r: bytes.NewReader(input)}
		dst := &boundaryVarintBox{mockBox: mb}
		_, err := Unmarshal(r, uint64(len(input)), dst, Context{})
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, ErrVarintOverflow) {
			t.Fatalf("unexpected error: %v", err)
		}
		assert.LessOrEqual(t, r.pos, int64(len(input)))
	})
}

func TestProbeResultShapeDiffersForProgressiveAndFragmentedContainers(t *testing.T) {
	progressive, err := os.Open("testdata/sample.mp4")
	require.NoError(t, err)
	defer progressive.Close()

	progressiveInfo, err := Probe(progressive)
	require.NoError(t, err)
	assert.Empty(t, progressiveInfo.Segments)
	for _, track := range progressiveInfo.Tracks {
		assert.NotEmpty(t, track.Samples)
		assert.NotEmpty(t, track.Chunks)
	}

	fragmented, err := os.Open("testdata/sample_fragmented.mp4")
	require.NoError(t, err)
	defer fragmented.Close()

	fragmentedInfo, err := Probe(fragmented)
	require.NoError(t, err)
	assert.NotEmpty(t, fragmentedInfo.Segments)
	for _, track := range fragmentedInfo.Tracks {
		assert.Empty(t, track.Samples)
		assert.Empty(t, track.Chunks)
	}

	assert.Len(t, fragmentedInfo.Tracks, len(progressiveInfo.Tracks))
	for i := range progressiveInfo.Tracks {
		assert.Equal(t, progressiveInfo.Tracks[i].TrackID, fragmentedInfo.Tracks[i].TrackID)
		assert.Greater(t, len(progressiveInfo.Tracks[i].Samples), 0)
		assert.Greater(t, len(progressiveInfo.Tracks[i].Chunks), 0)
		assert.Equal(t, 0, len(fragmentedInfo.Tracks[i].Samples))
		assert.Equal(t, 0, len(fragmentedInfo.Tracks[i].Chunks))
	}
	assert.Greater(t, len(fragmentedInfo.Segments), 0)
	assert.Equal(t, progressiveInfo.Timescale, fragmentedInfo.Timescale)
}

func TestEsdsZeroDescriptorsAndDeclaredDataLength(t *testing.T) {
	zero := &Esds{FullBox: FullBox{}}
	var encoded bytes.Buffer
	n, err := Marshal(&encoded, zero, Context{})
	require.NoError(t, err)
	assert.Equal(t, uint64(4), n)
	assert.Equal(t, fullBox(0, 0), encoded.Bytes())

	zeroDst := &Esds{}
	n, err = Unmarshal(bytes.NewReader(encoded.Bytes()), uint64(encoded.Len()), zeroDst, Context{})
	require.NoError(t, err)
	assert.Equal(t, uint64(4), n)
	assert.Equal(t, &Esds{FullBox: FullBox{}, Descriptors: []Descriptor{}}, zeroDst)

	for cut := 0; cut < 4; cut++ {
		r := &boundaryReadSeeker{r: bytes.NewReader(encoded.Bytes()[:cut])}
		n, err := Unmarshal(r, 4, &Esds{}, Context{})
		require.Error(t, err, "cut=%d", cut)
		assert.True(t, errors.Is(err, io.EOF), "cut=%d", cut)
		assert.Equal(t, uint64(0), n, "cut=%d", cut)
		assert.Equal(t, int64(cut), r.pos, "cut=%d", cut)
	}

	maxSize := &Esds{
		FullBox: FullBox{},
		Descriptors: []Descriptor{{
			Tag:  SLConfigDescrTag,
			Size: 4,
			Data: []byte{0x01, 0x02, 0x03, 0x04},
		}},
	}
	encoded.Reset()
	n, err = Marshal(&encoded, maxSize, Context{})
	require.NoError(t, err)
	want := bytes.Join([][]byte{
		fullBox(0, 0),
		{SLConfigDescrTag},
		{0x80, 0x80, 0x80, 0x04},
		{0x01, 0x02, 0x03, 0x04},
	}, nil)
	assert.Equal(t, uint64(len(want)), n)
	assert.Equal(t, want, encoded.Bytes())

	maxDst := &Esds{}
	n, err = Unmarshal(bytes.NewReader(want), uint64(len(want)), maxDst, Context{})
	require.NoError(t, err)
	assert.Equal(t, uint64(len(want)), n)
	assert.Equal(t, maxSize, maxDst)

	for cut := 4; cut < len(want); cut++ {
		r := &boundaryReadSeeker{r: bytes.NewReader(want[:cut])}
		n, err := Unmarshal(r, uint64(len(want)), &Esds{}, Context{})
		require.Error(t, err, "cut=%d", cut)
		if cut < 9 {
			assert.True(t, errors.Is(err, io.EOF), "cut=%d err=%v", cut, err)
		} else {
			assert.EqualError(t, err, "not enough bits", "cut=%d", cut)
		}
		assert.Equal(t, uint64(0), n, "cut=%d", cut)
		assert.Equal(t, int64(cut), r.pos, "cut=%d", cut)
	}
}
