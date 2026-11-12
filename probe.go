package mp4

import (
	"bytes"
	"errors"
	"io"
	"math"

	"github.com/abema/go-mp4/internal/bitio"
)

type ProbeInfo struct {
	MajorBrand       [4]byte
	MinorVersion     uint32
	CompatibleBrands [][4]byte
	FastStart        bool
	Timescale        uint32
	Duration         uint64
	Tracks           Tracks
	Segments         Segments
}

// Deprecated: replace with ProbeInfo
type FraProbeInfo = ProbeInfo

type Tracks []*Track

// Deprecated: replace with Track
type TrackInfo = Track

type Track struct {
	TrackID        uint32
	Timescale      uint32
	Duration       uint64
	Codec          Codec
	Encrypted      bool
	EditList       EditList
	Samples        Samples
	Chunks         Chunks
	AVC            *AVCDecConfigInfo
	MP4A           *MP4AInfo
	// syncSamplesBox is retained so sync frame lookup can avoid rescanning media data.
	syncSamplesBox *BoxInfo
}

type Codec int

const (
	CodecUnknown Codec = iota
	CodecAVC1
	CodecMP4A
)

type EditList []*EditListEntry

type EditListEntry struct {
	MediaTime       int64
	SegmentDuration uint64
}

type Samples []*Sample

type Sample struct {
	Size                  uint32
	TimeDelta             uint32
	CompositionTimeOffset int64
}

type Chunks []*Chunk

type Chunk struct {
	DataOffset      uint64
	SamplesPerChunk uint32
}

type AVCDecConfigInfo struct {
	ConfigurationVersion uint8
	Profile              uint8
	ProfileCompatibility uint8
	Level                uint8
	LengthSize           uint16
	Width                uint16
	Height               uint16
}

type MP4AInfo struct {
	OTI          uint8
	AudOTI       uint8
	ChannelCount uint16
}

type Segments []*Segment

// Deprecated: replace with Segment
type SegmentInfo = Segment

type Segment struct {
	TrackID               uint32
	MoofOffset            uint64
	BaseMediaDecodeTime   uint64
	DefaultSampleDuration uint32
	SampleCount           uint32
	Duration              uint32
	CompositionTimeOffset int32
	Size                  uint32
}

// Probe probes MP4 file
func Probe(r io.ReadSeeker) (*ProbeInfo, error) {
	probeInfo := &ProbeInfo{
		Tracks:   make([]*Track, 0, 8),
		Segments: make([]*Segment, 0, 8),
	}
	bis, err := ExtractBoxes(r, nil, []BoxPath{
		{BoxTypeFtyp()},
		{BoxTypeMoov()},
		{BoxTypeMoov(), BoxTypeMvhd()},
		{BoxTypeMoov(), BoxTypeTrak()},
		{BoxTypeMoof()},
		{BoxTypeMdat()},
	})
	if err != nil {
		return nil, err
	}
	var mdatAppeared bool
	for _, bi := range bis {
		switch bi.Type {
		case BoxTypeFtyp():
			var ftyp Ftyp
			if _, err := bi.SeekToPayload(r); err != nil {
				return nil, err
			}
			if err := checkPayloadSize(r, bi); err != nil {
				return nil, err
			}
			if _, err := Unmarshal(r, bi.Size-bi.HeaderSize, &ftyp, bi.Context); err != nil {
				return nil, err
			}
			probeInfo.MajorBrand = ftyp.MajorBrand
			probeInfo.MinorVersion = ftyp.MinorVersion
			probeInfo.CompatibleBrands = make([][4]byte, 0, len(ftyp.CompatibleBrands))
			for _, entry := range ftyp.CompatibleBrands {
				probeInfo.CompatibleBrands = append(probeInfo.CompatibleBrands, entry.CompatibleBrand)
			}
		case BoxTypeMoov():
			probeInfo.FastStart = !mdatAppeared
		case BoxTypeMvhd():
			var mvhd Mvhd
			if _, err := bi.SeekToPayload(r); err != nil {
				return nil, err
			}
			if err := checkPayloadSize(r, bi); err != nil {
				return nil, err
			}
			if _, err := Unmarshal(r, bi.Size-bi.HeaderSize, &mvhd, bi.Context); err != nil {
				return nil, err
			}
			probeInfo.Timescale = mvhd.Timescale
			if mvhd.GetVersion() == 0 {
				probeInfo.Duration = uint64(mvhd.DurationV0)
			} else {
				probeInfo.Duration = mvhd.DurationV1
			}
		case BoxTypeTrak():
			track, err := probeTrak(r, bi)
			if err != nil {
				return nil, err
			}
			probeInfo.Tracks = append(probeInfo.Tracks, track)
		case BoxTypeMoof():
			segments, err := probeMoof(r, bi)
			if err != nil {
				return nil, err
			}
			probeInfo.Segments = append(probeInfo.Segments, segments...)
		case BoxTypeMdat():
			mdatAppeared = true
		}
	}
	return probeInfo, nil
}

// ProbeFra probes fragmented MP4 file
// Deprecated: replace with Probe
func ProbeFra(r io.ReadSeeker) (*FraProbeInfo, error) {
	probeInfo, err := Probe(r)
	return (*FraProbeInfo)(probeInfo), err
}

func probeTrak(r io.ReadSeeker, bi *BoxInfo) (*Track, error) {
	track := new(Track)

	boxPaths := []BoxPath{
		{BoxTypeTkhd()},
		{BoxTypeEdts(), BoxTypeElst()},
		{BoxTypeMdia(), BoxTypeMdhd()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeAvc1()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeAvc1(), BoxTypeAvcC()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeEncv()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeEncv(), BoxTypeAvcC()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeMp4a()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeMp4a(), BoxTypeEsds()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeMp4a(), BoxTypeWave(), BoxTypeEsds()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeEnca()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeEnca(), BoxTypeEsds()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStts()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCtts()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsc()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStss()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsz()},
	}
	bis, err := ExtractBoxes(r, bi, boxPaths)
	if err != nil {
		return nil, err
	}
	var tkhd *Tkhd
	var elst *Elst
	var mdhd *Mdhd
	var avc1 *VisualSampleEntry
	var avcC *AVCDecoderConfiguration
	var audioSampleEntry *AudioSampleEntry
	var esds *Esds
	var stcoInfo *BoxInfo
	var co64Info *BoxInfo
	var sttsInfo *BoxInfo
	var stscInfo *BoxInfo
	var cttsInfo *BoxInfo
	var stszInfo *BoxInfo
	for _, child := range bis {
		switch child.Type {
		case BoxTypeTkhd():
			tkhd = new(Tkhd)
			if err = unmarshalProbeBox(r, child, tkhd); err != nil {
				return nil, err
			}
		case BoxTypeElst():
			elst = new(Elst)
			if err = unmarshalProbeBox(r, child, elst); err != nil {
				return nil, err
			}
		case BoxTypeMdhd():
			mdhd = new(Mdhd)
			if err = unmarshalProbeBox(r, child, mdhd); err != nil {
				return nil, err
			}
		case BoxTypeAvc1():
			avc1 = new(VisualSampleEntry)
			if err = unmarshalProbeBox(r, child, avc1); err != nil {
				return nil, err
			}
			track.Codec = CodecAVC1
		case BoxTypeAvcC():
			avcC = new(AVCDecoderConfiguration)
			if err = unmarshalProbeBox(r, child, avcC); err != nil {
				return nil, err
			}
		case BoxTypeEncv():
			encv := new(VisualSampleEntry)
			if err = unmarshalProbeBox(r, child, encv); err != nil {
				return nil, err
			}
			track.Codec = CodecAVC1
			track.Encrypted = true
			avc1 = encv
		case BoxTypeMp4a():
			audioSampleEntry = new(AudioSampleEntry)
			if err = unmarshalProbeBox(r, child, audioSampleEntry); err != nil {
				return nil, err
			}
			track.Codec = CodecMP4A
		case BoxTypeEnca():
			enca := new(AudioSampleEntry)
			if err = unmarshalProbeBox(r, child, enca); err != nil {
				return nil, err
			}
			track.Codec = CodecMP4A
			track.Encrypted = true
			audioSampleEntry = enca
		case BoxTypeEsds():
			esds = new(Esds)
			if err = unmarshalProbeBox(r, child, esds); err != nil {
				return nil, err
			}
		case BoxTypeStco():
			if stcoInfo != nil {
				return nil, errors.New("duplicate stco box")
			}
			stcoInfo = child
		case BoxTypeStts():
			if sttsInfo != nil {
				return nil, errors.New("duplicate stts box")
			}
			sttsInfo = child
		case BoxTypeStsc():
			if stscInfo != nil {
				return nil, errors.New("duplicate stsc box")
			}
			stscInfo = child
		case BoxTypeCtts():
			if cttsInfo != nil {
				return nil, errors.New("duplicate ctts box")
			}
			cttsInfo = child
		case BoxTypeStsz():
			if stszInfo != nil {
				return nil, errors.New("duplicate stsz box")
			}
			stszInfo = child
		case BoxTypeCo64():
			if co64Info != nil {
				return nil, errors.New("duplicate co64 box")
			}
			co64Info = child
		case BoxTypeStss():
			if track.syncSamplesBox != nil {
				return nil, errors.New("duplicate stss box")
			}
			track.syncSamplesBox = child
		}
	}

	if tkhd == nil {
		return nil, errors.New("tkhd box not found")
	}
	track.TrackID = tkhd.TrackID

	if elst != nil {
		editList := make([]*EditListEntry, 0, len(elst.Entries))
		for i := range elst.Entries {
			editList = append(editList, &EditListEntry{
				MediaTime:       elst.GetMediaTime(i),
				SegmentDuration: elst.GetSegmentDuration(i),
			})
		}
		track.EditList = editList
	}

	if mdhd == nil {
		return nil, errors.New("mdhd box not found")
	}
	track.Timescale = mdhd.Timescale
	track.Duration = mdhd.GetDuration()

	if avc1 != nil && avcC != nil {
		track.AVC = &AVCDecConfigInfo{
			ConfigurationVersion: avcC.ConfigurationVersion,
			Profile:              avcC.Profile,
			ProfileCompatibility: avcC.ProfileCompatibility,
			Level:                avcC.Level,
			LengthSize:           uint16(avcC.LengthSizeMinusOne) + 1,
			Width:                avc1.Width,
			Height:               avc1.Height,
		}
	}

	if audioSampleEntry != nil && esds != nil {
		oti, audOTI, err := detectAACProfile(esds)
		if err != nil {
			return nil, err
		}
		track.MP4A = &MP4AInfo{
			OTI:          oti,
			AudOTI:       audOTI,
			ChannelCount: audioSampleEntry.ChannelCount,
		}
	}

	if (stcoInfo == nil) == (co64Info == nil) {
		return nil, errors.New("stco/co64 box not found")
	}

	if sttsInfo == nil {
		return nil, errors.New("stts box not found")
	}

	if stscInfo == nil {
		return nil, errors.New("stsc box not found")
	}

	fileSize, err := readSeekerSize(r)
	if err != nil {
		return nil, err
	}

	if stcoInfo != nil {
		track.Chunks, err = readStcoChunks(r, stcoInfo, fileSize)
	} else {
		track.Chunks, err = readCo64Chunks(r, co64Info, fileSize)
	}
	if err != nil {
		return nil, err
	}

	sampleCount, sttsEntries, err := readSttsSummary(r, sttsInfo)
	if err != nil {
		return nil, err
	}

	chunkSampleCount, err := readStscChunks(r, stscInfo, track.Chunks)
	if err != nil {
		return nil, err
	}
	if chunkSampleCount != uint64(sampleCount) {
		return nil, errors.New("stsc sample count does not match stts sample count")
	}
	sampleCountInt, err := probeEntryCountInt(sampleCount)
	if err != nil {
		return nil, err
	}

	track.Samples = make(Samples, sampleCountInt)
	sampleValues := make([]Sample, sampleCountInt)
	for i := range track.Samples {
		track.Samples[i] = &sampleValues[i]
	}

	var sampleIndex uint32
	for _, entry := range sttsEntries {
		for i := uint32(0); i < entry.count; i++ {
			track.Samples[sampleIndex].TimeDelta = entry.delta
			sampleIndex++
		}
	}

	if cttsInfo != nil {
		count, err := readCttsSamples(r, cttsInfo, track.Samples)
		if err != nil {
			return nil, err
		}
		if count != sampleCount {
			return nil, errors.New("ctts sample count does not match stts sample count")
		}
	}

	if stszInfo != nil {
		count, err := readStszSamples(r, stszInfo, track.Samples)
		if err != nil {
			return nil, err
		}
		if count != sampleCount {
			return nil, errors.New("stsz sample count does not match stts sample count")
		}
	}
	if err := validateChunkSampleRanges(track.Chunks, track.Samples, fileSize); err != nil {
		return nil, err
	}

	return track, nil
}

func detectAACProfile(esds *Esds) (oti, audOTI uint8, err error) {
	configDscr := findDescriptorByTag(esds.Descriptors, DecoderConfigDescrTag)
	if configDscr == nil || configDscr.DecoderConfigDescriptor == nil {
		return 0, 0, nil
	}
	if configDscr.DecoderConfigDescriptor.ObjectTypeIndication != 0x40 {
		return configDscr.DecoderConfigDescriptor.ObjectTypeIndication, 0, nil
	}

	specificDscr := findDescriptorByTag(esds.Descriptors, DecSpecificInfoTag)
	if specificDscr == nil {
		return 0, 0, errors.New("DecoderSpecificationInfoDescriptor not found")
	}

	r := bitio.NewReader(bytes.NewReader(specificDscr.Data))
	remaining := len(specificDscr.Data) * 8

	// audio object type
	audioObjectType, read, err := getAudioObjectType(r)
	if err != nil {
		return 0, 0, err
	}
	remaining -= read

	// sampling frequency index
	samplingFrequencyIndex, err := r.ReadBits(4)
	if err != nil {
		return 0, 0, err
	}
	remaining -= 4
	if samplingFrequencyIndex[0] == 0x0f {
		if _, err = r.ReadBits(24); err != nil {
			return 0, 0, err
		}
		remaining -= 24
	}

	if audioObjectType == 2 && remaining >= 20 {
		if _, err = r.ReadBits(4); err != nil {
			return 0, 0, err
		}
		remaining -= 4
		syncExtensionType, err := r.ReadBits(11)
		if err != nil {
			return 0, 0, err
		}
		remaining -= 11
		if syncExtensionType[0] == 0x2 && syncExtensionType[1] == 0xb7 {
			extAudioObjectType, _, err := getAudioObjectType(r)
			if err != nil {
				return 0, 0, err
			}
			if extAudioObjectType == 5 || extAudioObjectType == 22 {
				sbr, err := r.ReadBits(1)
				if err != nil {
					return 0, 0, err
				}
				remaining--
				if sbr[0] != 0 {
					if extAudioObjectType == 5 {
						sfi, err := r.ReadBits(4)
						if err != nil {
							return 0, 0, err
						}
						remaining -= 4
						if sfi[0] == 0xf {
							if _, err := r.ReadBits(24); err != nil {
								return 0, 0, err
							}
							remaining -= 24
						}
						if remaining >= 12 {
							syncExtensionType, err := r.ReadBits(11)
							if err != nil {
								return 0, 0, err
							}
							if syncExtensionType[0] == 0x5 && syncExtensionType[1] == 0x48 {
								ps, err := r.ReadBits(1)
								if err != nil {
									return 0, 0, err
								}
								if ps[0] != 0 {
									return 0x40, 29, nil
								}
							}
						}
					}
					return 0x40, 5, nil
				}
			}
		}
	}
	return 0x40, audioObjectType, nil
}

func findDescriptorByTag(dscrs []Descriptor, tag int8) *Descriptor {
	for _, dscr := range dscrs {
		if dscr.Tag == tag {
			return &dscr
		}
	}
	return nil
}

func getAudioObjectType(r bitio.Reader) (byte, int, error) {
	audioObjectType, err := r.ReadBits(5)
	if err != nil {
		return 0, 0, err
	}
	if audioObjectType[0] != 0x1f {
		return audioObjectType[0], 5, nil
	}
	audioObjectType, err = r.ReadBits(6)
	if err != nil {
		return 0, 0, err
	}
	return audioObjectType[0] + 32, 11, nil
}

// probeMoof returns a segment for each track fragment of the moof box.
func probeMoof(r io.ReadSeeker, bi *BoxInfo) ([]*Segment, error) {
	trafs, err := ExtractBox(r, bi, BoxPath{BoxTypeTraf()})
	if err != nil {
		return nil, err
	}

	segments := make([]*Segment, 0, len(trafs))
	for _, traf := range trafs {
		segment, err := probeTraf(r, traf)
		if err != nil {
			return nil, err
		}
		segment.MoofOffset = bi.Offset
		segments = append(segments, segment)
	}
	return segments, nil
}

func probeTraf(r io.ReadSeeker, bi *BoxInfo) (*Segment, error) {
	bips, err := ExtractBoxesWithPayload(r, bi, []BoxPath{
		{BoxTypeTfhd()},
		{BoxTypeTfdt()},
		{BoxTypeTrun()},
	})
	if err != nil {
		return nil, err
	}

	var tfhd *Tfhd
	var tfdt *Tfdt
	var trun *Trun

	segment := &Segment{}
	for _, bip := range bips {
		switch bip.Info.Type {
		case BoxTypeTfhd():
			tfhd = bip.Payload.(*Tfhd)
		case BoxTypeTfdt():
			tfdt = bip.Payload.(*Tfdt)
		case BoxTypeTrun():
			trun = bip.Payload.(*Trun)
		}
	}

	if tfhd == nil {
		return nil, errors.New("tfhd not found")
	}
	segment.TrackID = tfhd.TrackID
	segment.DefaultSampleDuration = tfhd.DefaultSampleDuration

	if tfdt != nil {
		if tfdt.Version == 0 {
			segment.BaseMediaDecodeTime = uint64(tfdt.BaseMediaDecodeTimeV0)
		} else {
			segment.BaseMediaDecodeTime = tfdt.BaseMediaDecodeTimeV1
		}
	}

	if trun != nil {
		segment.SampleCount = trun.SampleCount

		if trun.CheckFlag(0x000100) {
			segment.Duration = 0
			for ei := range trun.Entries {
				segment.Duration += trun.Entries[ei].SampleDuration
			}
		} else {
			segment.Duration = tfhd.DefaultSampleDuration * segment.SampleCount
		}

		if trun.CheckFlag(0x000200) {
			segment.Size = 0
			for ei := range trun.Entries {
				segment.Size += trun.Entries[ei].SampleSize
			}
		} else {
			segment.Size = tfhd.DefaultSampleSize * segment.SampleCount
		}

		var duration uint32
		for ei := range trun.Entries {
			offset := int32(duration) + int32(trun.GetSampleCompositionTimeOffset(ei))
			if ei == 0 || offset < segment.CompositionTimeOffset {
				segment.CompositionTimeOffset = offset
			}
			if trun.CheckFlag(0x000100) {
				duration += trun.Entries[ei].SampleDuration
			} else {
				duration += tfhd.DefaultSampleDuration
			}
		}
	}

	return segment, nil
}

func FindIDRFrames(r io.ReadSeeker, trackInfo *TrackInfo) ([]int, error) {
	if trackInfo.AVC == nil {
		return nil, nil
	}
	if trackInfo.syncSamplesBox != nil {
		return readStssSampleNumbers(r, trackInfo.syncSamplesBox, uint32(len(trackInfo.Samples)))
	}
	lengthSize := uint32(trackInfo.AVC.LengthSize)

	var si int
	idxs := make([]int, 0, 8)
	nalHeaderBuffer := make([]byte, lengthSize+1)
	for _, chunk := range trackInfo.Chunks {
		end, overflow := addUint64(uint64(si), uint64(chunk.SamplesPerChunk))
		if overflow || end > math.MaxInt64 {
			return nil, errors.New("sample index overflow")
		}
		dataOffset := chunk.DataOffset
		for ; uint64(si) < end && si < len(trackInfo.Samples); si++ {
			sample := trackInfo.Samples[si]
			if sample.Size == 0 {
				continue
			}
			nalOffset := uint64(0)
			for {
				nextHeaderOffset, overflow := addUint64(nalOffset, uint64(lengthSize)+1)
				if overflow || nextHeaderOffset > uint64(sample.Size) {
					break
				}
				seekOffset, overflow := addUint64(dataOffset, nalOffset)
				if overflow || seekOffset > math.MaxInt64 {
					return nil, errors.New("NAL unit offset overflow")
				}
				if _, err := r.Seek(int64(seekOffset), io.SeekStart); err != nil {
					return nil, err
				}
				if _, err := io.ReadFull(r, nalHeaderBuffer); err != nil {
					return nil, err
				}
				var length uint32
				for i := 0; i < int(lengthSize); i++ {
					length = (length << 8) + uint32(nalHeaderBuffer[i])
				}
				if length == 0 {
					return nil, errors.New("invalid NAL unit length")
				}
				nalHeader := nalHeaderBuffer[lengthSize]
				nalType := nalHeader & 0x1f
				if nalType == 5 {
					idxs = append(idxs, si)
					break
				}
				nextOffset, overflow := addUint64(nalOffset, uint64(lengthSize))
				if overflow {
					return nil, errors.New("NAL unit offset overflow")
				}
				nalOffset, overflow = addUint64(nextOffset, uint64(length))
				if overflow {
					return nil, errors.New("NAL unit offset overflow")
				}
			}
			nextDataOffset, err := checkedSampleEnd(dataOffset, sample.Size)
			if err != nil {
				return nil, err
			}
			dataOffset = nextDataOffset
		}
	}
	return idxs, nil
}

func (samples Samples) GetBitrate(timescale uint32) uint64 {
	var totalSize uint64
	var totalDuration uint64
	for _, sample := range samples {
		totalSize += uint64(sample.Size)
		totalDuration += uint64(sample.TimeDelta)
	}
	if totalDuration == 0 {
		return 0
	}
	return 8 * totalSize * uint64(timescale) / totalDuration
}

func (samples Samples) GetMaxBitrate(timescale uint32, timeDelta uint64) uint64 {
	if timeDelta == 0 {
		return 0
	}
	var maxBitrate uint64
	var size uint64
	var duration uint64
	var begin int
	var end int
	for end < len(samples) {
		for {
			size += uint64(samples[end].Size)
			duration += uint64(samples[end].TimeDelta)
			end++
			if duration >= timeDelta || end == len(samples) {
				break
			}
		}
		if duration != 0 {
			bitrate := 8 * size * uint64(timescale) / duration
			if bitrate > maxBitrate {
				maxBitrate = bitrate
			}
		}
		for {
			size -= uint64(samples[begin].Size)
			duration -= uint64(samples[begin].TimeDelta)
			begin++
			if duration < timeDelta {
				break
			}
		}
	}
	return maxBitrate
}

func (segments Segments) GetBitrate(trackID uint32, timescale uint32) uint64 {
	var totalSize uint64
	var totalDuration uint64
	for _, segment := range segments {
		if segment.TrackID == trackID {
			totalSize += uint64(segment.Size)
			totalDuration += uint64(segment.Duration)
		}
	}
	if totalDuration == 0 {
		return 0
	}
	return 8 * totalSize * uint64(timescale) / totalDuration
}

func (segments Segments) GetMaxBitrate(trackID uint32, timescale uint32) uint64 {
	var maxBitrate uint64
	for _, segment := range segments {
		if segment.TrackID == trackID && segment.Duration != 0 {
			bitrate := 8 * uint64(segment.Size) * uint64(timescale) / uint64(segment.Duration)
			if bitrate > maxBitrate {
				maxBitrate = bitrate
			}
		}
	}
	return maxBitrate
}
