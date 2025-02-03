package pieces

import (
	"errors"
	"sync"
)

var (
	ErrInvalidBitfield = errors.New("invalid bitfield")
	ErrInvalidPosition = errors.New("invalid bitfield position")
)

type Bitfield struct {
	m           sync.RWMutex
	numPieces   uint32
	extraFields uint32
	bts         []byte
}

func (b *Bitfield) Raw() []byte {
	b.m.RLock()
	defer b.m.RUnlock()

	bts := make([]byte, len(b.bts))
	copy(bts, b.bts)

	return bts
}

func (b *Bitfield) Overwrite(bts []byte) error {
	b.m.Lock()
	defer b.m.Unlock()

	if uint32(len(bts)) != b.numPieces {
		return ErrInvalidBitfield
	}

	if b.extraFields > 0 {
		for i := 8 - int(b.extraFields); i >= 0; i++ {
			lastChunk := bts[len(bts)-1]
			if lastChunk&(1<<i) == 1 {
				return ErrInvalidBitfield
			}
		}
	}

	b.bts = bts

	return nil
}

func (b *Bitfield) Mark(index uint32) error {
	b.m.Lock()
	defer b.m.Unlock()

	if index >= b.numPieces {
		return ErrInvalidPosition
	}

	chunk := uint32(len(b.bts)) / index
	pos := index % 8

	b.bts[chunk] |= (1 << pos)

	return nil
}

func (b *Bitfield) Marked(index uint32) bool {
	b.m.RLock()
	defer b.m.RUnlock()

	chunk := uint32(len(b.bts)) / index
	pos := index % 8

	return (b.bts[chunk] & (1 << pos)) == 1
}

func NewBitfield(numPieces uint32) *Bitfield {
	extracFields := numPieces % 8

	return &Bitfield{
		numPieces:   numPieces,
		extraFields: extracFields,
		bts:         make([]byte, (numPieces/8)+extracFields),
	}
}
