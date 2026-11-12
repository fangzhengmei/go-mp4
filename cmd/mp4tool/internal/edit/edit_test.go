package edit

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/abema/go-mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func box(t *testing.T, boxType string, payload []byte) []byte {
	t.Helper()
	require.Len(t, boxType, 4)
	buf := bytes.NewBuffer(nil)
	require.NoError(t, binary.Write(buf, binary.BigEndian, uint32(8+len(payload))))
	buf.WriteString(boxType)
	buf.Write(payload)
	return buf.Bytes()
}

func largeBox(t *testing.T, boxType string, payload []byte) []byte {
	t.Helper()
	require.Len(t, boxType, 4)
	buf := bytes.NewBuffer(nil)
	require.NoError(t, binary.Write(buf, binary.BigEndian, uint32(1)))
	buf.WriteString(boxType)
	require.NoError(t, binary.Write(buf, binary.BigEndian, uint64(16+uint64(len(payload)))))
	buf.Write(payload)
	return buf.Bytes()
}

func runEdit(t *testing.T, input []byte) ([]byte, error) {
	t.Helper()
	dir, err := ioutil.TempDir("", "mp4-edit")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	inPath := filepath.Join(dir, "in.mp4")
	outPath := filepath.Join(dir, "out.mp4")
	require.NoError(t, ioutil.WriteFile(inPath, input, 0600))

	err = editFile(inPath, outPath)
	if err != nil {
		return nil, err
	}
	out, readErr := ioutil.ReadFile(outPath)
	require.NoError(t, readErr)
	return out, nil
}

func resetEditConfig(t *testing.T) {
	t.Helper()
	prev := config
	config = Config{values: Values{BaseMediaDecodeTime: UNoValue}}
	t.Cleanup(func() { config = prev })
}

// Unknown boxes interleaved with known boxes inside an ordinary container
// must be copied byte-for-byte and keep their relative ordering when a known
// sibling box is edited.
func TestEditPreservesUnknownBoxesAndOrder(t *testing.T) {
	resetEditConfig(t)
	unknown1 := box(t, "zzzz", []byte{0x01, 0x02, 0x03, 0x04})
	unknown2 := box(t, "yyyy", []byte{0x0a, 0x0b})
	mvhd := box(t, "mvhd", make([]byte, 100))
	moov := box(t, "moov", bytes.Join([][]byte{unknown1, mvhd, unknown2}, nil))
	input := append(box(t, "ftyp", append([]byte("isom"), 0, 0, 0, 1)), moov...)

	output, err := runEdit(t, input)
	require.NoError(t, err)

	// nothing was edited, so the whole file must be identical
	assert.Equal(t, input, output)

	// the unknown box payload must still be present at the same place
	bi := mustFindBox(t, bytes.NewReader(output), nil,
		mp4.BoxPath{mp4.BoxTypeMoov(), mp4.StrToBoxType("zzzz")})
	assert.Equal(t, uint64(12), bi.Size)
}

// The payload of an unknown box is opaque even when its bytes look like valid
// nested box headers: it must never be recursed into.
func TestEditDoesNotRecurseUnknownBoxContent(t *testing.T) {
	resetEditConfig(t)
	fakeChild := box(t, "aaaa", nil)
	unknown := box(t, "zzzz", fakeChild)
	moov := box(t, "moov", unknown)
	input := append(box(t, "ftyp", append([]byte("isom"), 0, 0, 0, 1)), moov...)

	var visited []string
	r := bytes.NewReader(input)
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		visited = append(visited, h.BoxInfo.Type.String())
		if !h.BoxInfo.IsSupportedType() {
			return nil, nil
		}
		return h.Expand()
	})
	require.NoError(t, err)
	// "aaaa" must not appear: zzzz is opaque
	assert.NotContains(t, visited, "aaaa")

	output, err := runEdit(t, input)
	require.NoError(t, err)
	assert.Equal(t, input, output)
}

// A 64-bit large-size header on an unknown box is preserved verbatim.
func TestEditPreservesLargeSizeHeader(t *testing.T) {
	resetEditConfig(t)
	unknown := largeBox(t, "zzzz", []byte{1, 2, 3, 4})
	moov := box(t, "moov", unknown)
	input := append(box(t, "ftyp", append([]byte("isom"), 0, 0, 0, 1)), moov...)

	output, err := runEdit(t, input)
	require.NoError(t, err)
	assert.Equal(t, input, output)
}

// The media data box is copied as an opaque blob, even though its payload is
// not a sequence of nested boxes.
func TestEditPreservesMdatOpaque(t *testing.T) {
	resetEditConfig(t)
	ftyp := box(t, "ftyp", append([]byte("isom"), 0, 0, 0, 1))
	// payload bytes that would be misparsed if mdat were treated as a
	// container: they look like a box header but belong to media data.
	mdat := box(t, "mdat", []byte{
		0x00, 0x00, 0x00, 0x08, 'f', 'a', 'k', 'e',
		0xDE, 0xAD,
	})
	input := append(ftyp, mdat...)

	output, err := runEdit(t, input)
	require.NoError(t, err)
	assert.Equal(t, input, output)
}

// Fragmented containers (styp/moof/traf with tfdt) are edited while unknown
// boxes in the same fragment are preserved.
func TestEditFragmentedContainer(t *testing.T) {
	resetEditConfig(t)
	unknown := box(t, "zzzz", []byte{9, 8, 7, 6})
	// tfdt version 1: FullBox(4) + BaseMediaDecodeTimeV1(8)
	tfdtPayload := append([]byte{1, 0, 0, 0}, make([]byte, 8)...)
	tfdt := box(t, "tfdt", tfdtPayload)
	traf := box(t, "traf", append(unknown, tfdt...))
	mfhd := box(t, "mfhd", []byte{0, 0, 0, 0, 0, 0, 0, 1})
	moof := box(t, "moof", append(mfhd, traf...))
	styp := box(t, "styp", append([]byte("msid"), 0, 0, 0, 0))
	mdat := box(t, "mdat", []byte{0xde, 0xad, 0xbe, 0xef})
	input := bytes.Join([][]byte{styp, moof, mdat}, nil)

	config = Config{values: Values{BaseMediaDecodeTime: 12345}}
	output, err := runEdit(t, input)
	require.NoError(t, err)

	// unknown box is still there with the same bytes and order (before tfdt)
	unkBI := mustFindBox(t, bytes.NewReader(output), nil,
		mp4.BoxPath{mp4.BoxTypeMoof(), mp4.BoxTypeTraf(), mp4.StrToBoxType("zzzz")})
	assert.Equal(t, uint64(12), unkBI.Size)

	// tfdt now carries the edited value
	tfdtBoxes := mustFindBoxesWithPayload(t, bytes.NewReader(output), nil,
		mp4.BoxPath{mp4.BoxTypeMoof(), mp4.BoxTypeTraf(), mp4.BoxTypeTfdt()})
	require.Len(t, tfdtBoxes, 1)
	tfdtBox := tfdtBoxes[0].Payload.(*mp4.Tfdt)
	assert.Equal(t, uint64(12345), tfdtBox.BaseMediaDecodeTimeV1)

	// parent sizes remain self-consistent
	moofBI := mustFindBox(t, bytes.NewReader(output), nil, mp4.BoxPath{mp4.BoxTypeMoof()})
	assertConsistentContainer(t, bytes.NewReader(output), moofBI)
}

// A top-level zero-size box (extends to EOF) is rewritten with an explicit
// size matching the new output length.
func TestEditZeroSizeTopLevelBox(t *testing.T) {
	resetEditConfig(t)
	ftyp := box(t, "ftyp", append([]byte("isom"), 0, 0, 0, 1))
	mvhd := box(t, "mvhd", make([]byte, 100))
	zeroMoov := append([]byte{0, 0, 0, 0, 'm', 'o', 'o', 'v'}, mvhd...)
	input := append(ftyp, zeroMoov...)

	output, err := runEdit(t, input)
	require.NoError(t, err)

	// the output must be a fully parseable file whose final box reaches EOF
	r := bytes.NewReader(output)
	_, err = mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		return h.Expand()
	})
	require.NoError(t, err)
	moovBI := mustFindBox(t, bytes.NewReader(output), nil, mp4.BoxPath{mp4.BoxTypeMoov()})
	// the EOF-extending semantics are preserved and the box still spans the
	// remainder of the output
	assert.True(t, moovBI.ExtendToEOF)
	assert.Equal(t, uint64(len(output))-moovBI.Offset, moovBI.Size)
}

// Truncated inputs produce an error and never a silently shortened file.
func TestEditTruncatedInput(t *testing.T) {
	resetEditConfig(t)
	t.Run("truncated known container", func(t *testing.T) {
		input := []byte{
			0x00, 0x00, 0x00, 0x64, 'm', 'o', 'o', 'v',
			0x00, 0x00, 0x00, 0x08, 'm', 'v', 'h', 'd',
		}
		_, err := runEdit(t, input)
		require.Error(t, err)
	})
	t.Run("truncated unknown box", func(t *testing.T) {
		input := []byte{
			0x00, 0x00, 0x00, 0x20, 'z', 'z', 'z', 'z',
		}
		_, err := runEdit(t, input)
		require.Error(t, err)
	})
	t.Run("zero size box inside container", func(t *testing.T) {
		inner := append([]byte{0, 0, 0, 0, 'z', 'z', 'z', 'z'},
			box(t, "free", nil)...)
		input := box(t, "moov", inner)
		_, err := runEdit(t, input)
		require.Error(t, err)
	})
}

// Dropping a box keeps all unknown siblings intact and fixes parent sizes.
func TestEditDropKeepsUnknownBoxes(t *testing.T) {
	resetEditConfig(t)
	unknown := box(t, "zzzz", []byte{1, 2, 3})
	free := box(t, "free", []byte{0xaa})
	moov := box(t, "moov", append(unknown, free...))
	input := moov

	config = Config{
		values:    Values{BaseMediaDecodeTime: math.MaxUint64},
		dropBoxes: Boxes{"free"},
	}

	dir, err := ioutil.TempDir("", "mp4-edit-drop")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	inPath := filepath.Join(dir, "in.mp4")
	outPath := filepath.Join(dir, "out.mp4")
	require.NoError(t, ioutil.WriteFile(inPath, input, 0600))
	require.NoError(t, editFile(inPath, outPath))
	output, err := ioutil.ReadFile(outPath)
	require.NoError(t, err)

	// only the unknown box remains, and the parent size was shrunk.
	// Parse the output with a raw box walk because generic extraction
	// validates declared sizes against the whole file length.
	r := bytes.NewReader(output)
	moovBI, err := mp4.ReadBoxInfo(r)
	require.NoError(t, err)
	require.Equal(t, mp4.BoxTypeMoov(), moovBI.Type)
	unkBI, err := mp4.ReadBoxInfo(r)
	require.NoError(t, err)
	assert.Equal(t, uint64(11), unkBI.Size)
	assert.Equal(t, uint64(8)+unkBI.Size, moovBI.Size)
	assert.Equal(t, uint64(len(output)), moovBI.Offset+moovBI.Size)
}

func mustFindBox(t *testing.T, r io.ReadSeeker, parent *mp4.BoxInfo, path mp4.BoxPath) *mp4.BoxInfo {
	t.Helper()
	bis, err := mp4.ExtractBoxes(r, parent, []mp4.BoxPath{path})
	require.NoError(t, err)
	require.Len(t, bis, 1)
	return bis[0]
}

func mustFindBoxesWithPayload(t *testing.T, r io.ReadSeeker, parent *mp4.BoxInfo, path mp4.BoxPath) []*mp4.BoxInfoWithPayload {
	t.Helper()
	bs, err := mp4.ExtractBoxesWithPayload(r, parent, []mp4.BoxPath{path})
	require.NoError(t, err)
	return bs
}

// assertConsistentContainer verifies that the children of bi are contiguous
// and exactly fill the box payload, i.e. the parent size matches its content.
func assertConsistentContainer(t *testing.T, r io.ReadSeeker, bi *mp4.BoxInfo) {
	t.Helper()
	children, err := mp4.ExtractBoxes(r, bi, []mp4.BoxPath{{mp4.BoxTypeAny()}})
	require.NoError(t, err)
	require.NotEmpty(t, children)

	expectedOffset := bi.Offset + bi.HeaderSize
	var childrenSize uint64
	for _, child := range children {
		assert.Equal(t, expectedOffset, child.Offset, "child %s has a gap or overlap", child.Type)
		expectedOffset = child.Offset + child.Size
		childrenSize += child.Size
	}
	assert.Equal(t, bi.Size-bi.HeaderSize, childrenSize, "parent size does not match its children")
}
