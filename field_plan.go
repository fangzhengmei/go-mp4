package mp4

import "reflect"

// fieldInstance is the per-codec plan derived from an immutable field
// definition. Dynamic sizes and lengths vary by box state and therefore never
// mutate the cached definition.
type fieldInstance struct {
	field
	cfo ICustomFieldObject
}

func (fi *fieldInstance) condition() fieldCondition {
	return fi.field.condition()
}

// resolveFieldInstance resolves dynamic metadata for one field. A nested
// custom-field object owns its own dynamic values; otherwise the root box owns
// them.
func resolveFieldInstance(f *field, box IImmutableBox, parent reflect.Value, ctx Context) *fieldInstance {
	fi := fieldInstance{
		field: *f,
	}

	cfo, ok := parent.Addr().Interface().(ICustomFieldObject)
	if ok {
		fi.cfo = cfo
	} else {
		fi.cfo = box
	}

	if fi.is(fieldSizeDynamic) {
		fi.size = fi.cfo.GetFieldSize(fi.name, ctx)
	}

	if fi.is(fieldLengthDynamic) {
		fi.length = fi.cfo.GetFieldLength(fi.name, ctx)
	}

	return &fi
}

func isTargetField(box IImmutableBox, fi *fieldInstance, ctx Context) bool {
	condition := fi.condition()
	if !condition.matchesVersion(box.GetVersion()) {
		return false
	}

	if !condition.matchesFlags(box.GetFlags()) {
		return false
	}

	if fi.is(fieldOptDynamic) && !fi.cfo.IsOptFieldEnabled(fi.name, ctx) {
		return false
	}

	return true
}
