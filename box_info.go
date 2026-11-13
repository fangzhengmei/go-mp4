package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

type Context struct {
	// IsQuickTimeCompatible represents whether ftyp.compatible_brands contains "qt  ".
	IsQuickTimeCompatible bool

	// QuickTimeKeysMetaEntryCount the expected number of items under the ilst box as observed from the keys box
	QuickTimeKeysMetaEntryCount int

	// UnderWave represents whether current box is under the wave box.
	UnderWave bool

	// UnderIlst represents whether current box is under the ilst box.
	UnderIlst bool

	// UnderIlstMeta represents whether current box is under the metadata box under the ilst box.
	UnderIlstMeta bool

	// UnderIlstFreeMeta represents whether current box is under "----" box.
	UnderIlstFreeMeta bool

	// UnderUdta represents whether current box is under the udta box.
	UnderUdta bool
}

// BoxInfo has common infomations of box
type BoxInfo struct {
	// Offset specifies an offset of the box in a file.
	Offset uint64

	// Size specifies size(bytes) of box.
	Size uint64

	// HeaderSize specifies size(bytes) of common fields which are defined as "Box" class member at ISO/IEC 14496-12.
	HeaderSize uint64

	// Type specifies box type which is represented by 4 characters.
	Type BoxType

	// ExtendToEOF is set true when Box.size is zero. It means that end of box equals to end of file.
	ExtendToEOF bool

	// Context would be set by ReadBoxStructure, not ReadBoxInfo.
	Context
}

func (bi *BoxInfo) IsSupportedType() bool {
	return bi.Type.IsSupported(bi.Context)
}

const (
	SmallHeaderSize = 8
	LargeHeaderSize = 16
)

// EncodeBoxInfo encodes the common header fields of a box as defined in ISO/IEC 14496-12.
// If bi.ExtendToEOF is true, the size field is set to zero (indicating the box extends to EOF).
// If the size fits in a uint32 and HeaderSize is not LargeHeaderSize, an 8-byte small header is used.
// Otherwise, a 16-byte large header is used with size field set to 1 and the actual size in the extended field.
func EncodeBoxInfo(bi *BoxInfo) []byte {
	var data []byte
	if bi.ExtendToEOF {
		data = make([]byte, SmallHeaderSize)
	} else if bi.Size <= math.MaxUint32 && bi.HeaderSize != LargeHeaderSize {
		data = make([]byte, SmallHeaderSize)
		binary.BigEndian.PutUint32(data, uint32(bi.Size))
	} else {
		data = make([]byte, LargeHeaderSize)
		binary.BigEndian.PutUint32(data, 1)
		binary.BigEndian.PutUint64(data[SmallHeaderSize:], bi.Size)
	}
	data[4] = bi.Type[0]
	data[5] = bi.Type[1]
	data[6] = bi.Type[2]
	data[7] = bi.Type[3]
	return data
}

// WriteBoxInfo writes common fields which are defined as "Box" class member at ISO/IEC 14496-12.
// This function ignores bi.Offset and returns BoxInfo which contains real Offset and recalculated Size/HeaderSize.
func WriteBoxInfo(w io.WriteSeeker, bi *BoxInfo) (*BoxInfo, error) {
	offset, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}

	data := EncodeBoxInfo(bi)
	if _, err := w.Write(data); err != nil {
		return nil, err
	}

	return &BoxInfo{
		Offset:      uint64(offset),
		Size:        bi.Size - bi.HeaderSize + uint64(len(data)),
		HeaderSize:  uint64(len(data)),
		Type:        bi.Type,
		ExtendToEOF: bi.ExtendToEOF,
	}, nil
}

// ReadBoxInfo reads common fields which are defined as "Box" class member at ISO/IEC 14496-12.
func ReadBoxInfo(r io.ReadSeeker) (*BoxInfo, error) {
	offset, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, &ReaderError{Err: err}
	}

	bi := &BoxInfo{
		Offset: uint64(offset),
	}

	// read 8 bytes
	buf := bytes.NewBuffer(make([]byte, 0, SmallHeaderSize))
	if _, err := io.CopyN(buf, r, SmallHeaderSize); err != nil {
		failureOffset, seekErr := currentOffset(r)
		if seekErr != nil {
			return nil, seekErr
		}
		if _, seekErr := r.Seek(offset, io.SeekStart); seekErr != nil {
			return nil, &ReaderError{Offset: failureOffset, Err: seekErr}
		}
		if isTruncationError(err) {
			originalErr := io.ErrUnexpectedEOF
			if err == io.EOF && failureOffset == bi.Offset {
				originalErr = io.EOF
			}
			return nil, &BoxError{
				BoxInfo: *bi,
				Offset:  failureOffset,
				Err:     originalErr,
			}
		}
		return nil, &ReaderError{Offset: failureOffset, Err: err}
	}
	bi.HeaderSize += SmallHeaderSize

	// pick size and type
	data := buf.Bytes()
	bi.Size = uint64(binary.BigEndian.Uint32(data))
	bi.Type = BoxType{data[4], data[5], data[6], data[7]}

	if bi.Size == 0 {
		// box extends to end of file
		offsetEOF, err := r.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, &ReaderError{Err: err}
		}
		if uint64(offsetEOF) < bi.Offset {
			return nil, &BoxError{
				BoxInfo: *bi,
				Offset:  bi.Offset,
				Err:     errors.New("invalid size"),
			}
		}
		bi.Size = uint64(offsetEOF) - bi.Offset
		bi.ExtendToEOF = true
		if _, err := bi.SeekToPayload(r); err != nil {
			return nil, &ReaderError{Err: err}
		}
	} else if bi.Size == 1 {
		// read more 8 bytes
		buf.Reset()
		n, err := io.CopyN(buf, r, LargeHeaderSize-SmallHeaderSize)
		if err != nil {
			failureOffset, seekErr := currentOffset(r)
			if seekErr != nil {
				return nil, seekErr
			}
			if _, seekErr := r.Seek(offset, io.SeekStart); seekErr != nil {
				return nil, &ReaderError{Offset: failureOffset, Err: seekErr}
			}
			if isTruncationError(err) {
				originalErr := io.ErrUnexpectedEOF
				if n == 0 && err == io.EOF {
					originalErr = io.EOF
				} else if n > 0 {
					failureOffset = bi.Offset + bi.HeaderSize + uint64(n)
				}
				return nil, &BoxError{
					BoxInfo: *bi,
					Offset:  failureOffset,
					Err:     originalErr,
				}
			}
			return nil, &ReaderError{Offset: failureOffset, Err: err}
		}
		bi.HeaderSize += LargeHeaderSize - SmallHeaderSize
		bi.Size = binary.BigEndian.Uint64(buf.Bytes())
	}

	if bi.Size == 0 {
		if _, seekErr := r.Seek(offset, io.SeekStart); seekErr != nil {
			return nil, &ReaderError{Offset: bi.Offset, Err: seekErr}
		}
		return nil, &BoxError{
			BoxInfo: *bi,
			Offset:  bi.Offset,
			Err:     errors.New("invalid size"),
		}
	}

	if bi.Size < bi.HeaderSize {
		if _, seekErr := r.Seek(offset, io.SeekStart); seekErr != nil {
			return nil, &ReaderError{Offset: bi.Offset, Err: seekErr}
		}
		return nil, &BoxError{
			BoxInfo: *bi,
			Offset:  bi.Offset,
			Err:     fmt.Errorf("invalid size: size=%d header_size=%d", bi.Size, bi.HeaderSize),
		}
	}
	if bi.Offset > math.MaxInt64 || bi.Size > math.MaxInt64-bi.Offset {
		if _, seekErr := r.Seek(offset, io.SeekStart); seekErr != nil {
			return nil, &ReaderError{Offset: bi.Offset, Err: seekErr}
		}
		return nil, &BoxError{
			BoxInfo: *bi,
			Offset:  bi.Offset,
			Err:     fmt.Errorf("box end offset overflows int64: offset=%d size=%d", bi.Offset, bi.Size),
		}
	}

	return bi, nil
}

func (bi *BoxInfo) SeekToStart(s io.Seeker) (int64, error) {
	if bi.Offset > math.MaxInt64 {
		return 0, &BoxError{
			BoxInfo: *bi,
			Offset:  bi.Offset,
			Err:     fmt.Errorf("box offset overflows int64: offset=%d", bi.Offset),
		}
	}
	offset, err := s.Seek(int64(bi.Offset), io.SeekStart)
	if err != nil {
		return offset, &ReaderError{Offset: bi.Offset, Err: err}
	}
	return offset, nil
}

func (bi *BoxInfo) SeekToPayload(s io.Seeker) (int64, error) {
	target, err := bi.payloadOffset()
	if err != nil {
		return 0, err
	}
	offset, err := s.Seek(int64(target), io.SeekStart)
	if err != nil {
		return offset, &ReaderError{Offset: target, Err: err}
	}
	return offset, nil
}

func (bi *BoxInfo) SeekToEnd(s io.Seeker) (int64, error) {
	target, err := bi.endOffset()
	if err != nil {
		return 0, err
	}
	offset, err := s.Seek(target, io.SeekStart)
	if err != nil {
		return offset, &ReaderError{Offset: bi.Offset + bi.Size, Err: err}
	}
	if uint64(offset) != bi.Offset+bi.Size {
		return offset, &BoxError{
			BoxInfo: *bi,
			Offset:  bi.Offset + bi.Size,
			Err:     io.ErrUnexpectedEOF,
		}
	}
	return offset, nil
}

func (bi *BoxInfo) payloadOffset() (uint64, error) {
	if bi.HeaderSize > math.MaxInt64-bi.Offset {
		return 0, &BoxError{
			BoxInfo: *bi,
			Offset:  bi.Offset,
			Err:     fmt.Errorf("box payload offset overflows int64: offset=%d header_size=%d", bi.Offset, bi.HeaderSize),
		}
	}
	return bi.Offset + bi.HeaderSize, nil
}

func (bi *BoxInfo) endOffset() (int64, error) {
	if bi.Offset > math.MaxInt64 || bi.Size > math.MaxInt64-bi.Offset {
		return 0, &BoxError{
			BoxInfo: *bi,
			Offset:  bi.Offset,
			Err:     fmt.Errorf("box end offset overflows int64: offset=%d size=%d", bi.Offset, bi.Size),
		}
	}
	return int64(bi.Offset + bi.Size), nil
}
