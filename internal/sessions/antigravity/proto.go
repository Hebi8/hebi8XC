package antigravity

import (
	"encoding/binary"
	"errors"
	"unicode/utf8"
)

var errProto = errors.New("invalid Antigravity protobuf")

// proto is a view of a protobuf message. Reads are bounded and unknown fields
// are skipped by length, including large attachments and thinking signatures.
// Only wire types used by proto3 are needed; no generated schema is vendored.
type proto struct {
	read         func(offset, size int64) ([]byte, error)
	offset, size int64
}

type field struct {
	wire  uint64
	value uint64
	data  proto
}

func (p proto) find(number uint64) (field, error) {
	var found field
	for pos := int64(0); pos < p.size; {
		b, err := p.read(p.offset+pos, min(20, p.size-pos))
		if err != nil {
			return field{}, err
		}
		tag, n := binary.Uvarint(b)
		if n <= 0 || tag>>3 == 0 || tag>>3 > (1<<29)-1 {
			return field{}, errProto
		}
		pos += int64(n)
		f := field{wire: tag & 7}
		var length uint64
		switch f.wire {
		case 0, 2:
			v, m := binary.Uvarint(b[n:])
			if m <= 0 {
				return field{}, errProto
			}
			pos += int64(m)
			if f.wire == 0 {
				f.value = v
			} else {
				length = v
			}
		case 1:
			length = 8
		case 5:
			length = 4
		default:
			return field{}, errProto
		}
		if length > uint64(p.size-pos) {
			return field{}, errProto
		}
		f.data = proto{read: p.read, offset: p.offset + pos, size: int64(length)}
		pos += int64(length)
		if tag>>3 == number {
			found = f
		}
	}
	return found, nil
}

func (p proto) child(number uint64) (proto, error) {
	f, err := p.find(number)
	if err != nil {
		return proto{}, err
	}
	if f.data.read != nil && f.wire != 2 {
		return proto{}, errProto
	}
	return f.data, nil
}

func (p proto) integer(number uint64) (uint64, error) {
	f, err := p.find(number)
	if err == nil && f.wire != 0 {
		err = errProto
	}
	return f.value, err
}

func (p proto) text(limit int) (string, bool, error) {
	if p.size == 0 {
		return "", false, nil
	}
	b, err := p.read(p.offset, min(p.size, int64(utf8.UTFMax*(limit+1))))
	if err != nil {
		return "", false, err
	}
	end := 0
	for n := 0; end < len(b) && n < limit; n++ {
		r, size := utf8.DecodeRune(b[end:])
		if r == utf8.RuneError && size == 1 {
			return "", false, errProto
		}
		end += size
	}
	return string(b[:end]), int64(end) < p.size, nil
}

func (p proto) timestamp(number uint64) (int64, error) {
	t, err := p.child(number)
	if err != nil {
		return 0, err
	}
	sec, err := t.integer(1)
	if err != nil {
		return 0, err
	}
	nanos, err := t.integer(2)
	if err != nil {
		return 0, err
	}
	return int64(sec)*1000 + int64(nanos)/1_000_000, nil
}
