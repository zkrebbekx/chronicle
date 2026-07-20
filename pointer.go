package chronicle

import "strings"

// This file is the one place chronicle interprets an RFC 6901 JSON Pointer.
// [Log.Diff] emits paths in this grammar (see escapePointer, called from
// diff.go) and [Log.FieldHistory] reads one back in it, so they must agree on
// exactly what a token is and how "~" and "/" escape. Keeping both directions
// here is what keeps the two features from drifting into two dialects.

// escapePointer applies RFC 6901 escaping to one JSON Pointer reference token:
// "~" becomes "~0" and "/" becomes "~1". It is the inverse of unescapePointer.
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

// parsePointer splits an RFC 6901 JSON Pointer into its decoded reference
// tokens. The empty string is the whole document and yields no tokens. Any
// other pointer must begin with "/", and every "~" within a token must be
// followed by "0" or "1"; a pointer that breaks either rule is malformed and
// returns [ErrInvalidPath], which is deliberately distinct from a well-formed
// pointer that simply matches nothing (that is not an error, it is an empty
// result — see [Log.FieldHistory]).
func parsePointer(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if path[0] != '/' {
		return nil, &PathError{Path: path, Reason: "a non-empty JSON Pointer must begin with '/'"}
	}
	// The leading '/' introduces the first token, so dropping the first split
	// element (always "") leaves one token per reference token, including a
	// trailing empty token for a pointer ending in '/'.
	raw := strings.Split(path, "/")[1:]
	tokens := make([]string, len(raw))
	for i, tok := range raw {
		decoded, ok := unescapePointer(tok)
		if !ok {
			return nil, &PathError{Path: path, Reason: "'~' must be followed by '0' or '1'"}
		}
		tokens[i] = decoded
	}
	return tokens, nil
}

// unescapePointer reverses escapePointer for one reference token: "~1" becomes
// "/", "~0" becomes "~". It reports false for a "~" that is not part of a valid
// escape, which RFC 6901 forbids.
func unescapePointer(tok string) (string, bool) {
	if !strings.Contains(tok, "~") {
		return tok, true
	}
	var b strings.Builder
	b.Grow(len(tok))
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if c != '~' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(tok) {
			return "", false
		}
		switch tok[i+1] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", false
		}
		i++
	}
	return b.String(), true
}

// valueAtPointer resolves the decoded reference tokens against a decoded JSON
// structure and returns the value found there.
//
// The returned present bool is the whole reason this is not a plain lookup: a
// path that runs off the end of the structure — a key not in an object, an
// index past an array, a token that tries to descend into a scalar — is absent,
// reported as (nil, false). A path that lands on an explicit JSON null is
// present, reported as (nil, true). Those are different facts, and
// [Log.FieldHistory] keeps them apart. The empty token list refers to the whole
// document, which is always present.
func valueAtPointer(root any, tokens []string) (value any, present bool) {
	cur := root
	for _, tok := range tokens {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[tok]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, ok := arrayIndex(tok, len(node))
			if !ok {
				return nil, false
			}
			cur = node[idx]
		default:
			// A scalar (or a JSON null) with tokens still to consume: the path
			// asks to descend into something that has no children.
			return nil, false
		}
	}
	return cur, true
}

// arrayIndex parses an RFC 6901 array reference token to an in-range index.
//
// The grammar is strict — "0", or a non-zero digit followed by digits — so a
// leading zero, a sign, "-" (the "end of array" token, which never names an
// existing element), or anything non-numeric is not an index. None of those is
// an error here: a token that is not a valid in-range index just means the path
// matches nothing at this node, the same absent result as a missing object key.
func arrayIndex(tok string, length int) (int, bool) {
	if tok == "" {
		return 0, false
	}
	if tok == "0" {
		return 0, length > 0
	}
	if tok[0] < '1' || tok[0] > '9' {
		return 0, false
	}
	n := 0
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n >= length {
			// Past the end, and large tokens cannot come back into range, so
			// stop before n can overflow on a pathological input.
			return 0, false
		}
	}
	return n, true
}
