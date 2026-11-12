package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/src-d/go-billy.v4/memfs"
)

func TestCopyPayload(t *testing.T) {
	src := bytes.NewReader([]byte{
		0x00, 0x00, 0x00, 0x10, 'u', 'n', 'k', 'n',
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	})
	bi, err := ReadBoxInfo(src)
	require.NoError(t, err)

	output, err := memfs.New().Create("output.mp4")
	require.NoError(t, err)
	w := NewWriter(output)

	n, err := w.CopyPayload(src, bi)
	require.NoError(t, err)
	assert.Equal(t, bi.Size-bi.HeaderSize, n)

	if _, err := output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(output)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}, got)
}

func TestCopyPayloadTruncated(t *testing.T) {
	// the header claims 32 bytes, but only the header exists
	src := bytes.NewReader([]byte{
		0x00, 0x00, 0x00, 0x20, 'u', 'n', 'k', 'n',
	})
	bi, err := ReadBoxInfo(src)
	require.NoError(t, err)

	output, err := memfs.New().Create("output.mp4")
	require.NoError(t, err)
	w := NewWriter(output)

	n, err := w.CopyPayload(src, bi)
	assert.Error(t, err)
	assert.Equal(t, uint64(0), n)
}

func TestEndBoxUpgradesToLargeHeader(t *testing.T) {
	output, err := memfs.New().Create("output.mp4")
	require.NoError(t, err)
	w := NewWriter(output)

	// parent box starts with an 8-byte small header
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMoov()})
	require.NoError(t, err)

	child, err := w.StartBox(&BoxInfo{Type: BoxTypeFree()})
	require.NoError(t, err)

	// force a final child size larger than uint32.
	// child final content: 16-byte header + 4GiB payload
	_, err = w.Seek(int64(child.Offset)+(1<<32)+7, io.SeekStart)
	require.NoError(t, err)
	_, err = w.Write([]byte{0xAA})
	require.NoError(t, err)

	childBI, err := w.EndBox()
	require.NoError(t, err)
	assert.Equal(t, uint64(LargeHeaderSize), childBI.HeaderSize)
	assert.Equal(t, uint64(1<<32)+16, childBI.Size)

	parentBI, err := w.EndBox()
	require.NoError(t, err)
	assert.Equal(t, uint64(LargeHeaderSize), parentBI.HeaderSize)

	// validate the serialized headers
	rf := output
	_, err = rf.Seek(0, io.SeekStart)
	require.NoError(t, err)
	parentHeader := make([]byte, 16)
	_, err = io.ReadFull(rf, parentHeader)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), binary.BigEndian.Uint32(parentHeader[0:4]))
	assert.Equal(t, BoxTypeMoov(), BoxType{parentHeader[4], parentHeader[5], parentHeader[6], parentHeader[7]})
	assert.Equal(t, parentBI.Size, binary.BigEndian.Uint64(parentHeader[8:16]))

	childHeader := make([]byte, 16)
	_, err = io.ReadFull(rf, childHeader)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), binary.BigEndian.Uint32(childHeader[0:4]))
	assert.Equal(t, BoxTypeFree(), BoxType{childHeader[4], childHeader[5], childHeader[6], childHeader[7]})
	assert.Equal(t, uint64(1<<32)+16, binary.BigEndian.Uint64(childHeader[8:16]))
}

func TestEndBoxUpgradesNestedLargeHeaders(t *testing.T) {
	output, err := memfs.New().Create("nested.mp4")
	require.NoError(t, err)
	w := NewWriter(output)

	// moov > trak > free, where the leaf box grows beyond the 32-bit range.
	// All three headers must become 16-byte large headers and their offsets
	// must stay contiguous.
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMoov()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeTrak()})
	require.NoError(t, err)
	leaf, err := w.StartBox(&BoxInfo{Type: BoxTypeFree()})
	require.NoError(t, err)

	_, err = w.Seek(int64(leaf.Offset)+(1<<32)+7, io.SeekStart)
	require.NoError(t, err)
	_, err = w.Write([]byte{0xCC})
	require.NoError(t, err)

	leafBI, err := w.EndBox()
	require.NoError(t, err)
	trakBI, err := w.EndBox()
	require.NoError(t, err)
	moovBI, err := w.EndBox()
	require.NoError(t, err)

	assert.Equal(t, uint64(32), leafBI.Offset)
	assert.Equal(t, uint64(LargeHeaderSize), leafBI.HeaderSize)
	assert.Equal(t, uint64(1<<32)+16, leafBI.Size)

	assert.Equal(t, uint64(16), trakBI.Offset)
	assert.Equal(t, uint64(LargeHeaderSize), trakBI.HeaderSize)
	assert.Equal(t, uint64(1<<32)+32, trakBI.Size)

	assert.Equal(t, uint64(0), moovBI.Offset)
	assert.Equal(t, uint64(LargeHeaderSize), moovBI.HeaderSize)
	assert.Equal(t, uint64(1<<32)+48, moovBI.Size)

	// verify the serialized headers are contiguous and self-consistent
	_, err = output.Seek(0, io.SeekStart)
	require.NoError(t, err)
	expected := []struct {
		typ  BoxType
		size uint64
	}{
		{BoxTypeMoov(), moovBI.Size},
		{BoxTypeTrak(), trakBI.Size},
		{BoxTypeFree(), leafBI.Size},
	}
	for _, c := range expected {
		header := make([]byte, 16)
		_, err = io.ReadFull(output, header)
		require.NoError(t, err)
		assert.Equal(t, uint32(1), binary.BigEndian.Uint32(header[0:4]))
		assert.Equal(t, c.typ, BoxType{header[4], header[5], header[6], header[7]})
		assert.Equal(t, c.size, binary.BigEndian.Uint64(header[8:16]))
	}
}

// writeOnlySeeker implements io.WriteSeeker but not io.Reader
type writeOnlySeeker struct {
	pos int64
	end int64
}

func (w *writeOnlySeeker) Write(p []byte) (int, error) {
	w.pos += int64(len(p))
	if w.pos > w.end {
		w.end = w.pos
	}
	return len(p), nil
}

func (w *writeOnlySeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = offset
	case io.SeekCurrent:
		w.pos += offset
	case io.SeekEnd:
		w.pos = w.end + offset
	}
	return w.pos, nil
}

func TestEndBoxLargeHeaderWithoutReadableWriter(t *testing.T) {
	w := NewWriter(&writeOnlySeeker{})
	bi, err := w.StartBox(&BoxInfo{Type: BoxTypeFree()})
	require.NoError(t, err)
	_, err = w.Seek(int64(bi.Offset)+(1<<32)+7, io.SeekStart)
	require.NoError(t, err)
	_, err = w.Write([]byte{0xAA})
	require.NoError(t, err)

	_, err = w.EndBox()
	assert.Error(t, err)
}
