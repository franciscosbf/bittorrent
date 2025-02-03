package id

import (
	"fmt"
	"strings"

	"golang.org/x/exp/rand"
)

var asciiCharacters = []byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

type Peer [20]byte

func (pi Peer) Raw() [20]byte {
	return [20]byte(pi)
}

func (pi Peer) String() string {
	raw := pi.Raw()

	return string(raw[:])
}

func NewPeerId() Peer {
	characters := strings.Builder{}
	characters.Grow(12)

	for range characters.Cap() {
		characters.WriteByte(asciiCharacters[rand.Intn(len(asciiCharacters))])
	}

	return Peer([]byte(fmt.Sprintf("-BT6969-%v", characters.String())))
}
