package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

var ErrBoxInfoNotFound = errors.New("box info not found")

// BoxType is mpeg box type
type BoxType [4]byte

func StrToBoxType(code string) BoxType {
	if len(code) != 4 {
		panic(fmt.Errorf("invalid box type id length: [%s]", code))
	}
	return BoxType{code[0], code[1], code[2], code[3]}
}

// Uint32ToBoxType returns a new BoxType from the provied uint32
func Uint32ToBoxType(i uint32) BoxType {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, i)
	return BoxType{b[0], b[1], b[2], b[3]}
}

func (boxType BoxType) String() string {
	if isPrintable(boxType[0]) && isPrintable(boxType[1]) && isPrintable(boxType[2]) && isPrintable(boxType[3]) {
		s := string([]byte{boxType[0], boxType[1], boxType[2], boxType[3]})
		s = strings.ReplaceAll(s, string([]byte{0xa9}), "(c)")
		return s
	}
	return fmt.Sprintf("0x%02x%02x%02x%02x", boxType[0], boxType[1], boxType[2], boxType[3])
}

func isASCII(c byte) bool {
	return c >= 0x20 && c <= 0x7e
}

func isPrintable(c byte) bool {
	return isASCII(c) || c == 0xa9
}

func (lhs BoxType) MatchWith(rhs BoxType) bool {
	if lhs == boxTypeAny || rhs == boxTypeAny {
		return true
	}
	return lhs == rhs
}

var boxTypeAny = BoxType{0x00, 0x00, 0x00, 0x00}

func BoxTypeAny() BoxType {
	return boxTypeAny
}
