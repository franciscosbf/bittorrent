package peer

type Id [20]byte

func (pi Id) Raw() [20]byte {
	return [20]byte(pi)
}
