package mp4

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type failAtWriteSeeker struct {
	buf       bytes.Buffer
	failAfter int
	pos       int64
}

func (f *failAtWriteSeeker) Write(p []byte) (int, error) {
	if f.failAfter >= 0 && f.buf.Len()+len(p) > f.failAfter {
		n := f.failAfter - f.buf.Len()
		if n < 0 {
			n = 0
		}
		f.buf.Write(p[:n])
		f.pos += int64(n)
		return n, errors.New("forced write failure")
	}
	n, err := f.buf.Write(p)
	f.pos += int64(n)
	return n, err
}

func (f *failAtWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.pos = offset
	case io.SeekCurrent:
		f.pos += offset
	case io.SeekEnd:
		f.pos = int64(f.buf.Len()) + offset
	}
	return f.pos, nil
}

func (f *failAtWriteSeeker) Truncate(size int64) error {
	if size != 0 {
		return errors.New("unsupported truncate size")
	}
	f.buf.Reset()
	f.pos = 0
	return nil
}

func buildRewriteTestContainer(t *testing.T, chunkOffset uint32) []byte {
	t.Helper()

	var output memoryWriteSeeker
	w := NewWriter(&output)
	mediaPayload := []byte("audio-and-video-payload")

	writeBox := func(boxType BoxType, payload []byte) {
		t.Helper()
		_, err := w.StartBox(&BoxInfo{Type: boxType, Size: SmallHeaderSize + uint64(len(payload)), HeaderSize: SmallHeaderSize})
		require.NoError(t, err)
		_, err = w.Write(payload)
		require.NoError(t, err)
		_, err = w.EndBox()
		require.NoError(t, err)
	}

	writeBox(BoxTypeFtyp(), []byte("isom0000"))
	writeBox(BoxTypeFree(), []byte("preserve-free-box"))

	_, err := w.StartBox(&BoxInfo{Type: BoxTypeMoov()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeTrak()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdia()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMinf()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeStbl()})
	require.NoError(t, err)

	writeUnknown := func(boxType BoxType, payload []byte) {
		t.Helper()
		writeBox(boxType, payload)
	}
	writeUnknown(StrToBoxType("zzzz"), []byte("unknown-before"))

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeStco()})
	require.NoError(t, err)
	_, err = Marshal(w, &Stco{EntryCount: 1, ChunkOffset: []uint32{chunkOffset}}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	writeUnknown(StrToBoxType("yyyy"), []byte("unknown-after"))

	for i := 0; i < 5; i++ {
		_, err = w.EndBox()
		require.NoError(t, err)
	}

	writeBox(BoxTypeFree(), []byte("trailing-free-box"))
	writeBox(BoxTypeMdat(), mediaPayload)
	return output.data
}

type memoryWriteSeeker struct {
	data []byte
	pos  int64
}

func (w *memoryWriteSeeker) Write(p []byte) (int, error) {
	end := int(w.pos) + len(p)
	if end > len(w.data) {
		w.data = append(w.data, make([]byte, end-len(w.data))...)
	}
	copy(w.data[w.pos:end], p)
	w.pos = int64(end)
	return len(p), nil
}

func (w *memoryWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = offset
	case io.SeekCurrent:
		w.pos += offset
	case io.SeekEnd:
		w.pos = int64(len(w.data)) + offset
	}
	return w.pos, nil
}

func (w *memoryWriteSeeker) Truncate(size int64) error {
	if size != 0 {
		return errors.New("unsupported truncate size")
	}
	w.data = nil
	w.pos = 0
	return nil
}

func topLevelBoxes(t *testing.T, data []byte) []BoxInfo {
	t.Helper()
	boxes, err := ExtractBox(bytes.NewReader(data), nil, BoxPath{BoxTypeAny()})
	require.NoError(t, err)
	infos := make([]BoxInfo, 0, len(boxes))
	for _, box := range boxes {
		infos = append(infos, *box)
	}
	return infos
}

func extractFirstPayload(t *testing.T, data []byte, path BoxPath) IBox {
	t.Helper()
	payloads, err := ExtractBoxWithPayload(bytes.NewReader(data), nil, path)
	require.NoError(t, err)
	require.NotEmpty(t, payloads)
	return payloads[0].Payload
}

func boxPayload(data []byte, info BoxInfo) []byte {
	return data[info.Offset+info.HeaderSize : info.Offset+info.Size]
}

func mustExtract(t *testing.T, data []byte, path BoxPath) []BoxInfo {
	t.Helper()
	boxes, err := ExtractBox(bytes.NewReader(data), nil, path)
	require.NoError(t, err)
	require.Len(t, boxes, 1)
	infos := make([]BoxInfo, len(boxes))
	for i := range boxes {
		infos[i] = *boxes[i]
	}
	return infos
}

func TestRewriteMetadataMovesMoovAndPreservesPayload(t *testing.T) {
	input := buildRewriteTestContainer(t, 169)
	originalBoxes := topLevelBoxes(t, input)
	originalMdat := originalBoxes[4]
	originalMedia := append([]byte(nil), boxPayload(input, originalMdat)...)

	for _, tc := range []struct {
		name     string
		position MetadataPosition
		moovAt   int
	}{
		{name: "beginning", position: MetadataAtBeginning, moovAt: 0},
		{name: "end", position: MetadataAtEnd, moovAt: len(originalBoxes) - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output memoryWriteSeeker
			require.NoError(t, RewriteMetadata(bytes.NewReader(input), &output, tc.position))

			data := output.data
			boxes := topLevelBoxes(t, data)
			require.Len(t, boxes, len(originalBoxes))
			require.Equal(t, BoxTypeMoov(), boxes[tc.moovAt].Type)
			if tc.position == MetadataAtEnd {
				require.Equal(t, BoxTypeFtyp(), boxes[0].Type)
			}

			var mdat *BoxInfo
			for i := range boxes {
				if boxes[i].Type == BoxTypeMdat() {
					mdat = &boxes[i]
					break
				}
			}
			require.NotNil(t, mdat)
			require.Equal(t, originalMedia, boxPayload(data, *mdat))

			stco := extractFirstPayload(t, data, BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()}).(*Stco)
			require.Equal(t, []uint32{uint32(mdat.Offset)}, stco.ChunkOffset)
			unknownBefore := mustExtract(t, data, BoxPath{
				BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), StrToBoxType("zzzz"),
			})
			require.Equal(t, []byte("unknown-before"), boxPayload(data, unknownBefore[0]))
			unknownAfter := mustExtract(t, data, BoxPath{
				BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), StrToBoxType("yyyy"),
			})
			require.Equal(t, []byte("unknown-after"), boxPayload(data, unknownAfter[0]))

			// Non-moov boxes, including unknown content and free-space boxes,
			// keep their exact bytes and relative top-level order apart from
			// the explicitly relocated moov box.
			inputWithoutMoov := bytes.Join([][]byte{
				input[originalBoxes[0].Offset : originalBoxes[0].Offset+originalBoxes[0].Size],
				input[originalBoxes[1].Offset : originalBoxes[1].Offset+originalBoxes[1].Size],
				input[originalBoxes[3].Offset : originalBoxes[3].Offset+originalBoxes[3].Size],
				input[originalBoxes[4].Offset : originalBoxes[4].Offset+originalBoxes[4].Size],
			}, nil)
			var outputWithoutMoov [][]byte
			for _, box := range boxes {
				if box.Type != BoxTypeMoov() {
					outputWithoutMoov = append(outputWithoutMoov, data[box.Offset:box.Offset+box.Size])
				}
			}
			require.Equal(t, inputWithoutMoov, bytes.Join(outputWithoutMoov, nil))
		})
	}
}

func TestRewriteMetadataConvertsStcoToCo64AndRecalculatesSizes(t *testing.T) {
	input := buildRewriteTestContainer(t, 169)
	plan, err := buildRewritePlan(bytes.NewReader(input), MetadataAtBeginning)
	require.NoError(t, err)

	// Replace the top-level layout with a non-overlapping synthetic data box
	// large enough that moving moov ahead of it makes the chunk target exceed
	// uint32. This exercises layout and size convergence without allocating a
	// multi-gigabyte fixture.
	largeSize := uint64(math.MaxUint32) - plan.moovSize - 4
	syntheticMdat := &rewriteRootBox{
		info: BoxInfo{Offset: largeSize, Size: 48, HeaderSize: SmallHeaderSize, Type: BoxTypeMdat()},
	}
	plan.rootBoxes = []*rewriteRootBox{
		{info: BoxInfo{Offset: 0, Size: largeSize, HeaderSize: SmallHeaderSize, Type: BoxTypeFree()}},
		syntheticMdat,
		{info: BoxInfo{Offset: plan.moov.info.Offset, Size: plan.moov.info.Size, HeaderSize: plan.moov.info.HeaderSize, Type: BoxTypeMoov()}, moov: plan.moov},
	}
	for _, chunk := range plan.collectChunkBoxes() {
		chunk.original = []uint64{largeSize + syntheticMdat.info.HeaderSize}
	}
	require.NoError(t, plan.resolveOffsets())

	chunks := plan.collectChunkBoxes()
	require.Len(t, chunks, 1)
	require.True(t, chunks[0].useLargeSize)
	require.Greater(t, chunks[0].offsets()[0], uint64(math.MaxUint32))
	co64Boxes, err := ExtractBoxes(bytes.NewReader(input), nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64()},
	})
	require.NoError(t, err)
	require.Empty(t, co64Boxes)

	// The co64 payload is four bytes per entry larger than stco. This delta
	// must be reflected in every parent box and the moov box itself.
	parentTypes := []BoxType{BoxTypeStbl(), BoxTypeMinf(), BoxTypeMdia(), BoxTypeTrak(), BoxTypeMoov()}
	originalStco, err := ExtractBox(bytes.NewReader(input), nil, BoxPath{
		BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco(),
	})
	require.NoError(t, err)
	require.Len(t, originalStco, 1)
	require.Equal(t, originalStco[0].Size+4, rewrittenNodeSize(plan.moov, BoxTypeStco()))
	for _, boxType := range parentTypes {
		path := BoxPath{BoxTypeMoov()}
		if boxType == BoxTypeMoov() {
			old, err := ExtractBox(bytes.NewReader(input), nil, path)
			require.NoError(t, err)
			require.Len(t, old, 1)
			require.Equal(t, old[0].Size+4, rewrittenNodeSize(plan.moov, boxType))
			continue
		}
		switch boxType {
		case BoxTypeTrak():
			path = append(path, BoxTypeTrak())
		case BoxTypeMdia():
			path = append(path, BoxTypeTrak(), BoxTypeMdia())
		case BoxTypeMinf():
			path = append(path, BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf())
		case BoxTypeStbl():
			path = append(path, BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl())
		}
		old, err := ExtractBox(bytes.NewReader(input), nil, path)
		require.NoError(t, err)
		require.Len(t, old, 1)
		require.Equal(t, old[0].Size+4, rewrittenNodeSize(plan.moov, boxType))
	}

	require.Equal(t, calculateMoovSize(plan.moov), plan.moovSize)
}

func TestRewriteMetadataPreservesExistingCo64(t *testing.T) {
	input := buildRewriteTestContainer(t, 169)
	plan, err := buildRewritePlan(bytes.NewReader(input), MetadataAtBeginning)
	require.NoError(t, err)
	chunk := plan.collectChunkBoxes()[0]
	chunk.info.Type = BoxTypeCo64()
	chunk.co64 = &Co64{
		FullBox:    chunk.stco.FullBox,
		EntryCount: chunk.stco.EntryCount,
		ChunkOffset: []uint64{
			uint64(chunk.stco.ChunkOffset[0]),
		},
	}
	chunk.stco = nil
	chunk.useLargeSize = true
	chunk.original = append([]uint64(nil), chunk.co64.ChunkOffset...)
	require.NoError(t, plan.resolveOffsets())

	var output memoryWriteSeeker
	require.NoError(t, writeRewrite(bytes.NewReader(input), &output, plan))
	payload := extractFirstPayload(t, output.data, BoxPath{
		BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64(),
	}).(*Co64)
	mdat := topLevelBoxes(t, output.data)
	var mdatInfo *BoxInfo
	for i := range mdat {
		if mdat[i].Type == BoxTypeMdat() {
			mdatInfo = &mdat[i]
		}
	}
	require.NotNil(t, mdatInfo)
	require.Equal(t, []uint64{mdatInfo.Offset}, payload.ChunkOffset)
}

func rewrittenNodeSize(node *rewriteBoxNode, boxType BoxType) uint64 {
	if node.info.Type == boxType {
		return calculateMoovSize(node)
	}
	for _, child := range node.children {
		if size := rewrittenNodeSize(child, boxType); size != 0 {
			return size
		}
	}
	return 0
}

func TestRewriteMetadataFailureDoesNotLeaveCompleteOutput(t *testing.T) {
	input := buildRewriteTestContainer(t, 169)
	output := &failAtWriteSeeker{failAfter: 20}
	err := RewriteMetadata(bytes.NewReader(input), output, MetadataAtBeginning)
	require.Error(t, err)
	require.NotEqual(t, len(input), output.buf.Len())
	require.Less(t, output.buf.Len(), len(input))
}

func TestRewriteMetadataRejectsInvalidPosition(t *testing.T) {
	input := buildRewriteTestContainer(t, 169)
	var output memoryWriteSeeker
	err := RewriteMetadata(bytes.NewReader(input), &output, MetadataPosition(2))
	require.Error(t, err)
	require.Empty(t, output.data)
}

func TestRewriteMetadataOnSampleContainer(t *testing.T) {
	input, err := os.ReadFile("testdata/sample.mp4")
	require.NoError(t, err)
	originalBoxes := topLevelBoxes(t, input)
	var originalMdat *BoxInfo
	for i := range originalBoxes {
		if originalBoxes[i].Type == BoxTypeMdat() {
			originalMdat = &originalBoxes[i]
			break
		}
	}
	require.NotNil(t, originalMdat)
	originalMedia := append([]byte(nil), boxPayload(input, *originalMdat)...)

	for _, position := range []MetadataPosition{MetadataAtBeginning, MetadataAtEnd} {
		var output memoryWriteSeeker
		require.NoError(t, RewriteMetadata(bytes.NewReader(input), &output, position))
		data := output.data
		require.Len(t, data, len(input))

		boxes := topLevelBoxes(t, data)
		var mdat *BoxInfo
		for i := range boxes {
			if boxes[i].Type == BoxTypeMdat() {
				mdat = &boxes[i]
				break
			}
		}
		require.NotNil(t, mdat)
		require.Equal(t, originalMedia, boxPayload(data, *mdat))

		stcoBoxes, err := ExtractBoxesWithPayload(bytes.NewReader(data), nil, []BoxPath{
			{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()},
			{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64()},
		})
		require.NoError(t, err)
		require.NotEmpty(t, stcoBoxes)
		for _, box := range stcoBoxes {
			var offsets []uint64
			switch payload := box.Payload.(type) {
			case *Stco:
				for _, offset := range payload.ChunkOffset {
					offsets = append(offsets, uint64(offset))
				}
			case *Co64:
				offsets = payload.ChunkOffset
			}
			for _, offset := range offsets {
				require.GreaterOrEqual(t, offset, mdat.Offset+mdat.HeaderSize)
				require.Less(t, offset, mdat.Offset+mdat.Size)
			}
		}
	}
}

func TestRewriteMetadataFileReplacesOutputOnlyAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.mp4")
	outputPath := filepath.Join(dir, "output.mp4")
	require.NoError(t, os.WriteFile(inputPath, buildRewriteTestContainer(t, 169), 0600))
	originalOutput := []byte("existing complete output")
	require.NoError(t, os.WriteFile(outputPath, originalOutput, 0600))

	require.NoError(t, RewriteMetadataFile(inputPath, outputPath, MetadataAtBeginning))
	rewritten, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	require.NotEqual(t, originalOutput, rewritten)
	require.Equal(t, BoxTypeMoov(), topLevelBoxes(t, rewritten)[0].Type)

	samePath := filepath.Join(dir, "same.mp4")
	require.NoError(t, os.WriteFile(samePath, buildRewriteTestContainer(t, 169), 0600))
	require.NoError(t, RewriteMetadataFile(samePath, samePath, MetadataAtBeginning))
	sameData, err := os.ReadFile(samePath)
	require.NoError(t, err)
	require.Equal(t, BoxTypeMoov(), topLevelBoxes(t, sameData)[0].Type)

	matches, err := filepath.Glob(filepath.Join(dir, ".go-mp4-rewrite-*"))
	require.NoError(t, err)
	require.Empty(t, matches)

	badInput := filepath.Join(dir, "missing-moov.mp4")
	require.NoError(t, os.WriteFile(badInput, []byte("not an mp4"), 0600))
	err = RewriteMetadataFile(badInput, outputPath, MetadataAtBeginning)
	require.Error(t, err)
	preserved, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	require.Equal(t, rewritten, preserved)

	matches, err = filepath.Glob(filepath.Join(dir, ".go-mp4-rewrite-*"))
	require.NoError(t, err)
	require.Empty(t, matches)
}
