package mp4

// fieldCondition contains the static tag predicates which decide whether a
// reflected field belongs in the current full-box version and flags.
type fieldCondition struct {
	version  uint8
	nVersion uint8
	optFlag  uint32
	nOptFlag uint32
}

func (f *field) condition() fieldCondition {
	return fieldCondition{
		version:  f.version,
		nVersion: f.nVersion,
		optFlag:  f.optFlag,
		nOptFlag: f.nOptFlag,
	}
}

func (c fieldCondition) matchesVersion(version uint8) bool {
	if version == anyVersion {
		return true
	}
	if c.version != anyVersion && version != c.version {
		return false
	}
	if c.nVersion != anyVersion && version == c.nVersion {
		return false
	}
	return true
}

func (c fieldCondition) matchesFlags(flags uint32) bool {
	if c.optFlag != 0 && flags&c.optFlag == 0 {
		return false
	}
	if c.nOptFlag != 0 && flags&c.nOptFlag != 0 {
		return false
	}
	return true
}
