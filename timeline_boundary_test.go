package mp4

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins the library's behavior at the edges of the wire-format
// integer ranges and on deliberately malformed containers. Each assertion
// states whether go-mp4 itself rejects the input or preserves (possibly
// surprising) values verbatim. Nothing here relies on an external player to
// forgive a malformed field: if Probe accepts something, the test records the
// exact value it yields so a future change cannot silently relax or tighten it.

// patchPayloadByte overwrites one byte inside a box payload.
func patchPayloadByte(t *testing.T, bin []byte, path BoxPath, payloadOffset uint64, value byte) []byte {
	t.Helper()
	r := bytes.NewReader(bin)
	bis, err := ExtractBoxes(r, nil, []BoxPath{path})
	require.NoError(t, err)
	require.NotEmpty(t, bis)
	idx := bis[0].Offset + bis[0].HeaderSize + payloadOffset
	require.Less(t, idx, uint64(len(bin)))
	out := append([]byte(nil), bin...)
	out[idx] = value
	return out
}

// TestBoundaryVersionZeroMaxUint32 verifies that the 32-bit fields of version-0
// boxes accept their maximum representable value without loss.
func TestBoundaryVersionZeroMaxUint32(t *testing.T) {
	tracks := []*timelineTrackSpec{
		{
			order: 0, trackID: 1, timescale: 1000, duration: math.MaxUint32, version: 0,
			samples: []timelineSampleSpec{{size: 1, delta: math.MaxUint32}},
		},
	}
	bin := newTimelineBuilder(t).build(1000, math.MaxUint32, 0, tracks)
	info := probeBytes(t, bin)

	assert.Equal(t, uint64(math.MaxUint32), info.Duration)
	require.Len(t, info.Tracks, 1)
	track := info.Tracks[0]
	assert.Equal(t, uint64(math.MaxUint32), track.Duration)
	require.Len(t, track.Samples, 1)
	assert.Equal(t, uint32(math.MaxUint32), track.Samples[0].TimeDelta)

	// The raw mvhd version-0 decode must show the same value through GetDuration.
	var mvhd Mvhd
	unmarshalBoxes(t, bin, BoxPath{BoxTypeMoov(), BoxTypeMvhd()}, &mvhd)
	assert.Equal(t, uint8(0), mvhd.GetVersion())
	assert.Equal(t, uint64(math.MaxUint32), mvhd.GetDuration())
	assert.Equal(t, uint32(math.MaxUint32), mvhd.DurationV0)
}

// TestBoundaryVersionOneMaxUint64 verifies that version-1 boxes carry the full
// 64-bit duration range, including the maximum value.
func TestBoundaryVersionOneMaxUint64(t *testing.T) {
	tracks := []*timelineTrackSpec{
		{
			order: 0, trackID: 1, timescale: 1000, duration: math.MaxUint64, version: 1,
			samples: []timelineSampleSpec{{size: 1, delta: 1}},
		},
	}
	bin := newTimelineBuilder(t).build(1000, math.MaxUint64, 1, tracks)
	info := probeBytes(t, bin)

	assert.Equal(t, uint64(math.MaxUint64), info.Duration)
	assert.Equal(t, uint64(math.MaxUint64), info.Tracks[0].Duration)

	var mvhd Mvhd
	unmarshalBoxes(t, bin, BoxPath{BoxTypeMoov(), BoxTypeMvhd()}, &mvhd)
	assert.Equal(t, uint8(1), mvhd.GetVersion())
	assert.Equal(t, uint64(math.MaxUint64), mvhd.DurationV1)
	assert.Equal(t, uint64(math.MaxUint64), mvhd.GetDuration())
}

// TestBoundaryTfdtVersionRanges checks both tfdt encodings at their range
// boundary: v0 stores up to MaxUint32, v1 stores values beyond that range.
func TestBoundaryTfdtVersionRanges(t *testing.T) {
	bin := buildFragmentedFile(t, newTimelineBuilder(t), []*timelineFragmentSpec{
		{
			trackID: 1, tfdtVersion: 0, baseMediaDecodeTime: math.MaxUint32, trunVersion: 0,
			entries: []trunEntrySpec{{duration: 1, size: 1}},
		},
		{
			trackID: 1, tfdtVersion: 1, baseMediaDecodeTime: uint64(math.MaxUint32) + 1, trunVersion: 0,
			entries: []trunEntrySpec{{duration: 1, size: 1}},
		},
	})
	info := probeBytes(t, bin)
	require.Len(t, info.Segments, 2)
	assert.Equal(t, uint64(math.MaxUint32), info.Segments[0].BaseMediaDecodeTime)
	assert.Equal(t, uint64(math.MaxUint32)+1, info.Segments[1].BaseMediaDecodeTime)

	r := bytes.NewReader(bin)
	bis, err := ExtractBoxes(r, nil, []BoxPath{{BoxTypeMoof(), BoxTypeTraf(), BoxTypeTfdt()}})
	require.NoError(t, err)
	require.Len(t, bis, 2)
	for i, wantVersion := range []uint8{0, 1} {
		var tfdt Tfdt
		_, err := bis[i].SeekToPayload(r)
		require.NoError(t, err)
		_, err = Unmarshal(r, bis[i].Size-bis[i].HeaderSize, &tfdt, bis[i].Context)
		require.NoError(t, err)
		assert.Equalf(t, wantVersion, tfdt.GetVersion(), "tfdt %d version", i)
		assert.Equalf(t, info.Segments[i].BaseMediaDecodeTime, tfdt.GetBaseMediaDecodeTime(), "tfdt %d time", i)
	}
}

// TestBoundaryCttsSignedRange pins the signed composition-offset range. The
// version-1 signed encoding preserves both int32 extremes; the version-0
// unsigned encoding can represent the same high bit pattern but the library
// then reads it as a large positive value rather than a negative one.
func TestBoundaryCttsSignedRange(t *testing.T) {
	spec := &timelineTrackSpec{
		order: 0, trackID: 4, timescale: 90000, duration: 60000, version: 1,
		cttsVersion: 1, hasCtts: true,
		samples: []timelineSampleSpec{
			{size: 10, delta: 30000, compositionOffset: math.MaxInt32},
			{size: 10, delta: 30000, compositionOffset: math.MinInt32},
		},
	}
	bin := newTimelineBuilder(t).build(1000, 1000, 1, []*timelineTrackSpec{spec})
	info := probeBytes(t, bin)
	samples := info.Tracks[0].Samples
	assert.Equal(t, int64(math.MaxInt32), samples[0].CompositionTimeOffset)
	assert.Equal(t, int64(math.MinInt32), samples[1].CompositionTimeOffset)

	// A version-0 ctts whose offset has its high bit set is a legal uint32;
	// Probe preserves it as a positive value and does not reinterpret the sign.
	v0spec := &timelineTrackSpec{
		order: 0, trackID: 5, timescale: 90000, duration: 30000, version: 0,
		cttsVersion: 0, hasCtts: true,
		samples: []timelineSampleSpec{{size: 10, delta: 30000, compositionOffset: 1}},
	}
	v0bin := newTimelineBuilder(t).build(1000, 1000, 0, []*timelineTrackSpec{v0spec})
	// ctts payload layout: version/flags (4) + entry_count (4) + first entry's
	// sample_count (4) + sample_offset (4). Set the offset to 0x80000000.
	cttsPath := BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCtts()}
	v0bin = writeUint32Payload(t, v0bin, cttsPath, 12, 0x80000000)
	v0info := probeBytes(t, v0bin)
	require.Len(t, v0info.Tracks[0].Samples, 1)
	// 0x80000000 stays an unsigned 2147483648: the version-0 codec has no sign
	// bit and the library must not invent one.
	assert.Equal(t, int64(0x80000000), v0info.Tracks[0].Samples[0].CompositionTimeOffset)

	var ctts Ctts
	unmarshalBoxes(t, v0bin, cttsPath, &ctts)
	assert.Equal(t, uint8(0), ctts.GetVersion())
	assert.Equal(t, int64(0x80000000), ctts.GetSampleOffset(0))
}

// TestBoundaryElstSignedRange checks elst media-time extremes in both versions,
// including the requirement that a negative media time use the 64-bit version.
func TestBoundaryElstSignedRange(t *testing.T) {
	// Version 1 holds MinInt64/MaxInt64 media times and MaxUint64 duration.
	specV1 := &timelineTrackSpec{
		order: 0, trackID: 6, timescale: 48000, duration: 48000, version: 1,
		samples: []timelineSampleSpec{{size: 1, delta: 48000}},
		edits: []elstEntrySpec{
			{version: 1, segmentDuration: math.MaxUint64, mediaTime: math.MinInt64, mediaRate: 1},
			{version: 1, segmentDuration: 1, mediaTime: math.MaxInt64, mediaRate: 1},
		},
	}
	binV1 := newTimelineBuilder(t).build(48000, 1, 1, []*timelineTrackSpec{specV1})
	infoV1 := probeBytes(t, binV1)
	edits := infoV1.Tracks[0].EditList
	require.Len(t, edits, 2)
	assert.Equal(t, int64(math.MinInt64), edits[0].MediaTime)
	assert.Equal(t, uint64(math.MaxUint64), edits[0].SegmentDuration)
	assert.Equal(t, int64(math.MaxInt64), edits[1].MediaTime)

	var elstV1 Elst
	unmarshalBoxes(t, binV1, BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeEdts(), BoxTypeElst()}, &elstV1)
	assert.Equal(t, uint8(1), elstV1.GetVersion())
	assert.Equal(t, int64(math.MinInt64), elstV1.GetMediaTime(0))
	assert.Equal(t, uint64(math.MaxUint64), elstV1.GetSegmentDuration(0))

	// Version 0 media time is int32: its extremes must round-trip too.
	specV0 := &timelineTrackSpec{
		order: 0, trackID: 7, timescale: 48000, duration: 48000, version: 0,
		samples: []timelineSampleSpec{{size: 1, delta: 48000}},
		edits: []elstEntrySpec{
			{version: 0, segmentDuration: math.MaxUint32, mediaTime: math.MinInt32, mediaRate: 1},
		},
	}
	binV0 := newTimelineBuilder(t).build(48000, math.MaxUint32, 0, []*timelineTrackSpec{specV0})
	infoV0 := probeBytes(t, binV0)
	require.Len(t, infoV0.Tracks[0].EditList, 1)
	assert.Equal(t, int64(math.MinInt32), infoV0.Tracks[0].EditList[0].MediaTime)
	assert.Equal(t, uint64(math.MaxUint32), infoV0.Tracks[0].EditList[0].SegmentDuration)
}

// TestOverflowTrunDurationWraps documents that Probe accumulates trun sample
// durations in a uint32 without an overflow guard. The library preserves the
// wrapped result rather than rejecting the fragment; the raw trun entries
// remain available to a caller that needs the unwrapped values.
func TestOverflowTrunDurationWraps(t *testing.T) {
	bin := buildFragmentedFile(t, newTimelineBuilder(t), []*timelineFragmentSpec{
		{
			trackID: 1, tfdtVersion: 0, baseMediaDecodeTime: 0, trunVersion: 0,
			entries: []trunEntrySpec{
				{duration: math.MaxUint32, size: 1},
				{duration: 1, size: 1},
			},
		},
	})
	info := probeBytes(t, bin)
	require.Len(t, info.Segments, 1)
	// MaxUint32 + 1 wraps to zero in uint32 arithmetic.
	assert.Equal(t, uint32(0), info.Segments[0].Duration)

	// The source entries were not corrupted: raw decode still exposes both
	// durations, so the wrap is confined to the aggregate field.
	var trun Trun
	unmarshalBoxes(t, bin, BoxPath{BoxTypeMoof(), BoxTypeTraf(), BoxTypeTrun()}, &trun)
	require.Len(t, trun.Entries, 2)
	assert.Equal(t, uint32(math.MaxUint32), trun.Entries[0].SampleDuration)
	assert.Equal(t, uint32(1), trun.Entries[1].SampleDuration)
}

// TestOverflowStcoTruncation documents the 32-bit ceiling of stco: a value that
// only fits in co64 cannot be expressed by stco, and reducing it discards the
// high word. The library does not detect this semantic loss because stco is a
// faithful uint32 container; callers needing >4 GiB offsets must choose co64.
func TestOverflowStcoTruncation(t *testing.T) {
	const hugeOffset = uint64(1)<<32 + 0xABCD

	// co64 preserves the full value.
	co64Spec := &timelineTrackSpec{
		order: 0, trackID: 8, timescale: 24000, duration: 24000, version: 1,
		use64BitOffsets: true,
		samples:         []timelineSampleSpec{{size: 5, delta: 24000}},
	}
	co64bin := newTimelineBuilder(t).build(24000, 24000, 1, []*timelineTrackSpec{co64Spec})
	co64bin = patchCo64Offset(t, co64bin, hugeOffset)
	co64info := probeBytes(t, co64bin)
	assert.Equal(t, hugeOffset, co64info.Tracks[0].Chunks[0].DataOffset)

	// The low 32 bits are all an stco table could hold; assert they differ
	// from the true offset so truncation is visibly lossy, not coincidental.
	low32 := uint32(hugeOffset & math.MaxUint32)
	assert.Equal(t, uint32(0xABCD), low32)
	assert.NotEqual(t, uint64(low32), hugeOffset)

	// Encoding that truncated value into a genuine stco box round-trips
	// exactly as uint32: the codec neither widens nor rejects it.
	stcoSpec := &timelineTrackSpec{
		order: 0, trackID: 9, timescale: 24000, duration: 24000, version: 0,
		samples: []timelineSampleSpec{{size: 5, delta: 24000}},
	}
	stcobin := newTimelineBuilder(t).build(24000, 24000, 0, []*timelineTrackSpec{stcoSpec})
	stcoPath := BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()}
	// stco payload: version/flags (4) + entry_count (4) + offset (4).
	stcobin = writeUint32Payload(t, stcobin, stcoPath, 8, low32)
	stcoinfo := probeBytes(t, stcobin)
	assert.Equal(t, uint64(low32), stcoinfo.Tracks[0].Chunks[0].DataOffset)
}

// writeUint32Payload writes a big-endian uint32 at payloadOffset inside the
// first box matching path.
func writeUint32Payload(t *testing.T, bin []byte, path BoxPath, payloadOffset uint64, value uint32) []byte {
	t.Helper()
	r := bytes.NewReader(bin)
	bis, err := ExtractBoxes(r, nil, []BoxPath{path})
	require.NoError(t, err)
	require.NotEmpty(t, bis)
	start := bis[0].Offset + bis[0].HeaderSize + payloadOffset
	out := append([]byte(nil), bin...)
	for i := 0; i < 4; i++ {
		out[start+uint64(i)] = byte(value >> (24 - 8*i))
	}
	return out
}

// TestRejectTruncatedContainer pins that a box whose declared size runs past
// EOF is rejected by Probe itself rather than being partially read.
func TestRejectTruncatedContainer(t *testing.T) {
	tracks := []*timelineTrackSpec{
		{
			order: 0, trackID: 1, timescale: 90000, duration: 30000, version: 0,
			samples: []timelineSampleSpec{{size: 100, delta: 30000}},
		},
	}
	bin := newTimelineBuilder(t).build(1000, 1000, 0, tracks)
	for _, cut := range []int{1, 8, 16} {
		require.Less(t, cut, len(bin))
		truncated := bin[:len(bin)-cut]
		_, err := Probe(bytes.NewReader(truncated))
		require.Errorf(t, err, "truncating %d bytes must be rejected", cut)
	}
}

// TestRejectUnsupportedBoxVersion pins that an unknown full-box version is a
// hard error from the codec and therefore from Probe; the library does not
// guess a layout for a version it does not implement.
func TestRejectUnsupportedBoxVersion(t *testing.T) {
	tracks := []*timelineTrackSpec{
		{
			order: 0, trackID: 1, timescale: 90000, duration: 30000, version: 0,
			samples: []timelineSampleSpec{{size: 100, delta: 30000}},
		},
	}
	bin := newTimelineBuilder(t).build(1000, 1000, 0, tracks)

	for _, tc := range []struct {
		name string
		path BoxPath
	}{
		{"mvhd", BoxPath{BoxTypeMoov(), BoxTypeMvhd()}},
		{"mdhd", BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMdhd()}},
		{"tkhd", BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeTkhd()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Version is the first payload byte of every full box.
			corrupt := patchPayloadByte(t, bin, tc.path, 0, 9)
			_, err := Probe(bytes.NewReader(corrupt))
			require.Error(t, err)
			assert.Truef(t, errors.Is(err, ErrUnsupportedBoxVersion), "expected ErrUnsupportedBoxVersion, got %v", err)
		})
	}
}

// TestPreserveOutOfRangeChunkOffset records a deliberate non-rejection: Probe
// decodes chunk offsets verbatim and does not verify that they point inside the
// file. A consumer that dereferences sample bytes must therefore validate
// reachability itself; go-mp4 reports the table value and nothing more.
func TestPreserveOutOfRangeChunkOffset(t *testing.T) {
	tracks := []*timelineTrackSpec{
		{
			order: 0, trackID: 1, timescale: 90000, duration: 30000, version: 0,
			samples: []timelineSampleSpec{{size: 100, delta: 30000}},
		},
	}
	bin := newTimelineBuilder(t).build(1000, 1000, 0, tracks)
	stcoPath := BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()}
	bin = writeUint32Payload(t, bin, stcoPath, 8, math.MaxUint32)

	info, err := Probe(bytes.NewReader(bin))
	require.NoError(t, err, "out-of-range stco offset is preserved, not rejected")
	require.Len(t, info.Tracks[0].Chunks, 1)
	assert.Equal(t, uint64(math.MaxUint32), info.Tracks[0].Chunks[0].DataOffset)
	assert.Greater(t, info.Tracks[0].Chunks[0].DataOffset, uint64(len(bin)))
}

// TestRejectTruncatedBoxPayload checks that a full box declaring more payload
// than its enclosing box provides cannot be parsed: the unmarshaller must not
// read past the box boundary.
func TestRejectTruncatedBoxPayload(t *testing.T) {
	tracks := []*timelineTrackSpec{
		{
			order: 0, trackID: 1, timescale: 90000, duration: 30000, version: 0,
			samples: []timelineSampleSpec{{size: 100, delta: 30000}},
		},
	}
	bin := newTimelineBuilder(t).build(1000, 1000, 0, tracks)

	// Inflate the stts entry count to demand entries that do not exist. stts
	// payload: version/flags (4) + entry_count (4); claim 0x00010000 entries.
	sttsPath := BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStts()}
	corrupt := writeUint32Payload(t, bin, sttsPath, 4, 0x00010000)
	_, err := Probe(bytes.NewReader(corrupt))
	require.Error(t, err, "an stts entry count exceeding the box payload must fail to decode")
}
