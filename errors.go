package mp4

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// BoxError represents an invalid MP4 container structure.
type BoxError struct {
	// Path is the box path from the root box to the failed box.
	Path BoxPath

	// BoxInfo contains information of the box being processed.
	BoxInfo BoxInfo

	// Offset is the absolute file offset where the failure was detected.
	Offset uint64

	// Err is the original structural error.
	Err error
}

func (e *BoxError) Error() string {
	return fmt.Sprintf("box error: path=%s offset=%d failure_offset=%d: %s",
		formatBoxPath(e.Path), e.BoxInfo.Offset, e.Offset, e.Err)
}

func (e *BoxError) Unwrap() error {
	return e.Err
}

// ReaderError represents a failure returned by the underlying reader.
// Truncated containers are represented by BoxError instead, even when the
// reader reports the truncation as io.EOF or io.ErrUnexpectedEOF.
type ReaderError struct {
	// Offset is the absolute reader offset where the failure was detected.
	Offset uint64

	// Err is the error returned by the underlying reader.
	Err error
}

func (e *ReaderError) Error() string {
	return fmt.Sprintf("reader error: offset=%d: %s", e.Offset, e.Err)
}

func (e *ReaderError) Unwrap() error {
	return e.Err
}

func formatBoxPath(path BoxPath) string {
	if len(path) == 0 {
		return "/"
	}

	parts := make([]string, 0, len(path))
	for _, boxType := range path {
		parts = append(parts, boxType.String())
	}
	return "/" + strings.Join(parts, "/")
}

func copyBoxPath(path BoxPath) BoxPath {
	if len(path) == 0 {
		return nil
	}

	copied := make(BoxPath, len(path))
	copy(copied, path)
	return copied
}

func isTruncationError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func isCleanEOF(err error) bool {
	return errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)
}

func wrapBoxError(err error, path BoxPath, bi *BoxInfo, offset uint64) error {
	if err == nil {
		return nil
	}

	var readerErr *ReaderError
	if errors.As(err, &readerErr) {
		if !isTruncationError(err) {
			return err
		}
	}

	var boxErr *BoxError
	if errors.As(err, &boxErr) {
		if len(boxErr.Path) == 0 {
			boxErr.Path = copyBoxPath(path)
		}
		if bi != nil {
			if boxErr.BoxInfo.Offset == 0 && bi.Offset != 0 {
				boxErr.BoxInfo.Offset = bi.Offset
			}
			if boxErr.BoxInfo.HeaderSize == 0 {
				boxErr.BoxInfo.HeaderSize = bi.HeaderSize
			}
			if boxErr.BoxInfo.Type == BoxTypeAny() {
				boxErr.BoxInfo.Type = bi.Type
			}
			if bi.ExtendToEOF {
				boxErr.BoxInfo.ExtendToEOF = true
			}
		}
		return boxErr
	}

	info := BoxInfo{}
	if bi != nil {
		info = *bi
	}
	return &BoxError{
		Path:    copyBoxPath(path),
		BoxInfo: info,
		Offset:  offset,
		Err:     err,
	}
}

func currentOffset(s io.Seeker) (uint64, error) {
	offset, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, &ReaderError{Err: err}
	}
	return uint64(offset), nil
}
