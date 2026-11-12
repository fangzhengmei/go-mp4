package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

const maxInt = int(^uint(0) >> 1)

type sttsSummaryEntry struct {
	count uint32
	delta uint32
}

type stscSummaryEntry struct {
	firstChunk      uint32
	samplesPerChunk uint32
}

func unmarshalProbeBox(r io.ReadSeeker, bi *BoxInfo, dst IBox) error {
	if _, err := bi.SeekToPayload(r); err != nil {
		return err
	}
	if err := checkPayloadSize(r, bi); err != nil {
		return err
	}
	box, _, err := UnmarshalAny(r, bi.Type, bi.Size-bi.HeaderSize, bi.Context)
	if err != nil {
		return err
	}
	switch src := box.(type) {
	case *Tkhd:
		*dst.(*Tkhd) = *src
	case *Elst:
		*dst.(*Elst) = *src
	case *Mdhd:
		*dst.(*Mdhd) = *src
	case *VisualSampleEntry:
		*dst.(*VisualSampleEntry) = *src
	case *AVCDecoderConfiguration:
		*dst.(*AVCDecoderConfiguration) = *src
	case *AudioSampleEntry:
		*dst.(*AudioSampleEntry) = *src
	case *Esds:
		*dst.(*Esds) = *src
	default:
		return fmt.Errorf("unexpected box type: got=%T want=%T", box, dst)
	}
	return nil
}

func readSeekerSize(r io.ReadSeeker) (int64, error) {
	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	if fileSize < 0 {
		return 0, errors.New("invalid file size")
	}
	return fileSize, nil
}

func seekProbeBoxPayload(r io.ReadSeeker, bi *BoxInfo) error {
	if _, err := bi.SeekToPayload(r); err != nil {
		return err
	}
	return checkPayloadSize(r, bi)
}

func readFullBoxHeader(r io.Reader, _ *BoxInfo) (uint8, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, err
	}
	return header[0], nil
}

func readProbeEntryCount(r io.Reader) (uint32, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf), nil
}

func checkProbeEntryCount(bi *BoxInfo, entryCount uint32, entrySize uint64) error {
	if entryCount != 0 && entrySize > math.MaxUint64/uint64(entryCount) {
		return errors.New("entry count overflow")
	}
	payloadSize := bi.Size - bi.HeaderSize
	const fullBoxHeaderSize = 4
	const entryCountSize = 4
	if entryCount != 0 && entrySize > math.MaxUint64/uint64(entryCount) {
		return errors.New("entry count overflow")
	}
	requiredSize := fullBoxHeaderSize + entryCountSize + uint64(entryCount)*entrySize
	if requiredSize > payloadSize {
		return fmt.Errorf("%s entry count exceeds box size: entryCount=%d, payloadSize=%d",
			bi.Type.String(), entryCount, payloadSize)
	}
	return nil
}

func probeEntryCountInt(entryCount uint32) (int, error) {
	if uint64(entryCount) > uint64(maxInt) {
		return 0, errors.New("entry count overflow")
	}
	return int(entryCount), nil
}

func checkedSampleCount(sum, count uint32) (uint32, error) {
	if count > math.MaxUint32-sum {
		return 0, errors.New("sample count overflow")
	}
	return sum + count, nil
}

func addUint64(a, b uint64) (uint64, bool) {
	if b > math.MaxUint64-a {
		return 0, true
	}
	return a + b, false
}

func checkedChunkSampleCount(sum, count uint64) (uint64, error) {
	if count > math.MaxUint64-sum {
		return 0, errors.New("chunk sample count overflow")
	}
	return sum + count, nil
}

func checkedSampleEnd(offset uint64, size uint32) (uint64, error) {
	if uint64(size) > math.MaxUint64-offset {
		return 0, errors.New("sample range overflow")
	}
	return offset + uint64(size), nil
}

func makeProbeChunks(chunkCount uint32) Chunks {
	chunks := make(Chunks, int(chunkCount))
	values := make([]Chunk, int(chunkCount))
	for i := range chunks {
		chunks[i] = &values[i]
	}
	return chunks
}

func readStcoChunks(r io.ReadSeeker, bi *BoxInfo, fileSize int64) (Chunks, error) {
	if err := seekProbeBoxPayload(r, bi); err != nil {
		return nil, err
	}
	version, err := readFullBoxHeader(r, bi)
	if err != nil {
		return nil, err
	}
	if version != 0 {
		return nil, ErrUnsupportedBoxVersion
	}
	entryCount, err := readProbeEntryCount(r)
	if err != nil {
		return nil, err
	}
	if err := checkProbeEntryCount(bi, entryCount, 4); err != nil {
		return nil, err
	}
	count, err := probeEntryCountInt(entryCount)
	if err != nil {
		return nil, err
	}
	chunks := makeProbeChunks(uint32(count))
	buf := make([]byte, 4)
	for i := range chunks {
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		offset := uint64(binary.BigEndian.Uint32(buf))
		if offset > uint64(fileSize) {
			return nil, fmt.Errorf("chunk offset exceeds file size: offset=%d, fileSize=%d", offset, fileSize)
		}
		chunks[i].DataOffset = offset
	}
	return chunks, nil
}

func readCo64Chunks(r io.ReadSeeker, bi *BoxInfo, fileSize int64) (Chunks, error) {
	if err := seekProbeBoxPayload(r, bi); err != nil {
		return nil, err
	}
	version, err := readFullBoxHeader(r, bi)
	if err != nil {
		return nil, err
	}
	if version != 0 {
		return nil, ErrUnsupportedBoxVersion
	}
	entryCount, err := readProbeEntryCount(r)
	if err != nil {
		return nil, err
	}
	if err := checkProbeEntryCount(bi, entryCount, 8); err != nil {
		return nil, err
	}
	count, err := probeEntryCountInt(entryCount)
	if err != nil {
		return nil, err
	}
	chunks := makeProbeChunks(uint32(count))
	buf := make([]byte, 8)
	for i := range chunks {
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		offset := binary.BigEndian.Uint64(buf)
		if offset > math.MaxInt64 || offset > uint64(fileSize) {
			return nil, fmt.Errorf("chunk offset exceeds file size: offset=%d, fileSize=%d", offset, fileSize)
		}
		chunks[i].DataOffset = offset
	}
	return chunks, nil
}

func readSttsSummary(r io.ReadSeeker, bi *BoxInfo) (uint32, []sttsSummaryEntry, error) {
	if err := seekProbeBoxPayload(r, bi); err != nil {
		return 0, nil, err
	}
	version, err := readFullBoxHeader(r, bi)
	if err != nil {
		return 0, nil, err
	}
	if version != 0 {
		return 0, nil, ErrUnsupportedBoxVersion
	}
	entryCount, err := readProbeEntryCount(r)
	if err != nil {
		return 0, nil, err
	}
	if err := checkProbeEntryCount(bi, entryCount, 8); err != nil {
		return 0, nil, err
	}
	count, err := probeEntryCountInt(entryCount)
	if err != nil {
		return 0, nil, err
	}
	entries := make([]sttsSummaryEntry, count)
	var sampleCount uint32
	buf := make([]byte, 8)
	for i := range entries {
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, nil, err
		}
		entries[i].count = binary.BigEndian.Uint32(buf[:4])
		entries[i].delta = binary.BigEndian.Uint32(buf[4:])
		sampleCount, err = checkedSampleCount(sampleCount, entries[i].count)
		if err != nil {
			return 0, nil, err
		}
	}
	return sampleCount, entries, nil
}

func readStscChunks(r io.ReadSeeker, bi *BoxInfo, chunks Chunks) (uint64, error) {
	if err := seekProbeBoxPayload(r, bi); err != nil {
		return 0, err
	}
	version, err := readFullBoxHeader(r, bi)
	if err != nil {
		return 0, err
	}
	if version != 0 {
		return 0, ErrUnsupportedBoxVersion
	}
	entryCount, err := readProbeEntryCount(r)
	if err != nil {
		return 0, err
	}
	if err := checkProbeEntryCount(bi, entryCount, 12); err != nil {
		return 0, err
	}
	count, err := probeEntryCountInt(entryCount)
	if err != nil {
		return 0, err
	}
	if len(chunks) == 0 {
		return 0, nil
	}
	if count == 0 {
		return 0, errors.New("stsc does not describe all chunks")
	}

	entries := make([]stscSummaryEntry, count)
	buf := make([]byte, 12)
	for i := range entries {
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, err
		}
		entries[i].firstChunk = binary.BigEndian.Uint32(buf[:4])
		entries[i].samplesPerChunk = binary.BigEndian.Uint32(buf[4:8])
		if entries[i].firstChunk == 0 || entries[i].firstChunk > uint32(len(chunks)) || entries[i].samplesPerChunk == 0 {
			return 0, errors.New("invalid stsc entry")
		}
		if i > 0 && entries[i].firstChunk <= entries[i-1].firstChunk {
			return 0, errors.New("invalid stsc entry")
		}
	}
	var totalSamples uint64
	for i := range entries {
		firstChunk := entries[i].firstChunk
		samplesPerChunk := entries[i].samplesPerChunk
		endChunk := uint32(len(chunks))
		if i+1 < len(entries) {
			endChunk = entries[i+1].firstChunk - 1
		}

		for chunkIndex := firstChunk - 1; chunkIndex < endChunk; chunkIndex++ {
			chunks[chunkIndex].SamplesPerChunk = samplesPerChunk
			totalSamples, err = checkedChunkSampleCount(totalSamples, uint64(samplesPerChunk))
			if err != nil {
				return 0, err
			}
		}
	}
	if entries[0].firstChunk != 1 {
		return 0, errors.New("stsc does not describe all chunks")
	}
	return totalSamples, nil
}

func readCttsSamples(r io.ReadSeeker, bi *BoxInfo, samples Samples) (uint32, error) {
	if err := seekProbeBoxPayload(r, bi); err != nil {
		return 0, err
	}
	version, err := readFullBoxHeader(r, bi)
	if err != nil {
		return 0, err
	}
	if version > 1 {
		return 0, ErrUnsupportedBoxVersion
	}
	entryCount, err := readProbeEntryCount(r)
	if err != nil {
		return 0, err
	}
	if err := checkProbeEntryCount(bi, entryCount, 8); err != nil {
		return 0, err
	}
	count, err := probeEntryCountInt(entryCount)
	if err != nil {
		return 0, err
	}

	var sampleCount uint32
	buf := make([]byte, 8)
	for i := 0; i < count; i++ {
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, err
		}
		count := binary.BigEndian.Uint32(buf[:4])
		sampleCount, err = checkedSampleCount(sampleCount, count)
		if err != nil {
			return 0, err
		}
		if sampleCount > uint32(len(samples)) {
			return 0, errors.New("ctts sample count exceeds stts sample count")
		}
		var offset int64
		if version == 0 {
			offset = int64(binary.BigEndian.Uint32(buf[4:]))
		} else {
			offset = int64(int32(binary.BigEndian.Uint32(buf[4:])))
		}
		first := sampleCount - count
		for sampleIndex := first; sampleIndex < sampleCount; sampleIndex++ {
			samples[sampleIndex].CompositionTimeOffset = offset
		}
	}
	return sampleCount, nil
}

func readStszSamples(r io.ReadSeeker, bi *BoxInfo, samples Samples) (uint32, error) {
	if err := seekProbeBoxPayload(r, bi); err != nil {
		return 0, err
	}
	version, err := readFullBoxHeader(r, bi)
	if err != nil {
		return 0, err
	}
	if version != 0 {
		return 0, ErrUnsupportedBoxVersion
	}
	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, err
	}
	constantSize := binary.BigEndian.Uint32(header[:4])
	sampleCount := binary.BigEndian.Uint32(header[4:])
	if sampleCount != uint32(len(samples)) {
		return 0, errors.New("stsz sample count does not match stts sample count")
	}
	if constantSize == 0 {
		if err := checkProbeEntryCount(bi, sampleCount, 4); err != nil {
			return 0, err
		}
		if _, err := probeEntryCountInt(sampleCount); err != nil {
			return 0, err
		}
	}

	if constantSize != 0 {
		for i := range samples {
			samples[i].Size = constantSize
		}
		return sampleCount, nil
	}
	if sampleCount == 0 {
		return 0, nil
	}

	buf := make([]byte, 4)
	for i := uint32(0); i < sampleCount; i++ {
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, err
		}
		samples[i].Size = binary.BigEndian.Uint32(buf)
	}
	return sampleCount, nil
}

func readStssSampleNumbers(r io.ReadSeeker, bi *BoxInfo, sampleCount uint32) ([]int, error) {
	if err := seekProbeBoxPayload(r, bi); err != nil {
		return nil, err
	}
	version, err := readFullBoxHeader(r, bi)
	if err != nil {
		return nil, err
	}
	if version != 0 {
		return nil, ErrUnsupportedBoxVersion
	}
	entryCount, err := readProbeEntryCount(r)
	if err != nil {
		return nil, err
	}
	if err := checkProbeEntryCount(bi, entryCount, 4); err != nil {
		return nil, err
	}
	count, err := probeEntryCountInt(entryCount)
	if err != nil {
		return nil, err
	}
	numbers := make([]int, count)
	var previous uint32
	buf := make([]byte, 4)
	for i := range numbers {
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		number := binary.BigEndian.Uint32(buf)
		if number == 0 || number > sampleCount || number <= previous {
			return nil, errors.New("invalid stss sample number")
		}
		if uint64(number-1) > uint64(maxInt) {
			return nil, errors.New("sample number overflow")
		}
		numbers[i] = int(number - 1)
		previous = number
	}
	return numbers, nil
}

func validateChunkSampleRanges(chunks Chunks, samples Samples, fileSize int64) error {
	var sampleIndex uint64
	for _, chunk := range chunks {
		if chunk.SamplesPerChunk == 0 {
			return errors.New("invalid stsc entry")
		}
		endSampleIndex, err := checkedChunkSampleCount(sampleIndex, uint64(chunk.SamplesPerChunk))
		if err != nil {
			return err
		}
		if endSampleIndex > uint64(len(samples)) {
			return errors.New("stsc sample count does not match stts sample count")
		}

		offset := chunk.DataOffset
		for ; sampleIndex < endSampleIndex; sampleIndex++ {
			nextOffset, err := checkedSampleEnd(offset, samples[sampleIndex].Size)
			if err != nil {
				return err
			}
			if nextOffset > uint64(fileSize) {
				return errors.New("sample range exceeds file size")
			}
			offset = nextOffset
		}
	}
	if sampleIndex != uint64(len(samples)) {
		return errors.New("stsc sample count does not match stts sample count")
	}
	return nil
}
