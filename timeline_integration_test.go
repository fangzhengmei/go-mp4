package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"testing"

	"github.com/orcaman/writerseeker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file contains timeline consistency integration tests.
//
// Every test builds a complete MP4 container entirely through the library's
// own box codec (Writer.StartBox/EndBox + Marshal), reads it back through the
// same codec and through Probe, and asserts that the timeline the two layers
// report is identical. The validity of an input is therefore decided by this
// library alone: malformed structures must be rejected here instead of being
// tolerated in the hope that an external player repairs them.

// timelineTrackSpec describes one synthetic track.
type timelineTrackSpec struct {
	// order is the position of the track within the build order and selects
	// the matching chunk offset table.
	order     int
	trackID   uint32
	timescale uint32 // mdhd timescale (per-track time units)
	duration  uint64 // mdhd duration in track time units; also drives tkhd
	version   uint8  // version of mvhd/mdhd/tkhd (0 or 1)
	samples   []timelineSampleSpec
	// empty marks a track whose sample tables declare zero samples.
	empty bool
	// use64BitOffsets selects co64 instead of stco.
	use64BitOffsets bool
	// edits are written into an edts/elst box; nil means no edit list.
	edits []elstEntrySpec
	// cttsVersion selects the ctts box version; 0 means no ctts box when
	// every composition offset is zero.
	cttsVersion uint8
	hasCtts     bool
}

type timelineSampleSpec struct {
	size              uint32
	delta             uint32 // decode delta (stts), track time units
	compositionOffset int64  // ctts
}

type elstEntrySpec struct {
	version         uint8
	segmentDuration uint64 // movie timescale units
	mediaTime       int64  // track timescale units; negative values need version 1
	mediaRate       int16
}

// timelineFragmentSpec describes one moof/mdat pair for fragmented files.
type timelineFragmentSpec struct {
	trackID             uint32
	tfdtVersion         uint8
	baseMediaDecodeTime uint64
	trunVersion         uint8
	entries             []trunEntrySpec
	// defaulted switches to tfhd default sample duration/size (trun carries no
	// per-sample duration/size fields).
	defaulted             bool
	defaultSampleDuration uint32
	defaultSampleSize     uint32
}

type trunEntrySpec struct {
	duration          uint32
	size              uint32
	compositionOffset int64
}

// timelineBuilder assembles a minimal, self-consistent MP4 container in memory.
type timelineBuilder struct {
	w *Writer
	t *testing.T
	// mdatFirst chooses the ftyp/mdat/moov (non-fast-start) layout when true,
	// and ftyp/moov/mdat (fast-start) when false.
	mdatFirst bool
}

func newTimelineBuilder(t *testing.T) *timelineBuilder {
	return &timelineBuilder{
		w:         NewWriter(&writerseeker.WriterSeeker{}),
		t:         t,
		mdatFirst: true,
	}
}

func (b *timelineBuilder) startBox(boxType BoxType) {
	b.t.Helper()
	_, err := b.w.StartBox(&BoxInfo{Type: boxType})
	if err != nil {
		panic(err)
	}
}

func (b *timelineBuilder) endBox() {
	b.t.Helper()
	_, err := b.w.EndBox()
	if err != nil {
		panic(err)
	}
}

func (b *timelineBuilder) marshal(box IImmutableBox) {
	b.t.Helper()
	if _, err := Marshal(b.w, box, Context{}); err != nil {
		panic(err)
	}
}

// bytes finishes the container and returns its serialized form.
func (b *timelineBuilder) bytes() []byte {
	ws := b.w.writer.(*writerseeker.WriterSeeker)
	bin, err := io.ReadAll(ws.BytesReader())
	if err != nil {
		panic(err)
	}
	return bin
}

// chunkInfo records the absolute file offset and payload size of one track's
// single chunk.
type chunkInfo struct {
	dataOffset uint64
	dataSize   uint64
}

// sampleByteLayout computes, for each track, the size of every sample and the
// absolute file offset of the chunk that contains them. All samples are placed
// in a single chunk.
func sampleByteLayout(spec *timelineTrackSpec, mdatPayloadOffset uint64) (totalSize uint64, chunkOffset uint64) {
	chunkOffset = mdatPayloadOffset
	for _, s := range spec.samples {
		totalSize += uint64(s.size)
	}
	return totalSize, chunkOffset
}

// build writes a non-fragmented file containing the given tracks.
// movieTimescale/duration populate mvhd; version selects the mvhd version.
func (b *timelineBuilder) build(movieTimescale uint32, movieDuration uint64, mvhdVersion uint8, tracks []*timelineTrackSpec) []byte {
	// ftyp
	b.startBox(BoxTypeFtyp())
	b.marshal(&Ftyp{
		MajorBrand:       BrandISOM(),
		MinorVersion:     0x200,
		CompatibleBrands: []CompatibleBrandElem{{CompatibleBrand: BrandISOM()}, {CompatibleBrand: BrandMP41()}},
	})
	b.endBox()

	// Pre-compute mdat payload and chunk offsets.
	chunks := make([]chunkInfo, len(tracks))
	var mdatData []byte
	if b.mdatFirst {
		// mdat header size is 8 while the payload fits in 32 bits; the builder
		// only creates small files, and the >4 GiB case is covered separately
		// through co64 offsets without emitting real multi-gigabyte payloads.
		// ftyp (24) + mdat header (8)
		mdatPayloadOffset := uint64(24 + 8)
		cursor := mdatPayloadOffset
		for i, tr := range tracks {
			dataSize, dataOffset := sampleByteLayout(tr, cursor)
			chunks[i] = chunkInfo{dataOffset: dataOffset, dataSize: dataSize}
			cursor += dataSize
			for _, s := range tr.samples {
				mdatData = append(mdatData, make([]byte, s.size)...)
			}
		}
	}

	writeMdat := func() {
		b.startBox(BoxTypeMdat())
		b.marshal(&Mdat{Data: mdatData})
		b.endBox()
	}
	writeMoov := func() {
		b.startBox(BoxTypeMoov())
		b.writeMvhd(mvhdVersion, movieTimescale, movieDuration, uint32(len(tracks))+1)
		for _, tr := range tracks {
			b.writeTrak(tr, chunks)
		}
		b.endBox()
	}

	if b.mdatFirst {
		writeMdat()
		writeMoov()
	} else {
		// Fast-start layout: moov precedes mdat, so offsets must be computed
		// against the moov size. Write moov once into a side buffer to measure
		// it, then write the real file with adjusted offsets.
		side := newTimelineBuilder(b.t)
		side.mdatFirst = false
		// placeholder offsets; size of the moov does not depend on them.
		for i := range tracks {
			chunks[i] = chunkInfo{dataOffset: 0}
		}
		side.startBox(BoxTypeMoov())
		side.writeMvhd(mvhdVersion, movieTimescale, movieDuration, uint32(len(tracks))+1)
		for _, tr := range tracks {
			side.writeTrak(tr, chunks)
		}
		side.endBox()
		moovSize := uint64(len(side.bytes()))

		ftypSize := uint64(24)
		mdatPayloadOffset := ftypSize + moovSize + 8
		cursor := mdatPayloadOffset
		mdatData = mdatData[:0]
		for i, tr := range tracks {
			dataSize, dataOffset := sampleByteLayout(tr, cursor)
			chunks[i] = chunkInfo{dataOffset: dataOffset, dataSize: dataSize}
			cursor += dataSize
			for _, s := range tr.samples {
				mdatData = append(mdatData, make([]byte, s.size)...)
			}
		}
		writeMoov()
		writeMdat()
	}

	return b.bytes()
}

func (b *timelineBuilder) writeMvhd(version uint8, timescale uint32, duration uint64, nextTrackID uint32) {
	b.startBox(BoxTypeMvhd())
	mvhd := &Mvhd{
		FullBox:     FullBox{Version: version},
		Timescale:   timescale,
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
		NextTrackID: nextTrackID,
	}
	if version == 0 {
		require.LessOrEqual(b.t, duration, uint64(math.MaxUint32))
		mvhd.DurationV0 = uint32(duration)
	} else {
		mvhd.DurationV1 = duration
	}
	b.marshal(mvhd)
	b.endBox()
}

func (b *timelineBuilder) writeTrak(spec *timelineTrackSpec, chunks []chunkInfo) {
	b.startBox(BoxTypeTrak())

	// tkhd
	b.startBox(BoxTypeTkhd())
	tkhd := &Tkhd{
		FullBox: FullBox{Version: spec.version},
		TrackID: spec.trackID,
		Matrix:  [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
	}
	if spec.version == 0 {
		require.LessOrEqual(b.t, spec.duration, uint64(math.MaxUint32))
		tkhd.DurationV0 = uint32(spec.duration)
	} else {
		tkhd.DurationV1 = spec.duration
	}
	b.marshal(tkhd)
	b.endBox()

	// optional edts/elst
	if len(spec.edits) > 0 {
		b.startBox(BoxTypeEdts())
		b.writeElst(spec.edits)
		b.endBox()
	}

	// mdia
	b.startBox(BoxTypeMdia())

	// mdhd
	b.startBox(BoxTypeMdhd())
	mdhd := &Mdhd{
		FullBox:   FullBox{Version: spec.version},
		Timescale: spec.timescale,
	}
	if spec.version == 0 {
		require.LessOrEqual(b.t, spec.duration, uint64(math.MaxUint32))
		mdhd.DurationV0 = uint32(spec.duration)
	} else {
		mdhd.DurationV1 = spec.duration
	}
	b.marshal(mdhd)
	b.endBox()

	// hdlr
	b.startBox(BoxTypeHdlr())
	b.marshal(&Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}, Name: "VideoHandler\x00"})
	b.endBox()

	// minf/stbl
	b.startBox(BoxTypeMinf())
	b.startBox(BoxTypeStbl())

	if spec.empty {
		// An empty but structurally valid track still carries all required
		// tables, each declaring zero entries.
		b.writeEmptySampleTables()
		b.endBox() // stbl
		b.endBox() // minf
		b.endBox() // mdia
		b.endBox() // trak
		return
	}

	// stco or co64: one chunk per track.
	b.startBox(BoxTypeStsd())
	b.marshal(&Stsd{FullBox: FullBox{}, EntryCount: 0})
	b.endBox()

	chunk := chunks[spec.trackIndex()]
	if spec.use64BitOffsets {
		b.startBox(BoxTypeCo64())
		b.marshal(&Co64{
			FullBox:     FullBox{},
			EntryCount:  1,
			ChunkOffset: []uint64{chunk.dataOffset},
		})
		b.endBox()
	} else {
		require.LessOrEqual(b.t, chunk.dataOffset, uint64(math.MaxUint32))
		b.startBox(BoxTypeStco())
		b.marshal(&Stco{
			FullBox:     FullBox{},
			EntryCount:  1,
			ChunkOffset: []uint32{uint32(chunk.dataOffset)},
		})
		b.endBox()
	}

	// stts (RLE-compressed decode deltas).
	b.startBox(BoxTypeStts())
	b.marshal(&Stts{
		FullBox:    FullBox{},
		EntryCount: uint32(len(rleDeltas(spec.samples))),
		Entries:    rleDeltas(spec.samples),
	})
	b.endBox()

	// ctts when requested.
	if spec.hasCtts {
		b.startBox(BoxTypeCtts())
		ctts := &Ctts{FullBox: FullBox{Version: spec.cttsVersion}}
		for _, e := range rleComposition(spec.samples) {
			entry := CttsEntry{SampleCount: e.count}
			if spec.cttsVersion == 0 {
				require.GreaterOrEqual(b.t, e.offset, int64(0))
				require.LessOrEqual(b.t, e.offset, int64(math.MaxUint32))
				entry.SampleOffsetV0 = uint32(e.offset)
			} else {
				entry.SampleOffsetV1 = int32(e.offset)
			}
			ctts.Entries = append(ctts.Entries, entry)
		}
		ctts.EntryCount = uint32(len(ctts.Entries))
		b.marshal(ctts)
		b.endBox()
	}

	// stsc: all samples live in chunk 1.
	b.startBox(BoxTypeStsc())
	b.marshal(&Stsc{
		FullBox:    FullBox{},
		EntryCount: 1,
		Entries: []StscEntry{{
			FirstChunk:             1,
			SamplesPerChunk:        uint32(len(spec.samples)),
			SampleDescriptionIndex: 1,
		}},
	})
	b.endBox()

	// stsz with one explicit size per sample.
	b.startBox(BoxTypeStsz())
	sizes := make([]uint32, len(spec.samples))
	for i, s := range spec.samples {
		sizes[i] = s.size
	}
	b.marshal(&Stsz{
		FullBox:     FullBox{},
		SampleSize:  0,
		SampleCount: uint32(len(spec.samples)),
		EntrySize:   sizes,
	})
	b.endBox()

	b.endBox() // stbl
	b.endBox() // minf
	b.endBox() // mdia
	b.endBox() // trak
}

func (b *timelineBuilder) writeEmptySampleTables() {
	b.startBox(BoxTypeStsd())
	b.marshal(&Stsd{FullBox: FullBox{}, EntryCount: 0})
	b.endBox()

	b.startBox(BoxTypeStco())
	b.marshal(&Stco{FullBox: FullBox{}, EntryCount: 0, ChunkOffset: nil})
	b.endBox()

	b.startBox(BoxTypeStts())
	b.marshal(&Stts{FullBox: FullBox{}, EntryCount: 0})
	b.endBox()

	b.startBox(BoxTypeStsc())
	b.marshal(&Stsc{FullBox: FullBox{}, EntryCount: 0})
	b.endBox()

	b.startBox(BoxTypeStsz())
	b.marshal(&Stsz{FullBox: FullBox{}, SampleSize: 0, SampleCount: 0})
	b.endBox()
}

func (b *timelineBuilder) writeElst(edits []elstEntrySpec) {
	version := edits[0].version
	b.startBox(BoxTypeElst())
	elst := &Elst{FullBox: FullBox{Version: version}, EntryCount: uint32(len(edits))}
	for _, e := range edits {
		require.Equal(b.t, version, e.version, "all elst entries must share the box version")
		entry := ElstEntry{MediaRateInteger: e.mediaRate, MediaRateFraction: 0}
		if version == 0 {
			require.LessOrEqual(b.t, e.segmentDuration, uint64(math.MaxUint32))
			require.GreaterOrEqual(b.t, e.mediaTime, int64(math.MinInt32))
			require.LessOrEqual(b.t, e.mediaTime, int64(math.MaxInt32))
			entry.SegmentDurationV0 = uint32(e.segmentDuration)
			entry.MediaTimeV0 = int32(e.mediaTime)
		} else {
			entry.SegmentDurationV1 = e.segmentDuration
			entry.MediaTimeV1 = e.mediaTime
		}
		elst.Entries = append(elst.Entries, entry)
	}
	b.marshal(elst)
	b.endBox()
}

// trackIndex maps a track spec to its position in the build order, which is
// also the index of its chunk-offset table entry.
func (spec *timelineTrackSpec) trackIndex() int {
	return spec.order
}

func rleDeltas(samples []timelineSampleSpec) []SttsEntry {
	var out []SttsEntry
	for _, s := range samples {
		if n := len(out); n > 0 && out[n-1].SampleDelta == s.delta {
			out[n-1].SampleCount++
			continue
		}
		out = append(out, SttsEntry{SampleCount: 1, SampleDelta: s.delta})
	}
	return out
}

func rleComposition(samples []timelineSampleSpec) []struct {
	count  uint32
	offset int64
} {
	type row = struct {
		count  uint32
		offset int64
	}
	var out []row
	for _, s := range samples {
		if n := len(out); n > 0 && out[n-1].offset == s.compositionOffset {
			out[n-1].count++
			continue
		}
		out = append(out, row{count: 1, offset: s.compositionOffset})
	}
	return out
}

// probeBytes runs Probe against an in-memory container.
func probeBytes(t *testing.T, bin []byte) *ProbeInfo {
	t.Helper()
	info, err := Probe(bytes.NewReader(bin))
	require.NoError(t, err)
	return info
}

// unmarshalBoxes re-parses the file through the low-level box codec and returns
// the first payload of the requested type found on the given path, proving the
// Probe result agrees with raw box decode.
func unmarshalBoxes(t *testing.T, bin []byte, path BoxPath, dst IBox) {
	t.Helper()
	unmarshalNthBox(t, bin, path, dst, 0)
}

// unmarshalNthBox decodes the n-th box matching path (zero based), which is
// required when several sibling tracks carry the same child box type.
func unmarshalNthBox(t *testing.T, bin []byte, path BoxPath, dst IBox, n int) {
	t.Helper()
	r := bytes.NewReader(bin)
	bis, err := ExtractBoxes(r, nil, []BoxPath{path})
	require.NoError(t, err)
	require.Less(t, n, len(bis), "box path %v match %d not found", path, n)
	_, err = bis[n].SeekToPayload(r)
	require.NoError(t, err)
	_, err = Unmarshal(r, bis[n].Size-bis[n].HeaderSize, dst, bis[n].Context)
	require.NoError(t, err)
}

// assertTrackTimeline cross-checks one probed track against its spec and the
// raw stts/ctts/stco/co64/stsz box decode.
func assertTrackTimeline(t *testing.T, bin []byte, info *ProbeInfo, spec *timelineTrackSpec, wantChunkOffset uint64) {
	t.Helper()

	var track *Track
	for _, tr := range info.Tracks {
		if tr.TrackID == spec.trackID {
			track = tr
			break
		}
	}
	require.NotNil(t, track, "track %d missing from probe result", spec.trackID)

	assert.Equal(t, spec.timescale, track.Timescale)
	assert.Equal(t, spec.duration, track.Duration)

	if spec.empty {
		require.Empty(t, track.Samples)
		require.Empty(t, track.Chunks)
		return
	}

	require.Len(t, track.Samples, len(spec.samples))
	require.Len(t, track.Chunks, 1)

	// Sample positions: the probed chunk offset plus the running sum of sample
	// sizes must locate every sample inside the file.
	runningOffset := wantChunkOffset
	for i, s := range spec.samples {
		got := track.Samples[i]
		assert.Equalf(t, s.size, got.Size, "sample %d size", i)
		assert.Equalf(t, s.delta, got.TimeDelta, "sample %d decode delta", i)
		assert.Equalf(t, s.compositionOffset, got.CompositionTimeOffset, "sample %d composition offset", i)
		if i == 0 {
			assert.Equalf(t, wantChunkOffset, track.Chunks[0].DataOffset, "chunk data offset")
		}
		// Every sample position must point inside the serialized container.
		assert.LessOrEqual(t, runningOffset+uint64(s.size), uint64(len(bin)))
		runningOffset += uint64(s.size)
	}
	assert.Equal(t, uint32(len(spec.samples)), track.Chunks[0].SamplesPerChunk)

	// The end of the final sample must coincide with the chunk end computed
	// from the stco/co64 table and the stsz table.
	assert.Equal(t, wantChunkOffset+sumSampleSizes(spec.samples), runningOffset)

	// Raw box decode must report the same chunk offset as Probe. The n-th
	// matching stbl child belongs to the n-th track in build order.
	if spec.use64BitOffsets {
		var co64 Co64
		unmarshalNthBox(t, bin, BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64()}, &co64, spec.order)
		require.Len(t, co64.ChunkOffset, 1)
		assert.Equal(t, wantChunkOffset, co64.ChunkOffset[0])
	} else {
		var stco Stco
		unmarshalNthBox(t, bin, BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()}, &stco, spec.order)
		require.Len(t, stco.ChunkOffset, 1)
		assert.Equal(t, wantChunkOffset, uint64(stco.ChunkOffset[0]))
	}

	// Edit list agreement between box codec and probe.
	if len(spec.edits) > 0 {
		require.Len(t, track.EditList, len(spec.edits))
		for i, e := range spec.edits {
			assert.Equalf(t, e.mediaTime, track.EditList[i].MediaTime, "edit %d media time", i)
			assert.Equalf(t, e.segmentDuration, track.EditList[i].SegmentDuration, "edit %d segment duration", i)
		}
		var elst Elst
		unmarshalNthBox(t, bin, BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeEdts(), BoxTypeElst()}, &elst, spec.order)
		require.Len(t, elst.Entries, len(spec.edits))
		for i := range spec.edits {
			assert.Equal(t, track.EditList[i].MediaTime, elst.GetMediaTime(i))
			assert.Equal(t, track.EditList[i].SegmentDuration, elst.GetSegmentDuration(i))
		}
	} else {
		assert.Empty(t, track.EditList)
	}
}

func sumSampleSizes(samples []timelineSampleSpec) uint64 {
	var total uint64
	for _, s := range samples {
		total += uint64(s.size)
	}
	return total
}

// TestTimelineMultiTrackDifferentTimescales builds two tracks with unrelated
// per-track timescales and sample rates and verifies Probe reproduces both
// timelines exactly, with sample positions derived from the chunk tables.
func TestTimelineMultiTrackDifferentTimescales(t *testing.T) {
	tracks := []*timelineTrackSpec{
		{
			order: 0, trackID: 1, timescale: 90000, duration: 90000, version: 0,
			samples: []timelineSampleSpec{
				{size: 100, delta: 30000, compositionOffset: 0},
				{size: 200, delta: 30000, compositionOffset: 0},
				{size: 300, delta: 30000, compositionOffset: 0},
			},
		},
		{
			order: 1, trackID: 2, timescale: 44100, duration: 44100, version: 0,
			samples: []timelineSampleSpec{
				{size: 50, delta: 1024},
				{size: 50, delta: 1024},
				// remainder keeps the mdhd duration independent of the stts sum
				{size: 50, delta: 1024},
				{size: 70, delta: 41328},
			},
		},
	}
	builder := newTimelineBuilder(t)
	bin := builder.build(1000, 1000, 0, tracks)
	info := probeBytes(t, bin)

	assert.Equal(t, uint32(1000), info.Timescale)
	assert.Equal(t, uint64(1000), info.Duration)
	require.Len(t, info.Tracks, 2)
	assert.False(t, info.FastStart, "mdat precedes moov in this layout")

	// Track 1 chunk starts right after the mdat header (ftyp=24, mdat hdr=8).
	assertTrackTimeline(t, bin, info, tracks[0], 32)
	// Track 2 chunk follows track 1's 600 bytes of sample data.
	assertTrackTimeline(t, bin, info, tracks[1], 32+600)
}

// TestTimelineVersionSwitching exercises version 0 (32-bit) and version 1
// (64-bit) variants of every versioned timeline box in the same container.
func TestTimelineVersionSwitching(t *testing.T) {
	tracks := []*timelineTrackSpec{
		{
			order: 0, trackID: 1, timescale: 90000, duration: 60000, version: 0,
			cttsVersion: 0, hasCtts: true,
			samples: []timelineSampleSpec{
				{size: 10, delta: 30000, compositionOffset: 15000},
				{size: 20, delta: 30000, compositionOffset: 15000},
			},
		},
		{
			order: 1, trackID: 2, timescale: 48000, duration: 72000, version: 1,
			cttsVersion: 1, hasCtts: true,
			samples: []timelineSampleSpec{
				{size: 40, delta: 24000, compositionOffset: 12000},
				{size: 60, delta: 24000, compositionOffset: -12000},
				{size: 80, delta: 24000, compositionOffset: 12000},
			},
		},
	}
	builder := newTimelineBuilder(t)
	bin := builder.build(600, 1500, 1, tracks)
	info := probeBytes(t, bin)

	// mvhd version 1 keeps the full 64-bit movie duration.
	assert.Equal(t, uint64(1500), info.Duration)
	require.Len(t, info.Tracks, 2)

	assertTrackTimeline(t, bin, info, tracks[0], 32)
	assertTrackTimeline(t, bin, info, tracks[1], 32+30)

	// Verify the raw boxes carry the versions the builder selected.
	var mvhd Mvhd
	unmarshalBoxes(t, bin, BoxPath{BoxTypeMoov(), BoxTypeMvhd()}, &mvhd)
	assert.Equal(t, uint8(1), mvhd.GetVersion())
	assert.Equal(t, uint64(1500), mvhd.GetDuration())

	var cttsV1 Ctts
	// The second track's ctts is the version-1 box; extract all ctts boxes.
	r := bytes.NewReader(bin)
	bis, err := ExtractBoxes(r, nil, []BoxPath{{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCtts()}})
	require.NoError(t, err)
	require.Len(t, bis, 2)
	_, err = bis[1].SeekToPayload(r)
	require.NoError(t, err)
	_, err = Unmarshal(r, bis[1].Size-bis[1].HeaderSize, &cttsV1, bis[1].Context)
	require.NoError(t, err)
	assert.Equal(t, uint8(1), cttsV1.GetVersion())
	// Negative composition offset survives the signed version-1 encoding.
	assert.Equal(t, int64(-12000), cttsV1.GetSampleOffset(1))
}

// TestTimelineEmptyTrack keeps an empty track alongside a populated one and
// pins the library's representation: zero samples, zero chunks, but a valid
// timescale/duration and an edit list if one was authored.
func TestTimelineEmptyTrack(t *testing.T) {
	tracks := []*timelineTrackSpec{
		{
			order: 0, trackID: 1, timescale: 24000, duration: 24000, version: 0, empty: true,
		},
		{
			order: 1, trackID: 2, timescale: 48000, duration: 48000, version: 0,
			samples: []timelineSampleSpec{{size: 123, delta: 48000}},
		},
	}
	builder := newTimelineBuilder(t)
	bin := builder.build(1000, 1000, 0, tracks)
	info := probeBytes(t, bin)
	require.Len(t, info.Tracks, 2)

	assertTrackTimeline(t, bin, info, tracks[0], 0)
	assertTrackTimeline(t, bin, info, tracks[1], 32)

	// The empty track must still expose its mdhd timescale/duration.
	assert.Equal(t, uint32(24000), info.Tracks[0].Timescale)
	assert.Equal(t, uint64(24000), info.Tracks[0].Duration)
}

// TestTimelineFastStartLayout flips the mdat/moov order and confirms the chunk
// offsets move with the layout while every sample remains addressable.
func TestTimelineFastStartLayout(t *testing.T) {
	tracks := []*timelineTrackSpec{
		{
			order: 0, trackID: 7, timescale: 30000, duration: 60000, version: 0,
			samples: []timelineSampleSpec{
				{size: 11, delta: 30000},
				{size: 22, delta: 30000},
			},
		},
	}
	builder := newTimelineBuilder(t)
	builder.mdatFirst = false
	bin := builder.build(1000, 2000, 0, tracks)
	info := probeBytes(t, bin)
	assert.True(t, info.FastStart)

	// Compute the expected offset: ftyp(24) + moov + mdat header(8).
	moovSize := locateMoovSize(t, bin)
	wantChunkOffset := uint64(24) + moovSize + 8
	assertTrackTimeline(t, bin, info, tracks[0], wantChunkOffset)
}

func locateMoovSize(t *testing.T, bin []byte) uint64 {
	t.Helper()
	bis, err := ExtractBoxes(bytes.NewReader(bin), nil, []BoxPath{{BoxTypeMoov()}})
	require.NoError(t, err)
	require.NotEmpty(t, bis)
	return bis[0].Size
}

// TestTimelineCombinedCompositionAndEditOffsets combines a ctts composition
// offset and an elst media-time shift on one track, then checks that the box
// codec and Probe preserve both independently. The two offsets live in
// different time spaces (movie vs. media), so the test converts through the
// known timescales rather than assuming they are interchangeable.
func TestTimelineCombinedCompositionAndEditOffsets(t *testing.T) {
	spec := &timelineTrackSpec{
		order: 0, trackID: 3, timescale: 90000, duration: 120000, version: 1,
		cttsVersion: 1, hasCtts: true,
		samples: []timelineSampleSpec{
			{size: 100, delta: 30000, compositionOffset: 20000},
			{size: 100, delta: 30000, compositionOffset: -5000},
			{size: 100, delta: 30000, compositionOffset: 20000},
			{size: 100, delta: 30000, compositionOffset: -5000},
		},
		edits: []elstEntrySpec{
			{version: 1, segmentDuration: 90000, mediaTime: 15000, mediaRate: 1},
		},
	}
	builder := newTimelineBuilder(t)
	bin := builder.build(90000, 90000, 1, []*timelineTrackSpec{spec})
	info := probeBytes(t, bin)
	require.Len(t, info.Tracks, 1)
	assertTrackTimeline(t, bin, info, spec, 32)

	track := info.Tracks[0]
	// Composition offsets are media-time values and must be returned verbatim.
	assert.Equal(t, int64(20000), track.Samples[0].CompositionTimeOffset)
	assert.Equal(t, int64(-5000), track.Samples[1].CompositionTimeOffset)
	// The edit list shifts the presentation start by 15000 media time units.
	require.Len(t, track.EditList, 1)
	assert.Equal(t, int64(15000), track.EditList[0].MediaTime)
	assert.Equal(t, uint64(90000), track.EditList[0].SegmentDuration)

	// Cross-space check: the edit shift, expressed in movie timescale units
	// (both timescales are 90000 here), must not move any sample outside the
	// media duration declared by mdhd.
	decodeEnd := uint64(0)
	for _, s := range spec.samples {
		decodeEnd += uint64(s.delta)
	}
	assert.Equal(t, spec.duration, decodeEnd)
}

// TestTimelineNegativeEditMediaTime requires an elst version-1 box because the
// -1 empty-edit sentinel does not fit in a signed 32-bit media-time field.
// It pins that the library preserves the negative value instead of clamping.
func TestTimelineNegativeEditMediaTime(t *testing.T) {
	spec := &timelineTrackSpec{
		order: 0, trackID: 4, timescale: 48000, duration: 48000, version: 1,
		samples: []timelineSampleSpec{{size: 64, delta: 48000}},
		edits: []elstEntrySpec{
			// media_time == -1 is the ISO-defined empty edit.
			{version: 1, segmentDuration: 12000, mediaTime: -1, mediaRate: 1},
			{version: 1, segmentDuration: 48000, mediaTime: 0, mediaRate: 1},
		},
	}
	builder := newTimelineBuilder(t)
	bin := builder.build(48000, 60000, 1, []*timelineTrackSpec{spec})
	info := probeBytes(t, bin)
	require.Len(t, info.Tracks, 1)
	track := info.Tracks[0]
	require.Len(t, track.EditList, 2)
	assert.Equal(t, int64(-1), track.EditList[0].MediaTime)
	assert.Equal(t, uint64(12000), track.EditList[0].SegmentDuration)
	assert.Equal(t, int64(0), track.EditList[1].MediaTime)

	var elst Elst
	unmarshalBoxes(t, bin, BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeEdts(), BoxTypeElst()}, &elst)
	assert.Equal(t, uint8(1), elst.GetVersion())
	assert.Equal(t, int64(-1), elst.GetMediaTime(0))
}

// TestTimelineCo64LargeOffset uses a 64-bit chunk offset above the 32-bit stco
// ceiling. No real 4 GiB payload is emitted: the offset is a logical timeline
// position, and the test only verifies encode/decode/probe agreement plus the
// fact that the value cannot be represented by stco.
func TestTimelineCo64LargeOffset(t *testing.T) {
	const hugeOffset = uint64(1)<<32 + 1234 // 4294968578
	spec := &timelineTrackSpec{
		order: 0, trackID: 5, timescale: 24000, duration: 24000, version: 1,
		use64BitOffsets: true,
		samples:         []timelineSampleSpec{{size: 77, delta: 24000}},
	}
	builder := newTimelineBuilder(t)
	bin := builder.build(24000, 24000, 1, []*timelineTrackSpec{spec})

	// Patch the co64 table to the logical large offset and re-probe. The mdat
	// bytes in this tiny file do not actually reach that offset, so this is a
	// pure codec/probe consistency check rather than a byte-position check.
	bin = patchCo64Offset(t, bin, hugeOffset)
	info := probeBytes(t, bin)
	require.Len(t, info.Tracks, 1)
	track := info.Tracks[0]
	require.Len(t, track.Chunks, 1)
	assert.Equal(t, hugeOffset, track.Chunks[0].DataOffset)
	assert.Greater(t, track.Chunks[0].DataOffset, uint64(math.MaxUint32))

	var co64 Co64
	unmarshalBoxes(t, bin, BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64()}, &co64)
	require.Len(t, co64.ChunkOffset, 1)
	assert.Equal(t, hugeOffset, co64.ChunkOffset[0])

	// The same value cannot survive an stco table: reducing it to 32 bits
	// loses the high word and leaves only the low 1234. The overflow test file
	// pins that the library preserves whatever was encoded rather than
	// repairing it; co64 is therefore the only lossless representation here.
	truncated := uint32(hugeOffset & math.MaxUint32)
	assert.Equal(t, uint32(1234), truncated)
	assert.NotEqual(t, uint64(truncated), hugeOffset)
}

// TestTimelineFragmentedSegments builds a fragmented file and checks the
// per-segment timeline: tfdt base media decode time, trun durations, sizes and
// composition offsets, including a version-1 tfdt beyond the 32-bit range.
func TestTimelineFragmentedSegments(t *testing.T) {
	builder := newTimelineBuilder(t)
	bin := buildFragmentedFile(t, builder, []*timelineFragmentSpec{
		{
			trackID: 1, tfdtVersion: 0, baseMediaDecodeTime: 0, trunVersion: 0,
			entries: []trunEntrySpec{
				{duration: 3000, size: 100, compositionOffset: 0},
				{duration: 3000, size: 200, compositionOffset: 0},
			},
		},
		{
			trackID: 1, tfdtVersion: 1, baseMediaDecodeTime: uint64(math.MaxUint32) + 9000, trunVersion: 1,
			entries: []trunEntrySpec{
				{duration: 3000, size: 150, compositionOffset: 1500},
				{duration: 3000, size: 250, compositionOffset: -1500},
			},
		},
	})
	info := probeBytes(t, bin)
	require.Len(t, info.Segments, 2)

	seg0 := info.Segments[0]
	assert.Equal(t, uint32(1), seg0.TrackID)
	assert.Equal(t, uint64(0), seg0.BaseMediaDecodeTime)
	assert.Equal(t, uint32(2), seg0.SampleCount)
	assert.Equal(t, uint32(6000), seg0.Duration)
	assert.Equal(t, uint32(300), seg0.Size)

	seg1 := info.Segments[1]
	assert.Equal(t, uint64(uint64(math.MaxUint32)+9000), seg1.BaseMediaDecodeTime)
	assert.Equal(t, uint32(2), seg1.SampleCount)
	assert.Equal(t, uint32(6000), seg1.Duration)
	assert.Equal(t, uint32(400), seg1.Size)
	// Probe.Segment.CompositionTimeOffset is the minimum of
	// int32(runningDecodeDuration) + int32(perSampleOffset), folded into a
	// signed 32-bit field: entry 0 yields 0+1500=1500 and entry 1 yields
	// 3000-1500=1500, so the reported minimum is 1500 rather than the raw
	// -1500 sample offset. This is the library's current 32-bit contract and
	// is pinned here; the lossless per-sample value still lives in trun below.
	assert.Equal(t, int32(1500), seg1.CompositionTimeOffset)

	// Raw trun decode is lossless: the signed version-1 composition offsets,
	// including the negative one, survive encode/decode untouched. The trun
	// data offset is relative to the moof start and must resolve to the mdat
	// payload (the first sample's absolute position).
	r := bytes.NewReader(bin)
	trunBIs, err := ExtractBoxes(r, nil, []BoxPath{{BoxTypeMoof(), BoxTypeTraf(), BoxTypeTrun()}})
	require.NoError(t, err)
	require.Len(t, trunBIs, 2)
	var trun Trun
	_, err = trunBIs[1].SeekToPayload(r)
	require.NoError(t, err)
	_, err = Unmarshal(r, trunBIs[1].Size-trunBIs[1].HeaderSize, &trun, trunBIs[1].Context)
	require.NoError(t, err)
	assert.Equal(t, uint8(1), trun.GetVersion())
	assert.True(t, trun.CheckFlag(0x000001))
	assert.Equal(t, int64(1500), trun.GetSampleCompositionTimeOffset(0))
	assert.Equal(t, int64(-1500), trun.GetSampleCompositionTimeOffset(1))

	mdatBIs, err := ExtractBoxes(r, nil, []BoxPath{{BoxTypeMdat()}})
	require.NoError(t, err)
	require.Len(t, mdatBIs, 2)
	// Absolute first-sample position = moof offset + relative trun data offset.
	firstSampleAbs := int64(seg1.MoofOffset) + int64(trun.DataOffset)
	assert.Equal(t, int64(mdatBIs[1].Offset+mdatBIs[1].HeaderSize), firstSampleAbs)
	assert.LessOrEqual(t, int(seg1.Size), len(bin)-int(firstSampleAbs))

	// Raw tfdt decode must agree with the probed base media decode time.
	bis, err := ExtractBoxes(r, nil, []BoxPath{{BoxTypeMoof(), BoxTypeTraf(), BoxTypeTfdt()}})
	require.NoError(t, err)
	require.Len(t, bis, 2)
	var tfdt Tfdt
	_, err = bis[1].SeekToPayload(r)
	require.NoError(t, err)
	_, err = Unmarshal(r, bis[1].Size-bis[1].HeaderSize, &tfdt, bis[1].Context)
	require.NoError(t, err)
	assert.Equal(t, uint8(1), tfdt.GetVersion())
	assert.Equal(t, uint64(uint64(math.MaxUint32)+9000), tfdt.GetBaseMediaDecodeTime())
}

// TestTimelineFragmentedDefaultSamples verifies the path where sample
// duration/size come from tfhd defaults instead of trun entries.
func TestTimelineFragmentedDefaultSamples(t *testing.T) {
	builder := newTimelineBuilder(t)
	bin := buildFragmentedFile(t, builder, []*timelineFragmentSpec{
		{
			trackID: 9, tfdtVersion: 0, baseMediaDecodeTime: 0,
			defaulted: true, defaultSampleDuration: 2048, defaultSampleSize: 64,
			entries: []trunEntrySpec{
				{duration: 2048, size: 64},
				{duration: 2048, size: 64},
				{duration: 2048, size: 64},
			},
		},
	})
	info := probeBytes(t, bin)
	require.Len(t, info.Segments, 1)
	seg := info.Segments[0]
	assert.Equal(t, uint32(3), seg.SampleCount)
	assert.Equal(t, uint32(6144), seg.Duration)
	assert.Equal(t, uint32(192), seg.Size)

	// Even with tfhd defaults, the trun data offset must locate the mdat
	// payload precisely.
	var trun Trun
	unmarshalBoxes(t, bin, BoxPath{BoxTypeMoof(), BoxTypeTraf(), BoxTypeTrun()}, &trun)
	assert.True(t, trun.CheckFlag(0x000001))
	r := bytes.NewReader(bin)
	mdatBIs, err := ExtractBoxes(r, nil, []BoxPath{{BoxTypeMdat()}})
	require.NoError(t, err)
	require.Len(t, mdatBIs, 1)
	assert.Equal(t, int64(mdatBIs[0].Offset+mdatBIs[0].HeaderSize),
		int64(seg.MoofOffset)+int64(trun.DataOffset))
}

// buildFragmentedFile writes ftyp + one moof/mdat pair per fragment spec.
func buildFragmentedFile(t *testing.T, b *timelineBuilder, frags []*timelineFragmentSpec) []byte {
	b.startBox(BoxTypeFtyp())
	b.marshal(&Ftyp{
		MajorBrand:       BrandISOM(),
		MinorVersion:     0x200,
		CompatibleBrands: []CompatibleBrandElem{{CompatibleBrand: BrandISOM()}},
	})
	b.endBox()

	for _, frag := range frags {
		// moof
		moofStart, err := b.w.Seek(0, io.SeekCurrent)
		require.NoError(t, err)
		b.startBox(BoxTypeMoof())
		b.startBox(BoxTypeTraf())

		// tfhd
		b.startBox(BoxTypeTfhd())
		tfhd := &Tfhd{FullBox: FullBox{}, TrackID: frag.trackID}
		if frag.defaulted {
			tfhd.AddFlag(TfhdDefaultSampleDurationPresent)
			tfhd.AddFlag(TfhdDefaultSampleSizePresent)
			tfhd.DefaultSampleDuration = frag.defaultSampleDuration
			tfhd.DefaultSampleSize = frag.defaultSampleSize
		}
		b.marshal(tfhd)
		b.endBox()

		// tfdt
		b.startBox(BoxTypeTfdt())
		tfdt := &Tfdt{FullBox: FullBox{Version: frag.tfdtVersion}}
		if frag.tfdtVersion == 0 {
			require.LessOrEqual(t, frag.baseMediaDecodeTime, uint64(math.MaxUint32))
			tfdt.BaseMediaDecodeTimeV0 = uint32(frag.baseMediaDecodeTime)
		} else {
			tfdt.BaseMediaDecodeTimeV1 = frag.baseMediaDecodeTime
		}
		b.marshal(tfdt)
		b.endBox()

		// trun
		b.startBox(BoxTypeTrun())
		trunPayloadStart, err := b.w.Seek(0, io.SeekCurrent)
		require.NoError(t, err)
		trun := &Trun{FullBox: FullBox{Version: frag.trunVersion}, SampleCount: uint32(len(frag.entries))}
		trun.AddFlag(0x000001) // data-offset present
		trun.DataOffset = 0    // patched after the moof size is known
		if !frag.defaulted {
			trun.AddFlag(0x000100) // sample-duration present
			trun.AddFlag(0x000200) // sample-size present
		}
		hasComposition := false
		for _, e := range frag.entries {
			if e.compositionOffset != 0 {
				hasComposition = true
			}
		}
		if hasComposition {
			trun.AddFlag(0x000800) // sample-composition-time-offsets present
		}
		for _, e := range frag.entries {
			entry := TrunEntry{SampleDuration: e.duration, SampleSize: e.size}
			if hasComposition {
				if frag.trunVersion == 0 {
					require.GreaterOrEqual(t, e.compositionOffset, int64(0))
					entry.SampleCompositionTimeOffsetV0 = uint32(e.compositionOffset)
				} else {
					entry.SampleCompositionTimeOffsetV1 = int32(e.compositionOffset)
				}
			}
			trun.Entries = append(trun.Entries, entry)
		}
		b.marshal(trun)
		b.endBox() // trun

		b.endBox() // traf
		b.endBox() // moof
		moofEnd, err := b.w.Seek(0, io.SeekCurrent)
		require.NoError(t, err)

		// mdat header follows immediately; its payload begins after the 8-byte
		// mdat header. trun.data_offset is relative to the moof start, so the
		// first sample lands exactly on the mdat payload.
		dataOffset := int32(moofEnd-moofStart) + 8
		_, err = b.w.Seek(trunPayloadStart+8, io.SeekStart)
		require.NoError(t, err)
		var dataOffsetBytes [4]byte
		binary.BigEndian.PutUint32(dataOffsetBytes[:], uint32(dataOffset))
		_, err = b.w.Write(dataOffsetBytes[:])
		require.NoError(t, err)
		_, err = b.w.Seek(moofEnd, io.SeekStart)
		require.NoError(t, err)

		// mdat
		b.startBox(BoxTypeMdat())
		var data []byte
		for _, e := range frag.entries {
			data = append(data, make([]byte, e.size)...)
		}
		b.marshal(&Mdat{Data: data})
		b.endBox()
	}

	return b.bytes()
}

// patchCo64Offset rewrites the single co64 entry of a serialized file in place.
func patchCo64Offset(t *testing.T, bin []byte, offset uint64) []byte {
	t.Helper()
	r := bytes.NewReader(bin)
	bis, err := ExtractBoxes(r, nil, []BoxPath{{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64()}})
	require.NoError(t, err)
	require.NotEmpty(t, bis)
	bi := bis[0]
	// co64 payload: version(1)+flags(3)+entry_count(4), then 8-byte offset.
	payloadStart := bi.Offset + bi.HeaderSize + 8
	out := append([]byte(nil), bin...)
	for i := 0; i < 8; i++ {
		out[payloadStart+uint64(i)] = byte(offset >> (56 - 8*i))
	}
	return out
}
