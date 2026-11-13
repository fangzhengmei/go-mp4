package mp4

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failingReadSeeker struct {
	*bytes.Reader
	failAt int64
	err    error
}

func (r *failingReadSeeker) Read(p []byte) (int, error) {
	offset, _ := r.Seek(0, io.SeekCurrent)
	if offset >= r.failAt {
		return 0, r.err
	}
	if int64(len(p)) > r.failAt-offset {
		p = p[:r.failAt-offset]
	}
	n, err := r.Reader.Read(p)
	if err == io.EOF && n > 0 && offset+int64(n) == r.failAt {
		return n, nil
	}
	return n, err
}

func boxHeader(size uint32, typ string) []byte {
	return append([]byte{byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size)}, typ...)
}

func largeBoxHeader(typ string, size uint64) []byte {
	b := boxHeader(1, typ)
	return append(b, byte(size>>56), byte(size>>48), byte(size>>40), byte(size>>32),
		byte(size>>24), byte(size>>16), byte(size>>8), byte(size))
}

func TestBoxErrorIncludesPathAndAbsoluteOffsets(t *testing.T) {
	// moov contains a mvhd whose declared size extends beyond moov.
	moovPayload := append(boxHeader(32, "mvhd"), make([]byte, 24)...)
	b := append(boxHeader(uint32(8+len(moovPayload)), "moov"), moovPayload...)

	_, err := ReadBoxStructure(bytes.NewReader(b), func(h *ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == BoxTypeMvhd() {
			_, expandErr := h.Expand()
			return nil, expandErr
		}
		return h.Expand()
	})

	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.Equal(t, BoxPath{BoxTypeMoov(), BoxTypeMvhd()}, boxErr.Path)
	assert.Equal(t, BoxTypeMvhd(), boxErr.BoxInfo.Type)
	assert.Equal(t, uint64(8), boxErr.BoxInfo.Offset)
	assert.Equal(t, uint64(16), boxErr.Offset)
	assert.True(t, errors.Is(err, io.EOF))
	assert.False(t, errors.Is(err, io.ErrUnexpectedEOF))

	var readerErr *ReaderError
	assert.False(t, errors.As(err, &readerErr))
}

func TestReadBoxInfoTruncatedExtendedSizeReportsRealOffsetAndRestoresCursor(t *testing.T) {
	b := largeBoxHeader("mdat", 64)[:12]
	r := bytes.NewReader(b)

	bi, err := ReadBoxInfo(r)
	require.Nil(t, bi)
	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.Equal(t, uint64(12), boxErr.Offset)
	assert.Equal(t, BoxTypeMdat(), boxErr.BoxInfo.Type)
	assert.True(t, errors.Is(err, io.ErrUnexpectedEOF))
	assert.Contains(t, err.Error(), "path=/")

	offset, seekErr := r.Seek(0, io.SeekCurrent)
	require.NoError(t, seekErr)
	assert.Equal(t, int64(0), offset)

	// A retry starts at the same box instead of reusing the failed EOF position.
	bi, err = ReadBoxInfo(r)
	require.Nil(t, bi)
	assert.Error(t, err)
	offset, seekErr = r.Seek(0, io.SeekCurrent)
	require.NoError(t, seekErr)
	assert.Equal(t, int64(0), offset)
}

func TestTruncatedExtendedSizeThroughStructureReportsActualTypePath(t *testing.T) {
	b := largeBoxHeader("mdat", 64)[:12]

	_, err := ReadBoxStructure(bytes.NewReader(b), func(h *ReadHandle) (interface{}, error) {
		return nil, nil
	})

	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.Equal(t, BoxPath{BoxTypeMdat()}, boxErr.Path)
	assert.Equal(t, uint64(12), boxErr.Offset)
	assert.True(t, errors.Is(err, io.ErrUnexpectedEOF))
}

func TestReadBoxInfoTruncatedHeaderReportsRealOffsetAndRestoresCursor(t *testing.T) {
	b := boxHeader(32, "mdat")[:7]
	r := bytes.NewReader(b)

	bi, err := ReadBoxInfo(r)
	require.Nil(t, bi)
	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.Equal(t, uint64(7), boxErr.Offset)
	assert.True(t, errors.Is(err, io.ErrUnexpectedEOF))

	offset, seekErr := r.Seek(0, io.SeekCurrent)
	require.NoError(t, seekErr)
	assert.Equal(t, int64(0), offset)
}

func TestReadBoxInfoEmptyReaderKeepsCleanEOFType(t *testing.T) {
	r := bytes.NewReader(nil)

	bi, err := ReadBoxInfo(r)
	require.Nil(t, bi)
	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.Equal(t, uint64(0), boxErr.Offset)
	assert.True(t, errors.Is(err, io.EOF))
	assert.False(t, errors.Is(err, io.ErrUnexpectedEOF))
}

func TestReadBoxInfoZeroExtendedSizeReportsBoxStartAndRestoresCursor(t *testing.T) {
	b := largeBoxHeader("mdat", 0)
	r := bytes.NewReader(b)

	bi, err := ReadBoxInfo(r)
	require.Nil(t, bi)
	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.Equal(t, uint64(0), boxErr.Offset)
	assert.Equal(t, uint64(0), boxErr.BoxInfo.Offset)
	assert.Contains(t, err.Error(), "invalid size")

	offset, seekErr := r.Seek(0, io.SeekCurrent)
	require.NoError(t, seekErr)
	assert.Equal(t, int64(0), offset)
}

func TestReadBoxInfoSmallerThanHeaderReportsBoxStart(t *testing.T) {
	b := boxHeader(0, "mdat")
	b[3] = 4
	r := bytes.NewReader(b)

	bi, err := ReadBoxInfo(r)
	require.Nil(t, bi)
	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.Equal(t, uint64(0), boxErr.Offset)
	assert.Contains(t, err.Error(), "invalid size")

	offset, seekErr := r.Seek(0, io.SeekCurrent)
	require.NoError(t, seekErr)
	assert.Equal(t, int64(0), offset)
}

func TestReadBoxInfoZeroSizeExtendsToEOF(t *testing.T) {
	b := append(boxHeader(0, "mdat"), 1, 2, 3, 4)
	r := bytes.NewReader(b)

	bi, err := ReadBoxInfo(r)
	require.NoError(t, err)
	assert.Equal(t, &BoxInfo{
		Offset:      0,
		Size:        12,
		HeaderSize:  8,
		Type:        BoxTypeMdat(),
		ExtendToEOF: true,
	}, bi)

	pos, err := r.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(8), pos)
}

func TestNestedZeroSizeBoxKeepsPayloadBoundaryForRetry(t *testing.T) {
	child := append(boxHeader(0, "zzzz"), 1, 2, 3)
	moov := append(boxHeader(uint32(8+len(child)), "moov"), child...)

	var handle *ReadHandle
	_, err := ReadBoxStructure(bytes.NewReader(moov), func(h *ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == StrToBoxType("zzzz") {
			handle = h
		}
		return h.Expand()
	})
	require.NoError(t, err)
	require.NotNil(t, handle)
	assert.True(t, handle.BoxInfo.ExtendToEOF)

	buf := bytes.NewBuffer(nil)
	n, err := handle.ReadData(buf)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), n)
	assert.Equal(t, []byte{1, 2, 3}, buf.Bytes())
}

func TestUnknownTruncatedBoxReportsEndOffset(t *testing.T) {
	// An unknown leaf box claims 32 bytes but has only 16 bytes of file data.
	b := boxHeader(32, "zzzz")
	b = append(b, make([]byte, 8)...)

	_, err := ReadBoxStructure(bytes.NewReader(b), func(h *ReadHandle) (interface{}, error) {
		return nil, nil
	})

	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.Equal(t, BoxPath{StrToBoxType("zzzz")}, boxErr.Path)
	assert.Equal(t, uint64(0), boxErr.BoxInfo.Offset)
	assert.Equal(t, uint64(32), boxErr.Offset)
	assert.True(t, errors.Is(err, io.ErrUnexpectedEOF))
}

func TestNestedUnknownTruncatedBoxReportsPathAndOffset(t *testing.T) {
	child := append(boxHeader(32, "zzzz"), make([]byte, 8)...)
	moov := append(boxHeader(uint32(8+len(child)), "moov"), child...)

	_, err := ReadBoxStructure(bytes.NewReader(moov), func(h *ReadHandle) (interface{}, error) {
		return h.Expand()
	})

	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.Equal(t, BoxPath{BoxTypeMoov(), StrToBoxType("zzzz")}, boxErr.Path)
	assert.Equal(t, uint64(8), boxErr.BoxInfo.Offset)
	assert.Equal(t, uint64(8), boxErr.Offset)
	assert.True(t, errors.Is(err, io.ErrUnexpectedEOF))
}

func TestDecodeFailureIncludesBoxPathAndCanRetry(t *testing.T) {
	// A full box needs at least four payload bytes; this stsz has only one.
	stsz := boxHeader(13, "stsz")
	stsz = append(stsz, 0, 0, 0, 0, 0)
	b := append(boxHeader(uint32(8+len(stsz)), "moov"), stsz...)

	var handle *ReadHandle
	_, err := ReadBoxStructure(bytes.NewReader(b), func(h *ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == BoxTypeStsz() {
			handle = h
			_, _, decodeErr := h.ReadPayload()
			return nil, decodeErr
		}
		return h.Expand()
	})

	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.Equal(t, BoxPath{BoxTypeMoov(), BoxTypeStsz()}, boxErr.Path)
	assert.Equal(t, uint64(8), boxErr.BoxInfo.Offset)
	assert.NotNil(t, handle)

	offset, seekErr := handle.ReadData(io.Discard)
	require.NoError(t, seekErr)
	assert.Equal(t, uint64(5), offset)
}

func TestOriginalSentinelErrorsRemainDetectable(t *testing.T) {
	b := boxHeader(8, "xxxx")
	_, err := ReadBoxStructure(bytes.NewReader(b), func(h *ReadHandle) (interface{}, error) {
		_, _, decodeErr := h.ReadPayload()
		return nil, decodeErr
	})

	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.True(t, errors.Is(err, ErrBoxInfoNotFound))
	assert.Equal(t, BoxPath{StrToBoxType("xxxx")}, boxErr.Path)
}

func TestExternalReaderErrorIsDistinguishedFromBoxError(t *testing.T) {
	b := append(boxHeader(32, "mdat"), make([]byte, 24)...)
	externalErr := errors.New("storage unavailable")
	r := &failingReadSeeker{Reader: bytes.NewReader(b), failAt: 8, err: externalErr}

	_, err := ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		return h.ReadData(io.Discard)
	})

	var readerErr *ReaderError
	require.True(t, errors.As(err, &readerErr))
	assert.True(t, errors.Is(err, externalErr))
	assert.Equal(t, uint64(8), readerErr.Offset)

	var boxErr *BoxError
	assert.False(t, errors.As(err, &boxErr))
}

func TestFindIDRFramesTruncationReportsMdatAndAbsoluteOffset(t *testing.T) {
	original, err := os.ReadFile("./testdata/sample.mp4")
	require.NoError(t, err)

	info, err := Probe(bytes.NewReader(original))
	require.NoError(t, err)

	truncated := bytes.NewReader(original[:100])
	_, err = FindIDRFrames(truncated, info.Tracks[0])

	var boxErr *BoxError
	require.True(t, errors.As(err, &boxErr))
	assert.Equal(t, BoxPath{BoxTypeMdat()}, boxErr.Path)
	assert.Equal(t, BoxTypeMdat(), boxErr.BoxInfo.Type)
	assert.Equal(t, uint64(40), boxErr.BoxInfo.Offset)
	assert.Greater(t, boxErr.Offset, uint64(40))
	assert.True(t, errors.Is(err, io.ErrUnexpectedEOF))
}

func TestFindIDRFramesExternalReaderErrorRemainsReaderError(t *testing.T) {
	original, err := os.ReadFile("./testdata/sample.mp4")
	require.NoError(t, err)

	info, err := Probe(bytes.NewReader(original))
	require.NoError(t, err)

	externalErr := errors.New("storage unavailable")
	r := &failingReadSeeker{
		Reader: bytes.NewReader(original),
		failAt: int64(info.Tracks[0].Chunks[0].DataOffset),
		err:    externalErr,
	}
	_, err = FindIDRFrames(r, info.Tracks[0])

	var readerErr *ReaderError
	require.True(t, errors.As(err, &readerErr))
	assert.True(t, errors.Is(err, externalErr))
	assert.GreaterOrEqual(t, readerErr.Offset, info.Tracks[0].Chunks[0].DataOffset)

	var boxErr *BoxError
	assert.False(t, errors.As(err, &boxErr))
}

func TestReadBoxStructureNormalFileResultIsUnchanged(t *testing.T) {
	paths := make([]BoxPath, 0, 64)
	offsets := make([]uint64, 0, 64)
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	_, err = ReadBoxStructure(f, func(h *ReadHandle) (interface{}, error) {
		paths = append(paths, append(BoxPath(nil), h.Path...))
		offsets = append(offsets, h.BoxInfo.Offset)
		return h.Expand()
	})
	require.NoError(t, err)
	assert.Len(t, paths, 57)
	assert.Equal(t, BoxPath{BoxTypeFtyp()}, paths[0])
	assert.Equal(t, uint64(0), offsets[0])
	assert.Equal(t, BoxPath{BoxTypeMoov(), BoxTypeUdta(), StrToBoxType("loci")}, paths[56])
	assert.Equal(t, uint64(8243), offsets[56])
}

func TestBoxErrorStringContainsTraceablePath(t *testing.T) {
	err := &BoxError{
		Path:    BoxPath{BoxTypeMoov(), BoxTypeTrak()},
		BoxInfo: BoxInfo{Offset: 24, Type: BoxTypeTrak()},
		Offset:  40,
		Err:     io.ErrUnexpectedEOF,
	}
	assert.Equal(t, "box error: path=/moov/trak offset=24 failure_offset=40: unexpected EOF", fmt.Sprint(err))
}
