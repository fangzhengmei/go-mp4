package mp4

import (
	"errors"
	"fmt"
	"io"
)

type Writer struct {
	writer  io.WriteSeeker
	reader  io.ReadSeeker
	biStack []*BoxInfo
}

func NewWriter(w io.WriteSeeker) *Writer {
	var reader io.ReadSeeker
	if rws, ok := w.(io.ReadWriteSeeker); ok {
		reader = rws
	}
	return &Writer{
		writer: w,
		reader: reader,
	}
}

// NewReadWriter creates a Writer backed by a readable and writable seeker.
// The readable interface is required when a box grows beyond the 32-bit size
// field and its small header must be replaced with a 64-bit large header.
func NewReadWriter(w io.ReadWriteSeeker) *Writer {
	return &Writer{
		writer: w,
		reader: w,
	}
}

func (w *Writer) Write(p []byte) (int, error) {
	return w.writer.Write(p)
}

func (w *Writer) Seek(offset int64, whence int) (int64, error) {
	return w.writer.Seek(offset, whence)
}

func (w *Writer) StartBox(bi *BoxInfo) (*BoxInfo, error) {
	bi, err := WriteBoxInfo(w.writer, bi)
	if err != nil {
		return nil, err
	}
	w.biStack = append(w.biStack, bi)
	return bi, nil
}

func (w *Writer) EndBox() (*BoxInfo, error) {
	bi := w.biStack[len(w.biStack)-1]
	w.biStack = w.biStack[:len(w.biStack)-1]
	end, err := w.writer.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}

	// A box may grow beyond the 32-bit size field while it is being written,
	// requiring a 16-byte 64-bit large-size header in place of the original
	// 8-byte header. The same applies to each ancestor box. Process headers
	// from the outermost box downwards: each insertion shifts all following
	// bytes (including the descendants) forward, and the accumulated shifts
	// are applied to every box offset before it is processed.
	type headerGrowth struct {
		bi    *BoxInfo
		delta uint64
	}

	// A box header always occupies 8 or 16 bytes, therefore its size field
	// must be computed against its current (pre-growth) header size.
	bi.Size = uint64(end) - bi.Offset

	// First, determine which headers must grow and by how much. The growth
	// of a child is inherited by all of its ancestors.
	planned := make([]headerGrowth, 0, len(w.biStack)+1)
	childGrowth := uint64(0)
	currentHeader := EncodeBoxInfo(bi)
	if delta := uint64(len(currentHeader)) - bi.HeaderSize; delta != 0 {
		planned = append(planned, headerGrowth{bi: bi, delta: delta})
		childGrowth = delta
	}
	for i := len(w.biStack) - 1; i >= 0; i-- {
		parent := w.biStack[i]
		parent.Size = uint64(end) - parent.Offset + childGrowth
		parentHeader := EncodeBoxInfo(parent)
		if delta := uint64(len(parentHeader)) - parent.HeaderSize; delta != 0 {
			planned = append(planned, headerGrowth{bi: parent, delta: delta})
			childGrowth += delta
		}
	}

	// Perform the insertions from the outermost box downwards. Each
	// insertion shifts all following boxes forward, so the accumulated shift
	// is added to a box offset only once all insertions before it have run.
	var appliedShift uint64
	for i := len(planned) - 1; i >= 0; i-- {
		growth := planned[i]
		target := growth.bi
		target.Offset += appliedShift
		if w.reader == nil {
			return nil, errors.New("writer cannot grow box header: underlying writer is not readable")
		}
		if err := w.shiftContents(target.Offset+target.HeaderSize, uint64(end)+appliedShift, growth.delta); err != nil {
			return nil, err
		}
		appliedShift += growth.delta
		end += int64(growth.delta)
		target.Size += growth.delta

		newHeader := EncodeBoxInfo(target)
		if _, err := w.writer.Seek(int64(target.Offset), io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := w.writer.Write(newHeader); err != nil {
			return nil, err
		}
		target.HeaderSize = uint64(len(newHeader))
	}

	// Always rewrite the current box header: even when its size did not
	// cross the 32-bit boundary, the final size field is only known now.
	finalHeader := EncodeBoxInfo(bi)
	if _, err := w.writer.Seek(int64(bi.Offset), io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := w.writer.Write(finalHeader); err != nil {
		return nil, err
	}
	bi.HeaderSize = uint64(len(finalHeader))

	if _, err := w.writer.Seek(end, io.SeekStart); err != nil {
		return nil, err
	}
	return bi, nil
}

// shiftContents moves the byte range [start, end) forward by delta bytes by
// inserting delta zero bytes at start. The caller must seek back to the
// intended position afterwards.
func (w *Writer) shiftContents(start, end uint64, delta uint64) error {
	const bufSize = 64 * 1024
	// Grow the destination region first. Writers that truncate on a
	// beyond-EOF write (e.g. in-memory file implementations) would otherwise
	// discard the tail bytes when the first destination block is written.
	if err := w.growOutput(end + delta); err != nil {
		return err
	}

	buf := make([]byte, bufSize)
	// Copy backwards so that source bytes are never overwritten before being
	// read. The destination region is grown by delta bytes.
	dst := end + delta
	src := end
	for src > start {
		n := bufSize
		if remaining := src - start; remaining < uint64(n) {
			n = int(remaining)
		}
		src -= uint64(n)
		dst -= uint64(n)
		if _, err := w.reader.Seek(int64(src), io.SeekStart); err != nil {
			return err
		}
		if _, err := io.ReadFull(w.reader, buf[:n]); err != nil {
			return err
		}
		if _, err := w.writer.Seek(int64(dst), io.SeekStart); err != nil {
			return err
		}
		if _, err := w.writer.Write(buf[:n]); err != nil {
			return err
		}
	}
	// Fill the inserted gap with zeros; it is overwritten by the new header
	// immediately after this function returns.
	if _, err := w.writer.Seek(int64(start), io.SeekStart); err != nil {
		return err
	}
	if _, err := w.writer.Write(make([]byte, delta)); err != nil {
		return err
	}
	return nil
}

// growOutput ensures the underlying output has room for size bytes. A single
// zero byte is written at size-1 when the current output is shorter; seekable
// file implementations create a sparse hole instead of materializing it.
func (w *Writer) growOutput(size uint64) error {
	cur, err := w.writer.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	end, err := w.writer.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if uint64(end) >= size {
		_, err := w.writer.Seek(cur, io.SeekStart)
		return err
	}
	if _, err := w.writer.Seek(int64(size)-1, io.SeekStart); err != nil {
		return err
	}
	if _, err := w.writer.Write([]byte{0}); err != nil {
		return err
	}
	_, err = w.writer.Seek(cur, io.SeekStart)
	return err
}

func (w *Writer) CopyBox(r io.ReadSeeker, bi *BoxInfo) error {
	if _, err := bi.SeekToStart(r); err != nil {
		return err
	}
	if n, err := io.CopyN(w, r, int64(bi.Size)); err != nil {
		return err
	} else if n != int64(bi.Size) {
		return errors.New("failed to copy box")
	}
	return nil
}

// CopyPayload copies the payload (the content excluding the box header) of
// the box described by bi from r to w. It is useful together with StartBox
// and EndBox for rewriting a box while keeping payload bytes that the
// library does not model, such as trailing data of a partially recognized
// box. A truncated source is reported as an error instead of producing a
// shorter, silently corrupted output box.
func (w *Writer) CopyPayload(r io.ReadSeeker, bi *BoxInfo) (uint64, error) {
	if bi.Size < bi.HeaderSize {
		return 0, fmt.Errorf("invalid box size: type=%s, size=%d, headerSize=%d", bi.Type.String(), bi.Size, bi.HeaderSize)
	}
	if _, err := bi.SeekToPayload(r); err != nil {
		return 0, err
	}
	size := bi.Size - bi.HeaderSize
	n, err := io.CopyN(w, r, int64(size))
	if err != nil {
		return uint64(n), err
	}
	if n != int64(size) {
		return uint64(n), errors.New("failed to copy box payload")
	}
	return uint64(n), nil
}
