package mp4

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// MetadataPosition specifies where RewriteMetadata places the moov box.
type MetadataPosition int

const (
	// MetadataAtBeginning places the moov box before all other top-level boxes.
	MetadataAtBeginning MetadataPosition = iota
	// MetadataAtEnd places the moov box after all other top-level boxes.
	MetadataAtEnd
)

type truncatableWriteSeeker interface {
	io.WriteSeeker
	Truncate(size int64) error
}

type chunkOffsetBox struct {
	info         BoxInfo
	stco         *Stco
	co64         *Co64
	useLargeSize bool
	offsetsValue []uint64
	original     []uint64
}

type rewriteBoxNode struct {
	info     BoxInfo
	children []*rewriteBoxNode
	chunks   []*chunkOffsetBox
}

type rewriteRootBox struct {
	info BoxInfo
	moov *rewriteBoxNode
}

type rewritePlan struct {
	rootBoxes       []*rewriteRootBox
	moov            *rewriteBoxNode
	moovSize        uint64
	moovTargetIndex int
}

var rewriteContainerTypes = map[BoxType]struct{}{
	BoxTypeDinf(): {},
	BoxTypeEdts(): {},
	BoxTypeIlst(): {},
	BoxTypeMdia(): {},
	BoxTypeMeta(): {},
	BoxTypeMfra(): {},
	BoxTypeMinf(): {},
	BoxTypeMoof(): {},
	BoxTypeMoov(): {},
	BoxTypeMvex(): {},
	BoxTypeSchi(): {},
	BoxTypeSinf(): {},
	BoxTypeStbl(): {},
	BoxTypeStsd(): {},
	BoxTypeTraf(): {},
	BoxTypeTrak(): {},
	BoxTypeTrep(): {},
	BoxTypeTrex(): {},
	BoxTypeUdta(): {},
	BoxTypeWave(): {},
}

// RewriteMetadata rewrites the container while moving its moov box to the
// requested position. All boxes other than moov are copied byte-for-byte, and
// only stco/co64 chunk offsets are rewritten inside moov. Writers that
// implement Truncate are reset before writing; use RewriteMetadataFile when an
// existing destination must remain untouched until the rewrite succeeds.
func RewriteMetadata(r io.ReadSeeker, w io.WriteSeeker, position MetadataPosition) error {
	if position != MetadataAtBeginning && position != MetadataAtEnd {
		return fmt.Errorf("invalid metadata position: %d", position)
	}

	plan, err := buildRewritePlan(r, position)
	if err != nil {
		return err
	}
	if err := plan.resolveOffsets(); err != nil {
		return err
	}
	return writeRewrite(r, w, plan)
}

// RewriteMetadataFile is the file-based form of RewriteMetadata. It writes to
// a temporary file in the destination directory and only replaces the output
// path after the rewritten container has been completed and synced.
func RewriteMetadataFile(inputPath, outputPath string, position MetadataPosition) (err error) {
	input, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer input.Close()

	outputDir := filepath.Dir(outputPath)
	temp, err := os.CreateTemp(outputDir, ".go-mp4-rewrite-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}()

	if err = RewriteMetadata(input, temp, position); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, outputPath)
}

func buildRewritePlan(r io.ReadSeeker, position MetadataPosition) (*rewritePlan, error) {
	plan := &rewritePlan{
		rootBoxes: make([]*rewriteRootBox, 0, 8),
	}
	stack := make([]*rewriteBoxNode, 0, 16)

	_, err := ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		bi := h.BoxInfo
		node := &rewriteBoxNode{info: bi}

		if len(stack) == 0 {
			root := &rewriteRootBox{info: bi}
			plan.rootBoxes = append(plan.rootBoxes, root)
			if bi.Type == BoxTypeMoov() {
				if plan.moov != nil {
					return nil, errors.New("multiple moov boxes are not supported")
				}
				plan.moov = node
				root.moov = node
				stack = append(stack, node)
				if _, err := h.Expand(); err != nil {
					return nil, err
				}
				stack = stack[:0]
			}
			return nil, nil
		}

		parent := stack[len(stack)-1]
		parent.children = append(parent.children, node)

		switch bi.Type {
		case BoxTypeStco():
			chunk, err := readStcoChunkBox(r, bi)
			if err != nil {
				return nil, err
			}
			node.chunks = append(node.chunks, chunk)
		case BoxTypeCo64():
			chunk, err := readCo64ChunkBox(r, bi)
			if err != nil {
				return nil, err
			}
			node.chunks = append(node.chunks, chunk)
		default:
			if _, ok := rewriteContainerTypes[bi.Type]; ok {
				stack = append(stack, node)
				if _, err := h.Expand(); err != nil {
					return nil, err
				}
				popped := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if popped != node {
					return nil, errors.New("internal error: unexpected box nesting")
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	if plan.moov == nil {
		return nil, errors.New("moov box not found")
	}
	stack = stack[:0]

	plan.moovTargetIndex = len(plan.rootBoxes) - 1
	if position == MetadataAtBeginning {
		plan.moovTargetIndex = 0
	} else {
		plan.moovTargetIndex = len(plan.rootBoxes)
	}
	plan.moovSize = calculateMoovSize(plan.moov)
	return plan, nil
}

func readStcoChunkBox(r io.ReadSeeker, bi BoxInfo) (*chunkOffsetBox, error) {
	if _, err := bi.SeekToPayload(r); err != nil {
		return nil, err
	}
	box := &Stco{}
	if _, err := Unmarshal(r, bi.Size-bi.HeaderSize, box, bi.Context); err != nil {
		return nil, err
	}
	offsets := make([]uint64, len(box.ChunkOffset))
	for i, offset := range box.ChunkOffset {
		offsets[i] = uint64(offset)
	}
	return &chunkOffsetBox{info: bi, stco: box, original: offsets}, nil
}

func readCo64ChunkBox(r io.ReadSeeker, bi BoxInfo) (*chunkOffsetBox, error) {
	if _, err := bi.SeekToPayload(r); err != nil {
		return nil, err
	}
	box := &Co64{}
	if _, err := Unmarshal(r, bi.Size-bi.HeaderSize, box, bi.Context); err != nil {
		return nil, err
	}
	return &chunkOffsetBox{info: bi, co64: box, useLargeSize: true, original: append([]uint64(nil), box.ChunkOffset...)}, nil
}

func (plan *rewritePlan) collectChunkBoxes() []*chunkOffsetBox {
	chunks := make([]*chunkOffsetBox, 0, 8)
	var walk func(*rewriteBoxNode)
	walk = func(node *rewriteBoxNode) {
		chunks = append(chunks, node.chunks...)
		for _, child := range node.children {
			walk(child)
		}
	}
	walk(plan.moov)
	return chunks
}

func calculateMoovSize(node *rewriteBoxNode) uint64 {
	var calculate func(*rewriteBoxNode) uint64
	calculate = func(n *rewriteBoxNode) uint64 {
		if len(n.children) == 0 && len(n.chunks) == 0 {
			return normalizeBoxSize(n.info.Size, n.info.HeaderSize)
		}
		if len(n.children) == 0 && len(n.chunks) == 1 {
			chunk := n.chunks[0]
			return chunkSize(chunk)
		}

		size := n.info.HeaderSize
		if len(n.chunks) == 0 {
			size += nodePrefixSize(n)
		}
		for _, child := range n.children {
			size += calculate(child)
		}
		for _, chunk := range n.chunks {
			size += chunkSize(chunk)
		}
		return normalizeBoxSize(size, n.info.HeaderSize)
	}
	return calculate(node)
}

func nodePrefixSize(node *rewriteBoxNode) uint64 {
	if len(node.children) == 0 {
		return node.info.Size - node.info.HeaderSize
	}
	firstChild := node.children[0]
	return firstChild.info.Offset - node.info.Offset - node.info.HeaderSize
}

func calculateNodeSize(node *rewriteBoxNode) uint64 {
	return calculateMoovSize(node)
}

func chunkSize(chunk *chunkOffsetBox) uint64 {
	entrySize := uint64(4)
	if chunk.useLargeSize {
		entrySize = 8
	}
	headerSize := chunk.info.HeaderSize
	if chunk.stco != nil && chunk.useLargeSize {
		headerSize = SmallHeaderSize
	}
	entryBytes := entrySize * uint64(chunk.entryCount())
	if math.MaxUint64-headerSize-8 < entryBytes {
		return math.MaxUint64
	}
	size := headerSize + 8 + entryBytes
	if size > math.MaxUint32 && headerSize == SmallHeaderSize {
		size += LargeHeaderSize - SmallHeaderSize
	}
	return size
}

func (chunk *chunkOffsetBox) entryCount() uint32 {
	if chunk.stco != nil {
		return chunk.stco.EntryCount
	}
	return chunk.co64.EntryCount
}

func normalizeBoxSize(size, originalHeaderSize uint64) uint64 {
	if size > math.MaxUint32 && originalHeaderSize == SmallHeaderSize {
		if math.MaxUint64-size < LargeHeaderSize-SmallHeaderSize {
			return math.MaxUint64
		}
		return size + LargeHeaderSize - SmallHeaderSize
	}
	return size
}

func (plan *rewritePlan) resolveOffsets() error {
	chunks := plan.collectChunkBoxes()
	for i := 0; i <= len(chunks); i++ {
		layout, err := plan.layout()
		if err != nil {
			return err
		}

		largeChanged := false
		for _, chunk := range chunks {
			needsLarge := chunk.useLargeSize
			offsets := make([]uint64, 0, chunk.entryCount())
			for _, oldOffset := range chunk.original {
				newOffset, ok := layout.mapOffset(oldOffset)
				if !ok {
					return fmt.Errorf("chunk offset %d is outside any top-level data box", oldOffset)
				}
				if newOffset > math.MaxUint32 && chunk.stco != nil {
					needsLarge = true
				}
				offsets = append(offsets, newOffset)
			}
			if needsLarge != chunk.useLargeSize {
				largeChanged = true
				chunk.useLargeSize = needsLarge
			}
			chunk.offsetsValue = append(chunk.offsetsValue[:0], offsets...)
		}

		newMoovSize := calculateMoovSize(plan.moov)
		if !largeChanged {
			plan.moovSize = newMoovSize
			finalLayout, err := plan.layout()
			if err != nil {
				return err
			}
			for _, chunk := range chunks {
				offsets := make([]uint64, 0, chunk.entryCount())
				for _, oldOffset := range chunk.original {
					newOffset, ok := finalLayout.mapOffset(oldOffset)
					if !ok {
						return fmt.Errorf("chunk offset %d is outside any top-level data box", oldOffset)
					}
					offsets = append(offsets, newOffset)
				}
				chunk.offsetsValue = append(chunk.offsetsValue[:0], offsets...)
				chunk.setOffsets(offsets)
			}
			return nil
		}
		plan.moovSize = newMoovSize
	}
	return errors.New("failed to converge chunk offset box layout")
}

type rewriteLayoutEntry struct {
	oldOffset uint64
	oldEnd    uint64
	newOffset uint64
}

type rewriteLayout struct {
	entries []rewriteLayoutEntry
}

func (plan *rewritePlan) layout() (*rewriteLayout, error) {
	layout := &rewriteLayout{entries: make([]rewriteLayoutEntry, 0, len(plan.rootBoxes))}
	var offset uint64
	moovPlaced := false

	for i, root := range plan.rootBoxes {
		if !moovPlaced && i == plan.moovTargetIndex {
			if math.MaxUint64-offset < plan.moovSize {
				return nil, errors.New("rewritten file offset overflow")
			}
			offset += plan.moovSize
			moovPlaced = true
		}
		if root.moov != nil {
			continue
		}
		if root.info.Offset+root.info.Size < root.info.Offset {
			return nil, errors.New("top-level box offset overflow")
		}
		if math.MaxUint64-offset < root.info.Size {
			return nil, errors.New("rewritten file offset overflow")
		}
		layout.entries = append(layout.entries, rewriteLayoutEntry{
			oldOffset: root.info.Offset,
			oldEnd:    root.info.Offset + root.info.Size,
			newOffset: offset,
		})
		offset += root.info.Size
	}
	if !moovPlaced && plan.moovTargetIndex == len(plan.rootBoxes) {
		if math.MaxUint64-offset < plan.moovSize {
			return nil, errors.New("rewritten file offset overflow")
		}
		offset += plan.moovSize
		moovPlaced = true
	}
	if !moovPlaced {
		return nil, errors.New("internal error: moov box was not placed")
	}
	return layout, nil
}

func (layout *rewriteLayout) mapOffset(offset uint64) (uint64, bool) {
	for _, entry := range layout.entries {
		if offset >= entry.oldOffset && offset < entry.oldEnd {
			delta := offset - entry.oldOffset
			if math.MaxUint64-entry.newOffset < delta {
				return 0, false
			}
			return entry.newOffset + delta, true
		}
	}
	return 0, false
}

func (chunk *chunkOffsetBox) offsets() []uint64 {
	if chunk.offsetsValue != nil {
		return chunk.offsetsValue
	}
	if chunk.stco != nil {
		offsets := make([]uint64, len(chunk.stco.ChunkOffset))
		for i, offset := range chunk.stco.ChunkOffset {
			offsets[i] = uint64(offset)
		}
		return offsets
	}
	return append([]uint64(nil), chunk.co64.ChunkOffset...)
}

func (chunk *chunkOffsetBox) setOffsets(offsets []uint64) {
	chunk.offsetsValue = append(chunk.offsetsValue[:0], offsets...)
	if chunk.stco != nil {
		values := make([]uint32, len(offsets))
		for i, offset := range offsets {
			values[i] = uint32(offset)
		}
		chunk.stco.ChunkOffset = values
		return
	}
	chunk.co64.ChunkOffset = offsets
}

func writeRewrite(r io.ReadSeeker, w io.WriteSeeker, plan *rewritePlan) error {
	if tws, ok := w.(truncatableWriteSeeker); ok {
		if err := tws.Truncate(0); err != nil {
			return err
		}
	}
	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return err
	}

	writer := NewWriter(w)
	moovWritten := false
	for i, root := range plan.rootBoxes {
		if !moovWritten && i == plan.moovTargetIndex {
			if err := writeMoov(r, writer, plan.moov); err != nil {
				return err
			}
			moovWritten = true
		}
		if root.moov != nil {
			continue
		}
		if err := writer.CopyBox(r, &root.info); err != nil {
			return err
		}
	}
	if !moovWritten && plan.moovTargetIndex == len(plan.rootBoxes) {
		if err := writeMoov(r, writer, plan.moov); err != nil {
			return err
		}
		moovWritten = true
	}
	if !moovWritten {
		return errors.New("internal error: moov box was not written")
	}
	return nil
}

func writeMoov(r io.ReadSeeker, w *Writer, node *rewriteBoxNode) error {
	_, err := w.StartBox(&node.info)
	if err != nil {
		return err
	}
	for _, child := range node.children {
		if err := writeNode(r, w, child); err != nil {
			return err
		}
	}
	ended, err := w.EndBox()
	if err != nil {
		return err
	}
	expectedSize := calculateNodeSize(node)
	if ended.Size != expectedSize {
		return errors.New("internal error: rewritten moov box size mismatch")
	}
	return nil
}

func writeNode(r io.ReadSeeker, w *Writer, node *rewriteBoxNode) error {
	if len(node.chunks) > 0 {
		return writeChunkBox(w, node.chunks[0])
	}
	if len(node.children) == 0 {
		return w.CopyBox(r, &node.info)
	}

	if _, err := w.StartBox(&node.info); err != nil {
		return err
	}
	if _, err := node.info.SeekToPayload(r); err != nil {
		return err
	}
	if _, err := io.CopyN(w, r, int64(nodePrefixSize(node))); err != nil {
		return err
	}
	for _, child := range node.children {
		if err := writeNode(r, w, child); err != nil {
			return err
		}
	}
	ended, err := w.EndBox()
	if err != nil {
		return err
	}
	if ended.Size != calculateNodeSize(node) {
		return errors.New("internal error: rewritten box size mismatch")
	}
	return nil
}

func writeChunkBox(w *Writer, chunk *chunkOffsetBox) error {
	info := chunk.info
	if chunk.useLargeSize && chunk.stco != nil {
		info.Type = BoxTypeCo64()
		info.HeaderSize = SmallHeaderSize
	}
	desiredSize := chunkSize(chunk)
	if desiredSize > math.MaxUint32 {
		info.HeaderSize = LargeHeaderSize
	}
	info.Size = desiredSize

	_, err := w.StartBox(&info)
	if err != nil {
		return err
	}
	var box IImmutableBox
	if chunk.useLargeSize && chunk.stco != nil {
		co64 := &Co64{
			FullBox:    chunk.stco.FullBox,
			EntryCount: chunk.stco.EntryCount,
		}
		co64.ChunkOffset = append([]uint64(nil), chunk.offsets()...)
		box = co64
	} else if chunk.co64 != nil {
		box = chunk.co64
	} else {
		box = chunk.stco
	}
	if _, err := Marshal(w, box, info.Context); err != nil {
		return err
	}
	ended, err := w.EndBox()
	if err != nil {
		return err
	}
	if ended.HeaderSize != info.HeaderSize || ended.Size != desiredSize {
		return errors.New("internal error: rewritten chunk offset box size mismatch")
	}
	return nil
}
