package mp4

import (
	"bytes"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type reflectionTestBox struct {
	mockBox
	FullBox    `mp4:"0,extend"`
	Dynamic    uint32 `mp4:"1,size=dynamic"`
	OptionalOn uint8  `mp4:"2,size=8,opt=0x000001"`
	DynamicOpt uint8  `mp4:"3,size=8,opt=dynamic"`
	ValueCount uint32 `mp4:"4,size=32"`
	Values     []byte `mp4:"5,size=8,len=dynamic"`
	V0Only     uint8  `mp4:"6,size=8,ver=0"`
	V1Only     uint8  `mp4:"7,size=8,ver=1"`
}

type reflectionAnyTestBox struct {
	AnyTypeBox
	FullBox `mp4:"0,extend"`
	Value   uint8 `mp4:"1,size=8"`
}

func (b *reflectionTestBox) GetFieldSize(name string, ctx Context) uint {
	if name == "Dynamic" {
		return 16
	}
	return b.mockBox.GetFieldSize(name, ctx)
}

func (b *reflectionTestBox) GetFieldLength(name string, ctx Context) uint {
	if name == "Values" {
		return uint(b.ValueCount)
	}
	return b.mockBox.GetFieldLength(name, ctx)
}

func (b *reflectionTestBox) IsOptFieldEnabled(name string, ctx Context) bool {
	if name == "DynamicOpt" {
		return b.Version == 0
	}
	if b.mockBox.DynOptMap != nil {
		return b.mockBox.IsOptFieldEnabled(name, ctx)
	}
	return false
}

func TestFieldMetadataRoundTrip(t *testing.T) {
	boxType := StrToBoxType("rflx")
	AddBoxDef(&reflectionTestBox{mockBox: mockBox{Type: boxType}}, 0, 1)

	for _, tc := range []struct {
		name string
		src  reflectionTestBox
		bin  []byte
	}{
		{
			name: "version zero uses fixed and dynamic optional fields",
			src: reflectionTestBox{
				mockBox: mockBox{Type: boxType},
				FullBox: FullBox{
					Version: 0,
					Flags:   [3]byte{0x00, 0x00, 0x01},
				},
				Dynamic:    0x1234,
				OptionalOn: 0x55,
				DynamicOpt: 0x66,
				ValueCount: 3,
				Values:     []byte{0x0a, 0x0b, 0x0c},
				V0Only:     0x77,
			},
			bin: []byte{
				0x00, 0x00, 0x00, 0x01,
				0x12, 0x34,
				0x55,
				0x66,
				0x00, 0x00, 0x00, 0x03,
				0x0a, 0x0b, 0x0c,
				0x77,
			},
		},
		{
			name: "version one excludes version-zero and flag-gated fields",
			src: reflectionTestBox{
				mockBox: mockBox{Type: boxType},
				FullBox: FullBox{
					Version: 1,
					Flags:   [3]byte{0x00, 0x00, 0x00},
				},
				Dynamic:    0x4321,
				ValueCount: 1,
				Values:     []byte{0x0d},
				V1Only:     0x88,
			},
			bin: []byte{
				0x01, 0x00, 0x00, 0x00,
				0x43, 0x21,
				0x00, 0x00, 0x00, 0x01,
				0x0d,
				0x88,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			n, err := Marshal(&buf, &tc.src, Context{})
			require.NoError(t, err)
			require.Equal(t, uint64(len(tc.bin)), n)
			require.Equal(t, tc.bin, buf.Bytes())

			dst := reflectionTestBox{mockBox: mockBox{Type: boxType}}
			n, err = Unmarshal(bytes.NewReader(tc.bin), uint64(len(tc.bin)), &dst, Context{})
			require.NoError(t, err)
			require.Equal(t, uint64(len(tc.bin)), n)
			assert.Equal(t, tc.src, dst)
		})
	}
}

func TestUnknownVersionKeepsUnsupportedBoxVersionError(t *testing.T) {
	boxType := StrToBoxType("rvuk")
	AddBoxDef(&reflectionTestBox{mockBox: mockBox{Type: boxType}}, 0)

	src := reflectionTestBox{
		mockBox: mockBox{Type: boxType},
		FullBox: FullBox{Version: 9},
	}

	var buf bytes.Buffer
	_, err := Marshal(&buf, &src, Context{})
	require.NoError(t, err)

	dst := reflectionTestBox{mockBox: mockBox{Type: boxType}}
	n, err := Unmarshal(bytes.NewReader(buf.Bytes()), uint64(buf.Len()), &dst, Context{})
	assert.Same(t, ErrUnsupportedBoxVersion, err)
	assert.Equal(t, uint64(0), n)
	assert.True(t, boxType.IsSupportedVersion(0, Context{}))
	assert.False(t, boxType.IsSupportedVersion(9, Context{}))
}

func TestBoxDefinitionRegistrationIsSafeForConcurrentReads(t *testing.T) {
	baseType := StrToBoxType("rthr")
	baseAnyType := StrToBoxType("rtha")
	overrideType := StrToBoxType("rovr")

	AddBoxDef(&reflectionTestBox{mockBox: mockBox{Type: overrideType}}, 0)
	require.False(t, overrideType.IsSupportedVersion(1, Context{}))
	AddBoxDef(&reflectionTestBox{mockBox: mockBox{Type: overrideType}}, 0, 1)
	require.True(t, overrideType.IsSupportedVersion(1, Context{}))

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			boxType := baseType
			boxType[3] = byte(i)
			AddBoxDef(&reflectionTestBox{mockBox: mockBox{Type: boxType}}, 0, 1)

			anyType := baseAnyType
			anyType[3] = byte(i)
			AddAnyTypeBoxDef(&reflectionAnyTestBox{AnyTypeBox: AnyTypeBox{Type: anyType}}, anyType, 0, 1)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			// Read registered types that were published by earlier iterations.
			// This avoids depending on scheduling while still exercising the
			// registry read/write synchronization continuously.
			boxType := baseType
			boxType[3] = byte((i + 1) % 100)
			_, _ = boxType.New(Context{})
			_ = boxType.IsSupportedVersion(1, Context{})

			anyType := baseAnyType
			anyType[3] = byte((i + 1) % 100)
			_, _ = anyType.New(Context{})
			_ = anyType.IsSupportedVersion(1, Context{})
		}
	}()

	wg.Wait()

	assert.True(t, overrideType.IsSupportedVersion(1, Context{}))
	for i := 0; i < 100; i++ {
		boxType := baseType
		boxType[3] = byte(i)
		box, err := boxType.New(Context{})
		require.NoError(t, err)
		assert.NotNil(t, box)
		assert.True(t, boxType.IsSupportedVersion(1, Context{}))

		anyType := baseAnyType
		anyType[3] = byte(i)
		anyBox, err := anyType.New(Context{})
		require.NoError(t, err)
		assert.Equal(t, anyType, anyBox.GetType())
		assert.True(t, anyType.IsSupportedVersion(1, Context{}))
	}
}
