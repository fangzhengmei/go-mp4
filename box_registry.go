package mp4

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"sync"
)

type boxDef struct {
	dataType reflect.Type
	versions []uint8
	isTarget func(Context) bool
	fields   []*field
}

var (
	boxMapMu sync.RWMutex
	boxMap   = make(map[BoxType][]boxDef, 64)
)

// AddBoxDef registers a concrete box definition and keeps it available to
// users extending the package. Registrations are safe to run concurrently with
// box construction and codec operations.
func AddBoxDef(payload IBox, versions ...uint8) {
	addBoxDef(payload.GetType(), newBoxDef(payload, nil, versions))
}

// AddBoxDefEx registers a context-specific concrete box definition.
func AddBoxDefEx(payload IBox, isTarget func(Context) bool, versions ...uint8) {
	addBoxDef(payload.GetType(), newBoxDef(payload, isTarget, versions))
}

// AddAnyTypeBoxDef registers an IAnyType box under the specified BoxType.
func AddAnyTypeBoxDef(payload IAnyType, boxType BoxType, versions ...uint8) {
	addBoxDef(boxType, newBoxDef(payload, nil, versions))
}

// AddAnyTypeBoxDefEx registers a context-specific IAnyType box definition.
func AddAnyTypeBoxDefEx(payload IAnyType, boxType BoxType, isTarget func(Context) bool, versions ...uint8) {
	addBoxDef(boxType, newBoxDef(payload, isTarget, versions))
}

func newBoxDef(payload IImmutableBox, isTarget func(Context) bool, versions []uint8) boxDef {
	return boxDef{
		dataType: reflect.TypeOf(payload).Elem(),
		versions: versions,
		isTarget: isTarget,
		fields:   buildFields(payload),
	}
}

func addBoxDef(boxType BoxType, def boxDef) {
	boxMapMu.Lock()
	defer boxMapMu.Unlock()

	// Replace the whole slice so a reader sees a complete immutable snapshot
	// instead of an in-progress append or a reused backing array.
	oldDefs := boxMap[boxType]
	newDefs := make([]boxDef, len(oldDefs)+1)
	copy(newDefs, oldDefs)
	newDefs[len(oldDefs)] = def
	boxMap[boxType] = newDefs
}

var itemBoxFields = buildFields(&Item{})

func (boxType BoxType) getBoxDef(ctx Context) *boxDef {
	boxMapMu.RLock()
	defer boxMapMu.RUnlock()

	boxDefs := boxMap[boxType]
	for i := len(boxDefs) - 1; i >= 0; i-- {
		def := boxDefs[i]
		if def.isTarget == nil || def.isTarget(ctx) {
			return &def
		}
	}
	if ctx.UnderIlst {
		typeID := int(binary.BigEndian.Uint32(boxType[:]))
		if typeID >= 1 && typeID <= ctx.QuickTimeKeysMetaEntryCount {
			return &boxDef{
				dataType: reflect.TypeOf(Item{}),
				isTarget: isIlstMetaContainer,
				fields:   itemBoxFields,
			}
		}
	}
	return nil
}

func (boxType BoxType) IsSupported(ctx Context) bool {
	return boxType.getBoxDef(ctx) != nil
}

func (boxType BoxType) New(ctx Context) (IBox, error) {
	boxDef := boxType.getBoxDef(ctx)
	if boxDef == nil {
		return nil, ErrBoxInfoNotFound
	}

	box, ok := reflect.New(boxDef.dataType).Interface().(IBox)
	if !ok {
		return nil, fmt.Errorf("box type not implements IBox interface: %s", boxType.String())
	}

	anyTypeBox, ok := box.(IAnyType)
	if ok {
		anyTypeBox.SetType(boxType)
	}

	return box, nil
}

func (boxType BoxType) GetSupportedVersions(ctx Context) ([]uint8, error) {
	boxDef := boxType.getBoxDef(ctx)
	if boxDef == nil {
		return nil, ErrBoxInfoNotFound
	}
	return boxDef.versions, nil
}

func (boxType BoxType) IsSupportedVersion(ver uint8, ctx Context) bool {
	boxDef := boxType.getBoxDef(ctx)
	if boxDef == nil {
		return false
	}
	if len(boxDef.versions) == 0 {
		return true
	}
	for _, sver := range boxDef.versions {
		if ver == sver {
			return true
		}
	}
	return false
}
