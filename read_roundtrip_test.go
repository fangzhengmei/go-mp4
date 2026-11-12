package mp4

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noSeekEndReader wraps bytes.Reader and rejects SeekEnd, emulating stream
// backed readers for which end-of-file positioning is unavailable.
type noSeekEndReader struct {
	*bytes.Reader
}

func (r *noSeekEndReader) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return 0, errors.New("SeekEnd is not supported")
	}
	return r.Reader.Seek(offset, whence)
}

func TestReadBoxStructureTruncatedBox(t *testing.T) {
	// moov declares 100 bytes but the input ends directly after its header.
	in := []byte{
		0x00, 0x00, 0x00, 0x64, 'm', 'o', 'o', 'v',
	}
	_, err := ReadBoxStructure(bytes.NewReader(in), func(h *ReadHandle) (interface{}, error) {
		return h.Expand()
	})
	require.Error(t, err)
}

func TestReadBoxStructureTruncatedUnknownBox(t *testing.T) {
	// an unknown box claims 32 bytes but only its 8-byte header exists
	in := []byte{
		0x00, 0x00, 0x00, 0x20, 'z', 'z', 'z', 'z',
	}
	_, err := ReadBoxStructure(bytes.NewReader(in), func(h *ReadHandle) (interface{}, error) {
		if !h.BoxInfo.IsSupportedType() {
			buf := bytes.NewBuffer(nil)
			_, err := h.ReadData(buf)
			return nil, err
		}
		return h.Expand()
	})
	require.Error(t, err)
}

func TestReadBoxStructureTrailingBytesInContainer(t *testing.T) {
	// moov contains one complete mvhd followed by 4 bytes that cannot form
	// another box header: a truncated input must produce an error.
	in := []byte{
		0x00, 0x00, 0x00, 0x0c, 'm', 'o', 'o', 'v',
		0x00, 0x00, 0x00, 0x08, 'm', 'v', 'h', 'd',
		0xaa, 0xbb, 0xcc, 0xdd,
	}
	_, err := ReadBoxStructure(bytes.NewReader(in), func(h *ReadHandle) (interface{}, error) {
		return h.Expand()
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected EOF")
}

func TestReadBoxStructureZeroSizeNestedBox(t *testing.T) {
	// A zero size is only legal at the top level. Inside a container it must
	// be rejected instead of looping forever.
	in := []byte{
		0x00, 0x00, 0x00, 0x20, 'm', 'o', 'o', 'v',
		0x00, 0x00, 0x00, 0x00, 'z', 'z', 'z', 'z',
		0x00, 0x00, 0x00, 0x08, 'm', 'v', 'h', 'd',
		0x00, 0x00, 0x00, 0x08, 'f', 'r', 'e', 'e',
	}
	_, err := ReadBoxStructure(bytes.NewReader(in), func(h *ReadHandle) (interface{}, error) {
		if !h.BoxInfo.IsSupportedType() {
			return nil, nil
		}
		return h.Expand()
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid zero-size box in container")
}

func TestReadBoxStructureZeroSizeTopLevelBox(t *testing.T) {
	// ftyp plus a zero-size moov that extends to the end of the file.
	in := []byte{
		0x00, 0x00, 0x00, 0x10, 'f', 't', 'y', 'p',
		'i', 's', 'o', 'm', 0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00, 'm', 'o', 'o', 'v',
	}
	var types []BoxType
	vals, err := ReadBoxStructure(bytes.NewReader(in), func(h *ReadHandle) (interface{}, error) {
		types = append(types, h.BoxInfo.Type)
		if h.BoxInfo.Type == BoxTypeMoov() {
			assert.True(t, h.BoxInfo.ExtendToEOF)
		}
		return nil, nil
	})
	require.NoError(t, err)
	assert.Len(t, vals, 2)
	assert.Equal(t, []BoxType{BoxTypeFtyp(), BoxTypeMoov()}, types)
}

// limitedEndReader reports a fake (smaller) end-of-file position for SeekEnd,
// emulating a zero-size box that ends inside its own header bytes.
type limitedEndReader struct {
	*bytes.Reader
	fakeEnd int64
}

func (r *limitedEndReader) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return r.fakeEnd, nil
	}
	return r.Reader.Seek(offset, whence)
}

func TestReadBoxInfoZeroSizeEmptyFile(t *testing.T) {
	// The header is fully readable, but the reported file end lies inside it,
	// so the resolved box is smaller than a header and must be rejected.
	r := &limitedEndReader{
		Reader:  bytes.NewReader([]byte{0x00, 0x00, 0x00, 0x00, 'z', 'z', 'z', 'z'}),
		fakeEnd: 4,
	}
	bi, err := ReadBoxInfo(r)
	require.Error(t, err)
	assert.Nil(t, bi)
}

func TestCheckPayloadSizeWithoutSeekEnd(t *testing.T) {
	// A well-formed box must remain readable even when the underlying reader
	// cannot seek to the end of the file.
	in := []byte{
		0x00, 0x00, 0x00, 0x0c, 'f', 't', 'y', 'p',
		'i', 's', 'o', 'm',
	}
	r := &noSeekEndReader{Reader: bytes.NewReader(in)}
	bi, err := ReadBoxInfo(r)
	require.NoError(t, err)
	assert.NoError(t, checkPayloadSize(r, bi))

	// An over-declared size is still detected through the payload read.
	r2 := &noSeekEndReader{Reader: bytes.NewReader([]byte{
		0x00, 0x00, 0x00, 0x20, 'z', 'z', 'z', 'z',
	})}
	bi2, err := ReadBoxInfo(r2)
	require.NoError(t, err)
	discard := &devNull{}
	_, err = io.CopyN(discard, r2, int64(bi2.Size-bi2.HeaderSize))
	assert.Error(t, err)
}

type devNull struct{}

func (*devNull) Write(p []byte) (int, error) { return len(p), nil }
