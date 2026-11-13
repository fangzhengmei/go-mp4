package mp4

import (
	"errors"
	"fmt"
	"io"
)

func checkPayloadSize(r io.ReadSeeker, bi *BoxInfo, path BoxPath) error {
	fileSize, err := r.Seek(0, io.SeekEnd)
	if _, seekErr := r.Seek(int64(bi.Offset+bi.HeaderSize), io.SeekStart); seekErr != nil {
		return wrapBoxError(seekErr, path, bi, bi.Offset+bi.HeaderSize)
	}
	if err != nil {
		return wrapBoxError(&ReaderError{Err: err}, path, bi, bi.Offset)
	}
	if bi.Offset+bi.Size > uint64(fileSize) {
		end := bi.Offset + bi.Size
		return wrapBoxError(
			fmt.Errorf("box size exceeds file size: offset=%d, size=%d, fileSize=%d: %w", bi.Offset, bi.Size, fileSize, io.ErrUnexpectedEOF),
			path,
			bi,
			end,
		)
	}
	return nil
}

func checkBoxWithinReader(r io.ReadSeeker, bi *BoxInfo, path BoxPath, totalSize uint64, isRoot bool) error {
	if !isRoot && bi.Size > totalSize {
		return wrapBoxError(
			fmt.Errorf("too large box size: type=%s, size=%d, actualBufSize=%d: %w", bi.Type.String(), bi.Size, totalSize, io.ErrUnexpectedEOF),
			path,
			bi,
			bi.Offset,
		)
	}

	if isRoot || bi.ExtendToEOF || !bi.IsSupportedType() {
		return checkPayloadSize(r, bi, path)
	}

	// A box within a parent whose bounds have already been validated only needs
	// an integer-safe seek check.
	target := int64(bi.Offset + bi.Size)
	if uint64(target) != bi.Offset+bi.Size {
		return wrapBoxError(
			fmt.Errorf("box end offset overflows int64: offset=%d size=%d", bi.Offset, bi.Size),
			path,
			bi,
			bi.Offset,
		)
	}
	if _, err := bi.SeekToEnd(r); err != nil {
		return wrapBoxError(err, path, bi, bi.Offset+bi.Size)
	}
	if _, err := bi.SeekToPayload(r); err != nil {
		return wrapBoxError(err, path, bi, bi.Offset+bi.HeaderSize)
	}
	return nil
}

func wrapReadError(err error, path BoxPath, bi *BoxInfo, r io.Seeker) error {
	if err == nil {
		return nil
	}

	var readerErr *ReaderError
	if errors.As(err, &readerErr) {
		if isTruncationError(err) || errors.Is(err, ErrBoxInfoNotFound) || errors.Is(err, ErrUnsupportedBoxVersion) {
			return wrapBoxError(err, path, bi, readerErr.Offset)
		}
		return err
	}

	var boxErr *BoxError
	if errors.As(err, &boxErr) {
		return wrapBoxError(err, path, bi, boxErr.Offset)
	}
	if errors.Is(err, ErrBoxInfoNotFound) || errors.Is(err, ErrUnsupportedBoxVersion) {
		offset, seekErr := currentOffset(r)
		if seekErr != nil {
			return seekErr
		}
		return wrapBoxError(err, path, bi, offset)
	}
	if !isTruncationError(err) {
		offset, seekErr := currentOffset(r)
		if seekErr != nil {
			return seekErr
		}
		return &ReaderError{Offset: offset, Err: err}
	}

	offset, seekErr := currentOffset(r)
	if seekErr != nil {
		return seekErr
	}
	return wrapBoxError(err, path, bi, offset)
}

func restorePayload(r io.ReadSeeker, bi *BoxInfo) error {
	_, err := bi.SeekToPayload(r)
	return err
}

func failureOffset(r io.Seeker, err error) uint64 {
	var boxErr *BoxError
	if errors.As(err, &boxErr) {
		return boxErr.Offset
	}

	offset, seekErr := currentOffset(r)
	if seekErr != nil {
		return 0
	}
	return offset
}

type BoxPath []BoxType

func (lhs BoxPath) compareWith(rhs BoxPath) (forwardMatch bool, match bool) {
	if len(lhs) > len(rhs) {
		return false, false
	}
	for i := 0; i < len(lhs); i++ {
		if !lhs[i].MatchWith(rhs[i]) {
			return false, false
		}
	}
	if len(lhs) < len(rhs) {
		return true, false
	}
	return false, true
}

type ReadHandle struct {
	Params      []interface{}
	BoxInfo     BoxInfo
	Path        BoxPath
	ReadPayload func() (box IBox, n uint64, err error)
	ReadData    func(io.Writer) (n uint64, err error)
	Expand      func(params ...interface{}) (vals []interface{}, err error)
}

type ReadHandler func(handle *ReadHandle) (val interface{}, err error)

func ReadBoxStructure(r io.ReadSeeker, handler ReadHandler, params ...interface{}) ([]interface{}, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, &ReaderError{Offset: 0, Err: err}
	}
	return readBoxStructure(r, 0, true, nil, Context{}, handler, params)
}

func ReadBoxStructureFromInternal(r io.ReadSeeker, bi *BoxInfo, handler ReadHandler, params ...interface{}) (interface{}, error) {
	return readBoxStructureFromInternal(r, bi, nil, handler, params)
}

func readBoxStructureFromInternal(r io.ReadSeeker, bi *BoxInfo, path BoxPath, handler ReadHandler, params []interface{}) (interface{}, error) {
	if _, err := bi.SeekToPayload(r); err != nil {
		return nil, wrapBoxError(err, append(path, bi.Type), bi, bi.Offset+bi.HeaderSize)
	}

	currentPath := make(BoxPath, len(path)+1)
	copy(currentPath, path)
	currentPath[len(path)] = bi.Type

	// check comatible-brands
	if len(path) == 0 && bi.Type == BoxTypeFtyp() {
		var ftyp Ftyp
		if err := checkPayloadSize(r, bi, currentPath); err != nil {
			return nil, err
		}
		if _, err := Unmarshal(r, bi.Size-bi.HeaderSize, &ftyp, bi.Context); err != nil {
			if err := restorePayload(r, bi); err != nil {
				return nil, err
			}
			return nil, wrapReadError(err, currentPath, bi, r)
		}
		if ftyp.HasCompatibleBrand(BrandQT()) {
			bi.IsQuickTimeCompatible = true
		}
		if _, err := bi.SeekToPayload(r); err != nil {
			return nil, wrapBoxError(err, currentPath, bi, bi.Offset+bi.HeaderSize)
		}
	}

	// parse numbered ilst items after keys box by saving EntryCount field to context
	if bi.Type == BoxTypeKeys() {
		var keys Keys
		if err := checkPayloadSize(r, bi, currentPath); err != nil {
			return nil, err
		}
		if _, err := Unmarshal(r, bi.Size-bi.HeaderSize, &keys, bi.Context); err != nil {
			if err := restorePayload(r, bi); err != nil {
				return nil, err
			}
			return nil, wrapReadError(err, currentPath, bi, r)
		}
		bi.QuickTimeKeysMetaEntryCount = int(keys.EntryCount)
		if _, err := bi.SeekToPayload(r); err != nil {
			return nil, wrapBoxError(err, currentPath, bi, bi.Offset+bi.HeaderSize)
		}
	}

	ctx := bi.Context
	if bi.Type == BoxTypeWave() {
		ctx.UnderWave = true
	} else if bi.Type == BoxTypeIlst() {
		ctx.UnderIlst = true
	} else if bi.UnderIlst && !bi.UnderIlstMeta && IsIlstMetaBoxType(bi.Type) {
		ctx.UnderIlstMeta = true
		if bi.Type == StrToBoxType("----") {
			ctx.UnderIlstFreeMeta = true
		}
	} else if bi.Type == BoxTypeUdta() {
		ctx.UnderUdta = true
	}

	h := &ReadHandle{
		Params:  params,
		BoxInfo: *bi,
		Path:    currentPath,
	}

	var childrenOffset uint64

	h.ReadPayload = func() (IBox, uint64, error) {
		if _, err := bi.SeekToPayload(r); err != nil {
			return nil, 0, wrapBoxError(err, currentPath, bi, bi.Offset+bi.HeaderSize)
		}

		if err := checkPayloadSize(r, bi, currentPath); err != nil {
			return nil, 0, err
		}
		box, n, err := UnmarshalAny(r, bi.Type, bi.Size-bi.HeaderSize, bi.Context)
		if err != nil {
			if restoreErr := restorePayload(r, bi); restoreErr != nil {
				return nil, 0, restoreErr
			}
			return nil, 0, wrapReadError(err, currentPath, bi, r)
		}
		childrenOffset = bi.Offset + bi.HeaderSize + n
		return box, n, nil
	}

	h.ReadData = func(w io.Writer) (uint64, error) {
		if _, err := bi.SeekToPayload(r); err != nil {
			return 0, wrapBoxError(err, currentPath, bi, bi.Offset+bi.HeaderSize)
		}

		readStart, err := currentOffset(r)
		if err != nil {
			return 0, err
		}
		if err := checkPayloadSize(r, bi, currentPath); err != nil {
			return 0, err
		}

		if _, err := bi.SeekToPayload(r); err != nil {
			return 0, wrapBoxError(err, currentPath, bi, bi.Offset+bi.HeaderSize)
		}
		size := bi.Size - bi.HeaderSize
		n, err := io.CopyN(w, r, int64(size))
		if err != nil {
			if restoreErr := restorePayload(r, bi); restoreErr != nil {
				return 0, restoreErr
			}
			wrappedErr := wrapReadError(err, currentPath, bi, r)
			var readerErr *ReaderError
			if errors.As(wrappedErr, &readerErr) {
				readerErr.Offset = readStart + uint64(n)
				return uint64(n), readerErr
			}
			return uint64(n), wrappedErr
		}
		return size, nil
	}

	h.Expand = func(params ...interface{}) ([]interface{}, error) {
		if childrenOffset == 0 {
			if bi.IsSupportedType() {
				if _, err := bi.SeekToPayload(r); err != nil {
					return nil, wrapBoxError(err, currentPath, bi, bi.Offset+bi.HeaderSize)
				}

				if err := checkPayloadSize(r, bi, currentPath); err != nil {
					return nil, err
				}
				_, n, err := UnmarshalAny(r, bi.Type, bi.Size-bi.HeaderSize, bi.Context)
				if err != nil {
					if restoreErr := restorePayload(r, bi); restoreErr != nil {
						return nil, restoreErr
					}
					return nil, wrapReadError(err, currentPath, bi, r)
				}
				childrenOffset = bi.Offset + bi.HeaderSize + n
			} else {
				if err := checkPayloadSize(r, bi, currentPath); err != nil {
					return nil, err
				}
				childrenOffset = bi.Offset + bi.Size
			}
		} else {
			if _, err := r.Seek(int64(childrenOffset), io.SeekStart); err != nil {
				return nil, wrapBoxError(&ReaderError{Err: err}, currentPath, bi, childrenOffset)
			}
		}

		childrenSize := bi.Offset + bi.Size - childrenOffset
		return readBoxStructure(r, childrenSize, false, currentPath, ctx, handler, params)
	}

	val, err := handler(h)
	if err != nil {
		return nil, wrapReadError(err, currentPath, bi, r)
	}

	if _, err := bi.SeekToEnd(r); err != nil {
		return nil, wrapBoxError(err, currentPath, bi, bi.Offset+bi.Size)
	}
	return val, nil
}

func readBoxStructure(r io.ReadSeeker, totalSize uint64, isRoot bool, path BoxPath, ctx Context, handler ReadHandler, params []interface{}) ([]interface{}, error) {
	vals := make([]interface{}, 0, 8)

	for isRoot || totalSize >= SmallHeaderSize {
		nextPath := append(copyBoxPath(path), BoxTypeAny())
		bi, err := ReadBoxInfo(r)
		if isRoot && isCleanEOF(err) {
			return vals, nil
		} else if err != nil {
			var headerErr *BoxError
			if errors.As(err, &headerErr) && headerErr.BoxInfo.Type != BoxTypeAny() {
				return nil, wrapBoxError(err, append(copyBoxPath(path), headerErr.BoxInfo.Type), &headerErr.BoxInfo, headerErr.Offset)
			}
			return nil, wrapBoxError(err, nextPath, nil, failureOffset(r, err))
		}
		currentPath := append(copyBoxPath(path), bi.Type)

		bi.Context = ctx
		if err := checkBoxWithinReader(r, bi, currentPath, totalSize, isRoot); err != nil {
			return nil, err
		}
		totalSize -= bi.Size

		val, err := readBoxStructureFromInternal(r, bi, path, handler, params)
		if err != nil {
			return nil, wrapReadError(err, currentPath, bi, r)
		}
		vals = append(vals, val)

		if bi.IsQuickTimeCompatible {
			ctx.IsQuickTimeCompatible = true
		}

		// preserve keys entry count on context for subsequent ilst number item box
		if bi.Type == BoxTypeKeys() {
			ctx.QuickTimeKeysMetaEntryCount = bi.QuickTimeKeysMetaEntryCount
		}
	}

	if totalSize != 0 && !ctx.IsQuickTimeCompatible {
		offset, err := currentOffset(r)
		if err != nil {
			return nil, err
		}
		return nil, &BoxError{
			Path:   copyBoxPath(path),
			Offset: offset,
			Err:    io.ErrUnexpectedEOF,
		}
	}

	return vals, nil
}
