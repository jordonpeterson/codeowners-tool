package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// This file turns policy bytes into a located value tree: every value and every
// object key remembers the byte offset it came from.
//
// Why not `json.Unmarshal` into structs with DisallowUnknownFields, which is the
// obvious way to get strictness for free? Three independent reasons, each fatal
// on its own:
//
//  1. DisallowUnknownFields is a property of encoding/json's STRUCT decode path.
//     The moment a type implements json.Unmarshaler — which the string-or-object
//     `ops` element must, in that design — the decoder hands it raw bytes and the
//     flag stops propagating. `on_zero_mtach` then sails through and applies the
//     default zero-match policy to every repo in the fleet, which is the one
//     failure the strictness rule exists to prevent (R-20).
//  2. It cannot coexist with the comment convention. Keys beginning with `_` and
//     the key `//` are ignored at every level, because JSON has no comments and
//     unknown fields are fatal; DisallowUnknownFields would reject `"_note"`.
//     Ignoring some unknown keys and rejecting others is a per-level decision the
//     decoder has no way to express.
//  3. It reports byte offsets, and only for some errors. Operators grep
//     `policy.json:14:22` out of a 100-repo log at 2am; nobody can use a byte
//     offset.
//
// Duplicate-key detection (below) needs a token-level pass regardless, since
// encoding/json silently keeps the LAST of two identical keys. Given that pass
// is mandatory, building the located tree in the same walk costs almost nothing
// and lets every later check report an exact position.

// value kinds, spelled as the byte that opens them in the source where there is
// one. 'z' is null, which has no distinguishing punctuation.
const (
	kObject = '{'
	kArray  = '['
	kString = 's'
	kNumber = 'n'
	kBool   = 'b'
	kNull   = 'z'
)

// jsonValue is one JSON value plus where in the file it was written.
type jsonValue struct {
	kind byte
	off  int    // byte offset of the value's first byte
	raw  string // exact source text of the value, for echoing it back verbatim
	str  string // kString
	num  json.Number
	b    bool
	// members is in source order. Duplicates are rejected before anything reads
	// this, so first-wins here is never observable.
	members []*member
	elems   []*jsonValue
}

// member is one key/value pair, with the key's own offset — errors about a
// field name must point at the name, not at its value.
type member struct {
	key    string
	keyOff int
	val    *jsonValue
}

// field returns the member with the given key, or nil.
func (v *jsonValue) field(name string) *member {
	if v == nil {
		return nil
	}
	for _, m := range v.members {
		if m.key == name {
			return m
		}
	}
	return nil
}

// describe names a value the way a policy author would say it out loud. Scalars
// are echoed literally because seeing your own typo is what makes the message
// land; composites are named by type, because quoting a whole array back at
// someone is not a readable error.
func (v *jsonValue) describe() string {
	switch v.kind {
	case kString:
		return fmt.Sprintf("the string %q", v.str)
	case kNumber:
		return "the number " + v.num.String()
	case kBool:
		return "the boolean " + v.raw
	case kNull:
		return "null"
	case kArray:
		return "an array"
	case kObject:
		return "an object"
	}
	return v.raw
}

// show is describe's compact form, used where the surrounding sentence already
// supplies the grammar.
func (v *jsonValue) show() string {
	switch v.kind {
	case kArray:
		return "an array"
	case kObject:
		return "an object"
	}
	return v.raw
}

// scanner walks the token stream once, building the located tree and collecting
// duplicate keys as it goes.
type scanner struct {
	src  []byte
	file string
	dec  *json.Decoder
	dups []error
}

// scanSource parses src into a located tree.
//
// A syntax error is returned alone and immediately: once the token stream is
// broken every subsequent problem is invented, and a wall of phantom errors is
// worse for the operator than the one real one. Duplicate keys come back
// separately because they are not syntax — the document parses — but they are
// structural enough that validating a tree with ambiguous values would report
// problems about whichever value happened to win.
func scanSource(src []byte, file string) (root *jsonValue, dups []error, syntax *Error) {
	s := &scanner{src: src, file: file}
	s.dec = json.NewDecoder(bytes.NewReader(src))
	// Without UseNumber, `1.5` and `1` both arrive as float64 and a fractional
	// version becomes indistinguishable from a whole one.
	s.dec.UseNumber()

	if len(bytes.TrimSpace(src)) == 0 {
		return nil, nil, s.errAt(0, "the policy file is empty; a policy is at minimum {\"version\": 1, \"ops\": [...]}")
	}
	v, err := s.value()
	if err != nil {
		return nil, nil, s.syntaxError(err)
	}
	// Trailing content means two documents in one file — usually a generator that
	// concatenated fragments without merging them. Silently reading the first
	// would run half the policy.
	if _, err := s.dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, nil, s.syntaxError(err)
		}
		return nil, nil, s.errAt(int(s.dec.InputOffset()), "unexpected content after the end of the policy object; a policy file holds exactly one JSON object")
	}
	if v.kind != kObject {
		return nil, nil, s.errAt(v.off, "the top-level value must be a JSON object, got %s; a policy file starts with `{` and sets \"version\" and \"ops\"", v.describe())
	}
	return v, s.dups, nil
}

// value consumes exactly one JSON value, recording where it started.
func (s *scanner) value() (*jsonValue, error) {
	start := s.valueStart()
	tok, err := s.dec.Token()
	if err != nil {
		return nil, err
	}
	v := &jsonValue{off: start}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v.kind = kObject
			err = s.object(v)
		case '[':
			v.kind = kArray
			err = s.array(v)
		default:
			// A closing delimiter can only arrive here if object/array below
			// mis-tracked its own extent, which would be a bug in this file.
			err = fmt.Errorf("unexpected %q", t)
		}
		if err != nil {
			return nil, err
		}
	case string:
		v.kind, v.str = kString, t
	case json.Number:
		v.kind, v.num = kNumber, t
	case bool:
		v.kind, v.b = kBool, t
	case nil:
		v.kind = kNull
	}
	v.raw = string(s.src[start:min(int(s.dec.InputOffset()), len(s.src))])
	return v, nil
}

func (s *scanner) object(v *jsonValue) error {
	seen := make(map[string]*member)
	for s.dec.More() {
		keyOff := s.valueStart()
		tok, err := s.dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("object key is not a string")
		}
		val, err := s.value()
		if err != nil {
			return err
		}
		m := &member{key: key, keyOff: keyOff, val: val}
		if prev, dup := seen[key]; dup {
			// encoding/json would keep the LAST of these without a word. A
			// generator concatenating fragments produces exactly this, and it is
			// invisible in review because both lines are individually correct —
			// so the message has to show BOTH values, not just name the key.
			s.dups = append(s.dups, s.errAt(keyOff,
				"duplicate key %q: this object sets it twice, first to %s and then to %s; one of the two would be discarded silently and the file gives no sign which",
				key, prev.val.show(), val.show()))
			continue
		}
		seen[key] = m
		v.members = append(v.members, m)
	}
	_, err := s.dec.Token() // the closing '}'
	return err
}

func (s *scanner) array(v *jsonValue) error {
	for s.dec.More() {
		e, err := s.value()
		if err != nil {
			return err
		}
		v.elems = append(v.elems, e)
	}
	_, err := s.dec.Token() // the closing ']'
	return err
}

// valueStart finds the first byte of the value (or key) that comes next.
//
// InputOffset marks the END of the token just returned, so the bytes between it
// and the next value are structural: whitespace, the `:` after a key, the `,`
// between elements. No JSON value can begin with any of them, so skipping them
// lands exactly on the first byte the author typed.
func (s *scanner) valueStart() int {
	i := int(s.dec.InputOffset())
	for i < len(s.src) {
		switch s.src[i] {
		case ' ', '\t', '\r', '\n', ':', ',':
			i++
		default:
			return i
		}
	}
	return i
}

// syntaxError converts a decoder failure into a located policy error.
//
// encoding/json's syntax messages ("invalid character ']' looking for beginning
// of value") describe the input rather than Go's types, so they survive UX rule
// 5 and are genuinely the most precise thing available. What does not survive is
// the byte offset, which is why every branch here resolves one.
func (s *scanner) syntaxError(err error) *Error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return s.errAt(len(s.src), "the policy file ends in the middle of a value; a bracket or brace is unclosed")
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		// SyntaxError.Offset counts bytes CONSUMED, so the offending byte is the
		// one before it.
		return s.errAt(int(se.Offset)-1, "invalid JSON: %s", se.Error())
	}
	return s.errAt(int(s.dec.InputOffset()), "invalid JSON: %s", err.Error())
}

func (s *scanner) errAt(off int, format string, args ...any) *Error {
	line, col := lineCol(s.src, off)
	return &Error{File: s.file, Line: line, Col: col, OpIndex: -1, Msg: fmt.Sprintf(format, args...)}
}

// lineCol converts a byte offset into the 1-based line and column an operator
// can act on. encoding/json deals only in offsets; leaving them unconverted is
// the whole file:line:col feature failing quietly, and it shows up as `Line: 0`.
func lineCol(src []byte, off int) (line, col int) {
	if off < 0 {
		off = 0
	}
	if off > len(src) {
		off = len(src)
	}
	line, lastNL := 1, -1
	for i := 0; i < off; i++ {
		if src[i] == '\n' {
			line++
			lastNL = i
		}
	}
	return line, off - lastNL
}

// ignoredKey reports whether a key is a comment rather than a field.
//
// JSON has no comments and unknown fields are fatal, so without this escape
// hatch the universal `"_comment"` convention would be ILLEGAL in a policy file.
// It does not weaken typo detection: `on_zero_mtach` does not start with `_`.
func ignoredKey(k string) bool {
	return k == "//" || strings.HasPrefix(k, "_")
}
