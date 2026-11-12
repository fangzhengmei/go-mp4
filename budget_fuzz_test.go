package mp4

import (
	"bytes"
	"testing"
)

func FuzzReadBoxStructureWithBudget(f *testing.F) {
	f.Add([]byte{
		0x00, 0x00, 0x00, 0x20, 0x66, 0x74, 0x79, 0x70,
		0x6d, 0x70, 0x34, 0x32, 0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x20,
		0x6d, 0x6f, 0x6f, 0x76, 0x00, 0x00, 0x00, 0x08,
	}, uint64(1), uint64(1), uint64(1))

	f.Fuzz(func(t *testing.T, b []byte, maxBytes, maxDepth, maxBoxes uint64) {
		budget := &ReadBudget{
			MaxReadBytes: maxBytes % 64,
			MaxDepth:     maxDepth % 8,
			MaxBoxes:     maxBoxes % 16,
		}
		_, err := ReadBoxStructureWithBudget(bytes.NewReader(b), budget, func(h *ReadHandle) (interface{}, error) {
			if h.BoxInfo.IsSupportedType() {
				if _, err := h.Expand(); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		_ = err
	})
}
