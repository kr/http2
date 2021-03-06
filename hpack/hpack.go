// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

// Package hpack implements HPACK, a compression format for
// efficiently representing HTTP header fields in the context of HTTP/2.
//
// See http://tools.ietf.org/html/draft-ietf-httpbis-header-compression-09
package hpack

import (
	"bytes"
	"errors"
	"fmt"
)

// A DecodingError is something the spec defines as a decoding error.
type DecodingError struct {
	Err error
}

func (de DecodingError) Error() string {
	return fmt.Sprintf("decoding error: %v", de.Err)
}

// An InvalidIndexError is returned when an encoder references a table
// entry before the static table or after the end of the dynamic table.
type InvalidIndexError int

func (e InvalidIndexError) Error() string {
	return fmt.Sprintf("invalid indexed representation index %d", int(e))
}

// A HeaderField is a name-value pair. Both the name and value are
// treated as opaque sequences of octets.
type HeaderField struct {
	Name, Value string

	// Sensitive means that this header field should never be
	// indexed.
	Sensitive bool
}

func (hf *HeaderField) size() uint32 {
	// http://http2.github.io/http2-spec/compression.html#rfc.section.4.1
	// "The size of the dynamic table is the sum of the size of
	// its entries.  The size of an entry is the sum of its name's
	// length in octets (as defined in Section 5.2), its value's
	// length in octets (see Section 5.2), plus 32.  The size of
	// an entry is calculated using the length of the name and
	// value without any Huffman encoding applied."

	// This can overflow if somebody makes a large HeaderField
	// Name and/or Value by hand, but we don't care, because that
	// won't happen on the wire because the encoding doesn't allow
	// it.
	return uint32(len(hf.Name) + len(hf.Value) + 32)
}

// A Decoder is the decoding context for incremental processing of
// header blocks.
type Decoder struct {
	dynTab dynamicTable
	emit   func(f HeaderField)

	// buf is the unparsed buffer. It's only written to
	// saveBuf if it was truncated in the middle of a header
	// block. Because it's usually not owned, we can only
	// process it under Write.
	buf     []byte // usually not owned
	saveBuf bytes.Buffer
}

func NewDecoder(maxSize uint32, emitFunc func(f HeaderField)) *Decoder {
	d := &Decoder{
		emit: emitFunc,
	}
	d.dynTab.allowedMaxSize = maxSize
	d.dynTab.setMaxSize(maxSize)
	return d
}

// TODO: add method *Decoder.Reset(maxSize, emitFunc) to let callers re-use Decoders and their
// underlying buffers for garbage reasons.

func (d *Decoder) SetMaxDynamicTableSize(v uint32) {
	d.dynTab.setMaxSize(v)
}

// SetAllowedMaxDynamicTableSize sets the upper bound that the encoded
// stream (via dynamic table size updates) may set the maximum size
// to.
func (d *Decoder) SetAllowedMaxDynamicTableSize(v uint32) {
	d.dynTab.allowedMaxSize = v
}

type dynamicTable struct {
	// s is the FIFO described at
	// http://http2.github.io/http2-spec/compression.html#rfc.section.2.3.2
	// The newest (low index) is append at the end, and items are
	// evicted from the front.
	ents           []HeaderField
	size           uint32
	maxSize        uint32 // current maxSize
	allowedMaxSize uint32 // maxSize may go up to this, inclusive
}

func (dt *dynamicTable) setMaxSize(v uint32) {
	dt.maxSize = v
	dt.evict()
}

// TODO: change dynamicTable to be a struct with a slice and a size int field,
// per http://http2.github.io/http2-spec/compression.html#rfc.section.4.1:
//
//
// Then make add increment the size. maybe the max size should move from Decoder to
// dynamicTable and add should return an ok bool if there was enough space.
//
// Later we'll need a remove operation on dynamicTable.

func (dt *dynamicTable) add(f HeaderField) {
	dt.ents = append(dt.ents, f)
	dt.size += f.size()
	dt.evict()
}

// If we're too big, evict old stuff (front of the slice)
func (dt *dynamicTable) evict() {
	base := dt.ents // keep base pointer of slice
	for dt.size > dt.maxSize {
		dt.size -= dt.ents[0].size()
		dt.ents = dt.ents[1:]
	}

	// Shift slice contents down if we evicted things.
	if len(dt.ents) != len(base) {
		copy(base, dt.ents)
		dt.ents = base[:len(dt.ents)]
	}
}

func (d *Decoder) maxTableIndex() int {
	return len(d.dynTab.ents) + len(staticTable)
}

func (d *Decoder) at(i uint64) (hf HeaderField, ok bool) {
	if i < 1 {
		return
	}
	if i > uint64(d.maxTableIndex()) {
		return
	}
	if i <= uint64(len(staticTable)) {
		return staticTable[i-1], true
	}
	dents := d.dynTab.ents
	return dents[len(dents)-(int(i)-len(staticTable))], true
}

// Decode decodes an entire block.
//
// TODO: remove this method and make it incremental later? This is
// easier for debugging now.
func (d *Decoder) DecodeFull(p []byte) ([]HeaderField, error) {
	var hf []HeaderField
	saveFunc := d.emit
	defer func() { d.emit = saveFunc }()
	d.emit = func(f HeaderField) { hf = append(hf, f) }
	if _, err := d.Write(p); err != nil {
		return nil, err
	}
	if err := d.Close(); err != nil {
		return nil, err
	}
	return hf, nil
}

func (d *Decoder) Close() error {
	if d.saveBuf.Len() > 0 {
		d.saveBuf.Reset()
		return DecodingError{errors.New("truncated headers")}
	}
	return nil
}

func (d *Decoder) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		// Prevent state machine CPU attacks (making us redo
		// work up to the point of finding out we don't have
		// enough data)
		return
	}
	// Only copy the data if we have to. Optimistically assume
	// that p will contain a complete header block.
	if d.saveBuf.Len() == 0 {
		d.buf = p
	} else {
		d.saveBuf.Write(p)
		d.buf = d.saveBuf.Bytes()
		d.saveBuf.Reset()
	}

	for len(d.buf) > 0 {
		err = d.parseHeaderFieldRepr()
		if err != nil {
			if err == errNeedMore {
				err = nil
				d.saveBuf.Write(d.buf)
			}
			break
		}
	}

	return len(p), err
}

// errNeedMore is an internal sentinel error value that means the
// buffer is truncated and we need to read more data before we can
// continue parsing.
var errNeedMore = errors.New("need more data")

type indexType int

const (
	indexedTrue indexType = iota
	indexedFalse
	indexedNever
)

func (v indexType) indexed() bool   { return v == indexedTrue }
func (v indexType) sensitive() bool { return v == indexedNever }

// returns errNeedMore if there isn't enough data available.
// any other error is atal.
// consumes d.buf iff it returns nil.
// precondition: must be called with len(d.buf) > 0
func (d *Decoder) parseHeaderFieldRepr() error {
	b := d.buf[0]
	switch {
	case b&128 != 0:
		// Indexed representation.
		// High bit set?
		// http://http2.github.io/http2-spec/compression.html#rfc.section.6.1
		return d.parseFieldIndexed()
	case b&192 == 64:
		// 6.2.1 Literal Header Field with Incremental Indexing
		// 0b10xxxxxx: top two bits are 10
		// http://http2.github.io/http2-spec/compression.html#rfc.section.6.2.1
		return d.parseFieldLiteral(6, indexedTrue)
	case b&240 == 0:
		// 6.2.2 Literal Header Field without Indexing
		// 0b0000xxxx: top four bits are 0000
		// http://http2.github.io/http2-spec/compression.html#rfc.section.6.2.2
		return d.parseFieldLiteral(4, indexedFalse)
	case b&240 == 16:
		// 6.2.3 Literal Header Field never Indexed
		// 0b0001xxxx: top four bits are 0001
		// http://http2.github.io/http2-spec/compression.html#rfc.section.6.2.3
		return d.parseFieldLiteral(4, indexedNever)
	case b&224 == 32:
		// 6.3 Dynamic Table Size Update
		// Top three bits are '001'.
		// http://http2.github.io/http2-spec/compression.html#rfc.section.6.3
		return d.parseDynamicTableSizeUpdate()
	}

	return DecodingError{errors.New("invalid encoding")}
}

// (same invariants and behavior as parseHeaderFieldRepr)
func (d *Decoder) parseFieldIndexed() error {
	buf := d.buf
	idx, buf, err := readVarInt(7, buf)
	if err != nil {
		return err
	}
	hf, ok := d.at(idx)
	if !ok {
		return DecodingError{InvalidIndexError(idx)}
	}
	d.emit(HeaderField{Name: hf.Name, Value: hf.Value})
	d.buf = buf
	return nil
}

// (same invariants and behavior as parseHeaderFieldRepr)
func (d *Decoder) parseFieldLiteral(n uint8, it indexType) error {
	buf := d.buf
	nameIdx, buf, err := readVarInt(n, buf)
	if err != nil {
		return err
	}

	var hf HeaderField
	if nameIdx > 0 {
		ihf, ok := d.at(nameIdx)
		if !ok {
			return DecodingError{InvalidIndexError(nameIdx)}
		}
		hf.Name = ihf.Name
	} else {
		hf.Name, buf, err = readString(buf)
		if err != nil {
			return err
		}
	}
	hf.Value, buf, err = readString(buf)
	if err != nil {
		return err
	}
	d.buf = buf
	if it.indexed() {
		d.dynTab.add(hf)
	}
	hf.Sensitive = it.sensitive()
	d.emit(hf)
	return nil
}

// (same invariants and behavior as parseHeaderFieldRepr)
func (d *Decoder) parseDynamicTableSizeUpdate() error {
	buf := d.buf
	size, buf, err := readVarInt(5, buf)
	if err != nil {
		return err
	}
	if size > uint64(d.dynTab.allowedMaxSize) {
		return DecodingError{errors.New("dynamic table size update too large")}
	}
	d.dynTab.setMaxSize(uint32(size))
	d.buf = buf
	return nil
}

var errVarintOverflow = DecodingError{errors.New("varint integer overflow")}

// readVarInt reads an unsigned variable length integer off the
// beginning of p. n is the parameter as described in
// http://http2.github.io/http2-spec/compression.html#rfc.section.5.1.
//
// n must always be between 1 and 8.
//
// The returned remain buffer is either a smaller suffix of p, or err != nil.
// The error is errNeedMore if p doesn't contain a complete integer.
func readVarInt(n byte, p []byte) (i uint64, remain []byte, err error) {
	if n < 1 || n > 8 {
		panic("bad n")
	}
	if len(p) == 0 {
		return 0, p, errNeedMore
	}
	i = uint64(p[0])
	if n < 8 {
		i &= (1 << uint64(n)) - 1
	}
	if i < (1<<uint64(n))-1 {
		return i, p[1:], nil
	}

	origP := p
	p = p[1:]
	var m uint64
	for len(p) > 0 {
		b := p[0]
		p = p[1:]
		i += uint64(b&127) << m
		if b&128 == 0 {
			return i, p, nil
		}
		m += 7
		if m >= 63 { // TODO: proper overflow check. making this up.
			return 0, origP, errVarintOverflow
		}
	}
	return 0, origP, errNeedMore
}

func readString(p []byte) (s string, remain []byte, err error) {
	if len(p) == 0 {
		return "", p, errNeedMore
	}
	isHuff := p[0]&128 != 0
	strLen, p, err := readVarInt(7, p)
	if err != nil {
		return "", p, err
	}
	if uint64(len(p)) < strLen {
		return "", p, errNeedMore
	}
	if !isHuff {
		return string(p[:strLen]), p[strLen:], nil
	}

	// TODO: optimize this garbage:
	var buf bytes.Buffer
	if _, err := HuffmanDecode(&buf, p[:strLen]); err != nil {
		return "", nil, err
	}
	return buf.String(), p[strLen:], nil
}
