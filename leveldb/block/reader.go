// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// This LevelDB Go implementation is based on LevelDB C++ implementation.
// Which contains the following header:
//   Copyright (c) 2011 The LevelDB Authors. All rights reserved.
//   Use of this source code is governed by a BSD-style license that can be
//   found in the LEVELDBCPP_LICENSE file. See the LEVELDBCPP_AUTHORS file
//   for names of contributors.

// Package block allows read and write sorted key/value.
package block

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/syndtr/goleveldb/leveldb/comparer"
	"github.com/syndtr/goleveldb/leveldb/errors"
)

// Reader represent a block reader.
type Reader struct {
	cmp comparer.BasicComparer

	buf, rbuf []byte

	restartLen   int
	restartStart int
}

// NewReader create new initialized block reader.
func NewReader(buf []byte, cmp comparer.BasicComparer) (b *Reader, err error) {
	if len(buf) < 8 {
		err = errors.ErrCorrupt("block to short")
		return
	}

	// Decode restart len
	restartLen := binary.LittleEndian.Uint32(buf[len(buf)-4:])

	// Calculate restart start offset
	restartStart := len(buf) - int(restartLen)*4 - 4
	if restartStart >= len(buf)-4 {
		err = errors.ErrCorrupt("bad restart offset in block")
		return
	}

	b = &Reader{
		cmp:          cmp,
		buf:          buf,
		rbuf:         buf[restartStart : len(buf)-4],
		restartLen:   int(restartLen),
		restartStart: restartStart,
	}
	return
}

// NewIterator create new iterator over the block.
func (b *Reader) NewIterator() *Iterator {
	if b.restartStart == 0 {
		return new(Iterator)
	}
	return &Iterator{b: b}
}

// InitIterator initialize given block iterator.
func (b *Reader) InitIterator(i *Iterator) {
	if b.restartStart == 0 {
		i.b = nil
	} else {
		i.b = b
	}
	i.err = nil
	i.ri = 0
	i.rr = nil
}

type keyVal struct {
	key, value []byte
}

type restartRange struct {
	raw []byte
	buf *bytes.Buffer

	kv  keyVal
	pos int

	cached bool
	cache  []keyVal
}

func (r *restartRange) next() (err error) {
	if r.cached && len(r.cache) > r.pos {
		r.kv = r.cache[r.pos]
		r.pos++
		return
	}

	if r.buf.Len() == 0 {
		return io.EOF
	}

	var nkey []byte

	// Read header
	var shared, nonShared, valueLen uint64
	shared, err = binary.ReadUvarint(r.buf)
	if err != nil || shared > uint64(len(r.kv.key)) {
		goto corrupt
	}
	nonShared, err = binary.ReadUvarint(r.buf)
	if err != nil {
		goto corrupt
	}
	valueLen, err = binary.ReadUvarint(r.buf)
	if err != nil {
		goto corrupt
	}
	if nonShared+valueLen > uint64(r.buf.Len()) {
		goto corrupt
	}

	if r.cached && r.pos > 0 {
		r.cache = append(r.cache, r.kv)
	}

	// Read content
	nkey = r.buf.Next(int(nonShared))
	if shared == 0 {
		r.kv.key = nkey
	} else {
		pkey := r.kv.key[:shared]
		key := make([]byte, shared+nonShared)
		copy(key, pkey)
		copy(key[shared:], nkey)
		r.kv.key = key
	}
	r.kv.value = r.buf.Next(int(valueLen))
	r.pos++
	return

corrupt:
	return errors.ErrCorrupt("bad entry in block")
}

func (r *restartRange) prev() (err error) {
	if r.pos <= 1 {
		r.pos = 0
		return io.EOF
	}

	r.pos--
	if r.cached {
		// this entry not cached yet
		if len(r.cache) == r.pos {
			r.cache = append(r.cache, r.kv)
		}
		r.kv = r.cache[r.pos-1]
		return
	}
	r.cached = true

	pos := r.pos
	r.reset()
	for r.pos < pos {
		err = r.next()
		if err != nil {
			if err == io.EOF {
				panic("not reached")
			}
			return
		}
	}
	return
}

func (r *restartRange) last() (err error) {
	if !r.cached {
		r.cached = true
		r.reset()
	}

	for {
		err = r.next()
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
	}
	return
}

func (r *restartRange) reset() {
	if r.pos > 0 {
		r.buf = bytes.NewBuffer(r.raw)
		r.pos = 0
	}
}

func (r *restartRange) key() []byte {
	return r.kv.key
}

func (r *restartRange) value() []byte {
	return r.kv.value
}

type Iterator struct {
	b   *Reader
	err error
	ri  int           // restart index
	rr  *restartRange // restart range
}

func (i *Iterator) getRestartOffset(idx int) (offset int, err error) {
	if idx >= i.b.restartLen {
		panic("out of range")
	}

	offset = int(binary.LittleEndian.Uint32(i.b.rbuf[idx*4:]))
	return
}

func (i *Iterator) getRestartKey(idx int) (key []byte, err error) {
	offset, err := i.getRestartOffset(idx)
	if err != nil {
		return
	}

	buf := i.b.buf[offset:]
	shared, n := binary.Uvarint(buf) // shared key
	buf = buf[n:]
	nonShared, n := binary.Uvarint(buf) // non-shared key
	buf = buf[n:]
	valueLen, n := binary.Uvarint(buf) // value len
	buf = buf[n:]

	if shared > 0 || nonShared+valueLen > uint64(len(buf)) {
		err = errors.ErrCorrupt("bad entry in block")
		return
	}

	key = buf[:nonShared]
	return
}

func (i *Iterator) getRestartRange(idx int) (r *restartRange, err error) {
	var start, end int
	start, err = i.getRestartOffset(idx)
	if err != nil {
		return
	}
	if start >= i.b.restartStart {
		goto corrupt
	}

	if idx+1 < i.b.restartLen {
		end, err = i.getRestartOffset(idx + 1)
		if err != nil {
			return
		}
		if end >= i.b.restartStart {
			goto corrupt
		}
	} else {
		end = i.b.restartStart
	}

	if start < end {
		r = &restartRange{raw: i.b.buf[start:end]}
		r.buf = bytes.NewBuffer(r.raw)
		return
	}

corrupt:
	return nil, errors.ErrCorrupt("bad restart range in block")
}

func (i *Iterator) setRestartRange() {
	i.rr, i.err = i.getRestartRange(i.ri)
}

func (i *Iterator) Empty() bool {
	return i.b == nil
}

func (i *Iterator) Valid() bool {
	return i.rr != nil
}

func (i *Iterator) First() bool {
	if i.Empty() {
		return false
	}
	i.ri = 0
	i.rr = nil
	return i.Next()
}

func (i *Iterator) Last() bool {
	if i.Empty() {
		return false
	}
	i.ri = i.b.restartLen
	i.rr = nil
	return i.Prev()
}

func (i *Iterator) Seek(key []byte) (r bool) {
	if i.err != nil || i.Empty() {
		return false
	}

	b := i.b

	j, k := 0, b.restartLen-1
	for j < k {
		h := j + (k-j+1)/2

		var rkey []byte
		rkey, i.err = i.getRestartKey(h)
		if i.err != nil {
			return false
		}

		if b.cmp.Compare(rkey, key) < 0 {
			j = h
		} else {
			k = h - 1
		}
	}

	i.ri = j
	i.rr = nil

	// linear scan for first 'key' >= 'target'
	for i.Next() {
		if b.cmp.Compare(i.rr.key(), key) >= 0 {
			return true
		}
	}
	return false
}

func (i *Iterator) Next() bool {
	if i.err != nil || i.Empty() || i.ri == i.b.restartLen {
		return false
	}

	// no restart range, create it
	if i.rr == nil {
		i.setRestartRange()
		if i.err != nil {
			return false
		}
	}

	i.err = i.rr.next()
	if i.err != nil {
		// reached end of restart range, advancing
		// restart index
		if i.err == io.EOF {
			i.err = nil
			i.ri++
			i.rr = nil
			return i.Next()
		}
		return false
	}

	return true
}

func (i *Iterator) Prev() bool {
	if i.err != nil || i.Empty() {
		return false
	}

	if i.rr == nil {
		if i.ri == 0 {
			return false
		}
		i.ri--
		i.setRestartRange()
		if i.err != nil {
			return false
		}
		i.err = i.rr.last()
		if i.err != nil {
			return false
		}
		return true
	}

	i.err = i.rr.prev()
	if i.err != nil {
		if i.err == io.EOF {
			i.err = nil
			i.rr = nil
			return i.Prev()
		}
		return false
	}

	return true
}

func (i *Iterator) Key() []byte {
	if i.rr == nil {
		return nil
	}
	return i.rr.key()
}

func (i *Iterator) Value() []byte {
	if i.rr == nil {
		return nil
	}
	return i.rr.value()
}

func (i *Iterator) Error() error { return i.err }
