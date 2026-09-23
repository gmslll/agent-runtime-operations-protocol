package core

import (
	"bytes"
	"fmt"
	"unicode/utf8"
)

func ValidateUTF8Offset(text string, offset int) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("text is not valid UTF-8")
	}
	if offset < 0 || offset > len(text) {
		return fmt.Errorf("UTF-8 byte offset %d is outside 0..%d", offset, len(text))
	}
	if offset < len(text) && !utf8.RuneStart(text[offset]) {
		return fmt.Errorf("UTF-8 byte offset %d is not a code point boundary", offset)
	}
	return nil
}

// TextAccumulator applies append-only output.delta events and accepts exact
// duplicate retransmission without appending it twice.
type TextAccumulator struct {
	value []byte
	seen  map[int][]byte
}

func NewTextAccumulator(initial string) (*TextAccumulator, error) {
	if !utf8.ValidString(initial) {
		return nil, fmt.Errorf("initial text is not valid UTF-8")
	}
	return &TextAccumulator{value: []byte(initial), seen: map[int][]byte{}}, nil
}

func (accumulator *TextAccumulator) Apply(offset int, delta string) error {
	if accumulator == nil {
		return fmt.Errorf("nil text accumulator")
	}
	if !utf8.ValidString(delta) {
		return fmt.Errorf("delta is not valid UTF-8")
	}
	if previous, exists := accumulator.seen[offset]; exists {
		if bytes.Equal(previous, []byte(delta)) {
			return nil
		}
		return fmt.Errorf("offset %d was retransmitted with different content", offset)
	}
	if offset != len(accumulator.value) {
		return fmt.Errorf("delta offset %d does not equal current UTF-8 byte length %d", offset, len(accumulator.value))
	}
	accumulator.seen[offset] = append([]byte(nil), delta...)
	accumulator.value = append(accumulator.value, delta...)
	return nil
}

func (accumulator *TextAccumulator) String() string { return string(accumulator.value) }
func (accumulator *TextAccumulator) Bytes() int     { return len(accumulator.value) }
