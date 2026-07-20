package chronicle

import (
	"encoding/json"
	"math/big"
	"reflect"
	"slices"
	"strings"
)

// ChangeOp describes what happened to a field between two points.
type ChangeOp uint8

const (
	// ChangeModified means the field exists on both sides with different
	// values.
	ChangeModified ChangeOp = iota
	// ChangeAdded means the field exists only on the later side.
	ChangeAdded
	// ChangeRemoved means the field exists only on the earlier side.
	ChangeRemoved
)

// String implements [fmt.Stringer].
func (o ChangeOp) String() string {
	switch o {
	case ChangeModified:
		return "modified"
	case ChangeAdded:
		return "added"
	case ChangeRemoved:
		return "removed"
	default:
		return "changeop(" + itoa(int(o)) + ")"
	}
}

// FieldChange is one field-level difference between two states.
type FieldChange struct {
	// Path locates the field as an RFC 6901 JSON Pointer: "/salary",
	// "/address/city", "/tags/0". The empty path refers to the whole document.
	// Keys containing "~" or "/" are escaped as "~0" and "~1".
	Path string
	// Op says whether the field was added, removed or modified.
	Op ChangeOp
	// Old is the value on the earlier side, nil for an addition. For a change
	// at a structural node it is the whole subtree, not a scalar.
	Old any
	// New is the value on the later side, nil for a removal.
	New any
}

// Delta is the set of field-level changes between two points in an entity's
// history, together with the records those points resolved to.
type Delta struct {
	// Kind and EntityID identify the entity.
	Kind, EntityID string
	// From and To are the points that were compared, as resolved — zero
	// fields in the caller's [As] have been replaced with the instants
	// actually used.
	From, To As
	// FromRecord is the record in force at From, or nil if the entity had no
	// state there.
	FromRecord *Record
	// ToRecord is the record in force at To, or nil if the entity had no state
	// there.
	ToRecord *Record
	// Changes are the differences, ordered by path. It is empty when the two
	// states are structurally identical, which is not the same as the two
	// records being the same record.
	//
	// Numbers compare by value rather than by notation: 1, 1.0 and 1e0 are the
	// same number and produce no change. Where two numbers genuinely differ,
	// Old and New carry each side in its original notation.
	Changes []FieldChange
}

// IsEmpty reports whether the two states were structurally identical.
func (d Delta) IsEmpty() bool { return len(d.Changes) == 0 }

// Paths returns the paths that changed, in order.
func (d Delta) Paths() []string {
	out := make([]string, len(d.Changes))
	for i, c := range d.Changes {
		out[i] = c.Path
	}
	return out
}

// Change returns the change at the given path, and whether there was one.
func (d Delta) Change(path string) (FieldChange, bool) {
	for _, c := range d.Changes {
		if c.Path == path {
			return c, true
		}
	}
	return FieldChange{}, false
}

// diffValues computes the structural difference between two decoded values,
// descending through objects and arrays.
//
// Objects are compared by key, so reordering keys is not a change. Arrays are
// compared by position, which is the documented limitation: inserting an
// element at the front of an array reports every subsequent element as
// modified plus one addition at the end, rather than a single insertion. Doing
// better needs an alignment heuristic — a longest-common-subsequence over
// values, or an identity field nominated per array — and a heuristic that is
// wrong in the cases it does not fit is worse than a rule that is simple and
// stated. See the package documentation.
func diffValues(path string, oldVal, newVal any, out *[]FieldChange) {
	oldMap, oldIsMap := oldVal.(map[string]any)
	newMap, newIsMap := newVal.(map[string]any)
	if oldIsMap && newIsMap {
		diffMaps(path, oldMap, newMap, out)
		return
	}

	oldArr, oldIsArr := oldVal.([]any)
	newArr, newIsArr := newVal.([]any)
	if oldIsArr && newIsArr {
		diffArrays(path, oldArr, newArr, out)
		return
	}

	// Either both sides are scalars, or the shape changed at this node — an
	// object became a string, an array became a number. Both are one change at
	// this path, carrying the whole of each side.
	if !equalScalars(oldVal, newVal) {
		*out = append(*out, FieldChange{Path: path, Op: ChangeModified, Old: oldVal, New: newVal})
	}
}

// equalScalars compares two leaf values, treating numbers as numbers.
//
// [encoding/json.Number] is a string, and comparing it as one would report a
// change of notation as a change of value: 1 versus 1.0, or 100 versus 1e2.
// JSON does not distinguish those, so neither does Diff — numbers compare by
// value, exactly, with no float64 round trip. A json.Number that does not
// parse (possible only from a codec that hand-builds one) falls back to string
// equality, which errs toward reporting a change — over-reporting is the
// recoverable direction for a change log.
func equalScalars(oldVal, newVal any) bool {
	oldNum, oldIsNum := oldVal.(json.Number)
	newNum, newIsNum := newVal.(json.Number)
	if oldIsNum && newIsNum {
		if oldNum == newNum {
			return true
		}
		a, okA := new(big.Rat).SetString(oldNum.String())
		b, okB := new(big.Rat).SetString(newNum.String())
		if !okA || !okB {
			return false
		}
		return a.Cmp(b) == 0
	}
	return reflect.DeepEqual(oldVal, newVal)
}

func diffMaps(path string, oldVal, newVal map[string]any, out *[]FieldChange) {
	keys := make([]string, 0, len(oldVal)+len(newVal))
	for k := range oldVal {
		keys = append(keys, k)
	}
	for k := range newVal {
		if _, ok := oldVal[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)

	for _, k := range keys {
		child := path + "/" + escapePointer(k)
		ov, inOld := oldVal[k]
		nv, inNew := newVal[k]
		switch {
		case inOld && !inNew:
			*out = append(*out, FieldChange{Path: child, Op: ChangeRemoved, Old: ov})
		case !inOld && inNew:
			*out = append(*out, FieldChange{Path: child, Op: ChangeAdded, New: nv})
		default:
			diffValues(child, ov, nv, out)
		}
	}
}

func diffArrays(path string, oldVal, newVal []any, out *[]FieldChange) {
	n := max(len(oldVal), len(newVal))
	for i := 0; i < n; i++ {
		child := path + "/" + itoa(i)
		switch {
		case i >= len(newVal):
			*out = append(*out, FieldChange{Path: child, Op: ChangeRemoved, Old: oldVal[i]})
		case i >= len(oldVal):
			*out = append(*out, FieldChange{Path: child, Op: ChangeAdded, New: newVal[i]})
		default:
			diffValues(child, oldVal[i], newVal[i], out)
		}
	}
}

// escapePointer applies RFC 6901 escaping to one JSON Pointer reference token.
func escapePointer(s string) string {
	if !strings.ContainsAny(s, "~/") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '~':
			b.WriteString("~0")
		case '/':
			b.WriteString("~1")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
