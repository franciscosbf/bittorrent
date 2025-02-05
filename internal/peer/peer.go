package peer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/franciscosbf/bittorrent/internal/id"
	"github.com/franciscosbf/bittorrent/internal/pieces"
	"github.com/franciscosbf/bittorrent/internal/torrent"
	"github.com/franciscosbf/bittorrent/internal/tracker"
)

var (
	ErrConnFailed      = errors.New("failed to connect to peer")
	ErrHandshakeFailed = errors.New("failed to establish handshake")
)

const (
	connTimeout = 4 * time.Second
	heartbeat   = 2 * time.Minute
)

func dialTcpConn(addr string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, connTimeout)
	if err != nil {
		return nil, ErrConnFailed
	}

	return conn, nil
}

func buildHandshakeMsg(ih torrent.InfoHash, pi id.Peer) []byte {
	buff := bytes.Buffer{}
	buff.Grow(68)

	buff.WriteByte(19)

	buff.Write([]byte("BitTorrent protocol"))

	buff.Write([]byte{0, 0, 0, 0, 0, 0, 0, 0})

	ihRaw := ih.Raw()
	buff.Write(ihRaw[:])

	piRaw := pi.Raw()
	buff.Write(piRaw[:])

	return buff.Bytes()
}

func buildKeepAliveMsg() [4]byte {
	return [4]byte{0, 0, 0, 0}
}

func buildChokeMsg() [5]byte {
	return [5]byte{0, 0, 0, 1, 0}
}

func buildUnchokeMsg() [5]byte {
	return [5]byte{0, 0, 0, 1, 1}
}

func buildInterestedMsg() [5]byte {
	return [5]byte{0, 0, 0, 1, 2}
}

func buildNotInterestedMsg() [5]byte {
	return [5]byte{0, 0, 0, 1, 3}
}

func buildHaveMsg(index uint32) [9]byte {
	buff := [9]byte{0, 0, 0, 5, 4}

	binary.Encode(buff[5:], binary.BigEndian, index)

	return buff
}

func buildBitfieldMsg(bitfield []byte) []byte {
	buff := make([]byte, 4+1+len(bitfield))

	binary.Encode(buff, binary.BigEndian, cap(buff))
	buff[4] = 5

	copy(buff[4:], bitfield)

	return buff
}

func buildRequestMsg(index, begin, length uint32) [17]byte {
	buff := [17]byte{0, 0, 0, 13, 6}

	binary.Encode(buff[5:], binary.BigEndian, index)
	binary.Encode(buff[9:], binary.BigEndian, begin)
	binary.Encode(buff[13:], binary.BigEndian, length)

	return buff
}

func buildPieceBlockMsg(index, begin uint32, block []byte) []byte {
	buff := make([]byte, 4+9+len(block))

	binary.Encode(buff, binary.BigEndian, cap(buff))
	buff[4] = 7

	binary.Encode(buff[5:], binary.BigEndian, index)
	binary.Encode(buff[9:], binary.BigEndian, begin)
	copy(buff[13:], block)

	return buff
}

func buildCancelMsg(index, begin, length uint32) [17]byte {
	buff := [17]byte{0, 0, 0, 13, 8}

	binary.Encode(buff[5:], binary.BigEndian, index)
	binary.Encode(buff[9:], binary.BigEndian, begin)
	binary.Encode(buff[13:], binary.BigEndian, length)

	return buff
}

type msgType byte

const (
	chokeMsg msgType = iota + 1
	unchokeMsg
	interestedMsg
	notInterestedMsg
	haveMsg
	bitfieldMsg
	requestMsg
	pieceMsg
	cancelMsg
	portMsg
)

type EventHandlers struct {
	ReceivedBlock  func(c *Client, index, begin uint32, block []byte)
	RequestedBlock func(c *Client, index, begin, length uint32)
	CancelledBlock func(c *Client, index, begin, length uint32)
}

type Client struct {
	addr          tracker.PeerAddress
	interested    atomic.Bool
	choked        atomic.Bool
	closed        atomic.Bool
	stopHeartbeat chan struct{}
	conn          net.Conn
	b             *pieces.Bitfield
}

func (c *Client) setInterested(interested bool) {
	c.interested.Store(interested)
}

func (c *Client) setChoked(choked bool) {
	c.choked.Store(choked)
}

func (c *Client) sendMsg(msg []byte) bool {
	_, err := c.conn.Write(msg)

	return err == nil
}

func (c *Client) close() {
	if c.closed.CompareAndSwap(false, true) {
		c.stopHeartbeat <- struct{}{}
		c.conn.Close()
	}
}

func (c *Client) doHandshake(ih torrent.InfoHash, pi id.Peer) error {
	handshakeMsg := buildHandshakeMsg(ih, pi)
	if _, err := c.conn.Write(handshakeMsg); err != nil {
		return ErrHandshakeFailed
	}

	ackSize := len(handshakeMsg) - len(pi.Raw())
	handshakeMsgAck := make([]byte, ackSize)
	if _, err := c.conn.Read(handshakeMsgAck); err != nil {
		return ErrHandshakeFailed
	}
	if !bytes.Equal(handshakeMsg[:ackSize], handshakeMsgAck) {
		return ErrHandshakeFailed
	}

	return nil
}

func (c *Client) keepBeating() bool {
	ctx, cancel := context.WithTimeout(context.Background(), heartbeat)
	defer cancel()

	select {
	case <-ctx.Done():
		return true
	case <-c.stopHeartbeat:
		return false
	}
}

func (c *Client) startHeartbeat() {
	go func() {
		defer c.close()

		msg := buildKeepAliveMsg()
		for c.keepBeating() && c.sendMsg(msg[:]) {
		}
	}()
}

func (c *Client) startMsgsHandler(eh EventHandlers) {
	go func() {
		defer c.close()

		baseBuff := []byte{}
		for {
			rawLengthPrefix := [4]byte{}
			if _, err := c.conn.Read(rawLengthPrefix[:]); err != nil {
				break
			}
			var lengthPrefix uint32
			binary.Decode(rawLengthPrefix[:], binary.BigEndian, &lengthPrefix)

			if lengthPrefix == 0 {
				continue
			}

			var msgId [1]byte
			if _, err := c.conn.Read(msgId[:]); err != nil {
				return
			}

			if uint32(cap(baseBuff)) < lengthPrefix {
				baseBuff = make([]byte, lengthPrefix)
			}
			buff := baseBuff[:lengthPrefix-1]

			mt := msgType(msgId[0])

			switch mt {
			case chokeMsg:
				c.setChoked(true)
			case unchokeMsg:
				c.setChoked(false)
			case interestedMsg:
				c.setInterested(true)
			case notInterestedMsg:
				c.setInterested(false)
			case haveMsg:
				if _, err := c.conn.Read(buff); err != nil {
					return
				}

				var index uint32
				binary.Decode(buff, binary.BigEndian, &index)
				if err := c.b.Mark(index); err != nil {
					return
				}
			case bitfieldMsg:
				if _, err := c.conn.Read(buff); err != nil {
					return
				}

				if err := c.b.Overwrite(buff); err != nil {
					return
				}
			case requestMsg, cancelMsg:
				if _, err := c.conn.Read(buff); err != nil {
					return
				}

				var index, begin, length uint32
				binary.Decode(buff[:4], binary.BigEndian, &index)
				binary.Decode(buff[4:8], binary.BigEndian, &begin)
				binary.Decode(buff[8:], binary.BigEndian, &length)

				if mt == requestMsg {
					go eh.RequestedBlock(c, index, begin, length)
				} else {
					go eh.CancelledBlock(c, index, begin, length)
				}
			case pieceMsg:
				if _, err := c.conn.Read(buff); err != nil {
					return
				}

				var index, begin uint32
				binary.Decode(buff[:4], binary.BigEndian, &index)
				binary.Decode(buff[4:8], binary.BigEndian, &begin)
				block := append(make([]byte, lengthPrefix-8), buff[8:]...)

				go eh.ReceivedBlock(c, index, begin, block)
			case portMsg:
			default:
				return
			}
		}
	}()
}

func (c *Client) Addr() tracker.PeerAddress {
	return c.addr
}

func (c *Client) Interested() bool {
	return c.interested.Load()
}

func (c *Client) Choked() bool {
	return c.choked.Load()
}

func (c *Client) Closed() bool {
	return c.closed.Load()
}

func (c *Client) SendChoke() bool {
	msg := buildChokeMsg()

	return c.sendMsg(msg[:])
}

func (c *Client) SendUnchoke() bool {
	msg := buildUnchokeMsg()

	return c.sendMsg(msg[:])
}

func (c *Client) SendInterested() bool {
	msg := buildInterestedMsg()

	return c.sendMsg(msg[:])
}

func (c *Client) SendNotInterested() bool {
	msg := buildNotInterestedMsg()

	return c.sendMsg(msg[:])
}

func (c *Client) SendHave(index uint32) bool {
	msg := buildHaveMsg(index)

	return c.sendMsg(msg[:])
}

func (c *Client) SendBitfield(bitfield *pieces.Bitfield) bool {
	msg := buildBitfieldMsg(bitfield.Raw())

	return c.sendMsg(msg)
}

func (c *Client) SendRequest(index, begin, length uint32) bool {
	msg := buildRequestMsg(index, begin, length)

	return c.sendMsg(msg[:])
}

func (c *Client) SendPieceBlock(index, begin uint32, block []byte) bool {
	msg := buildPieceBlockMsg(index, begin, block)

	return c.sendMsg(msg[:])
}

func (c *Client) SendCancel(index, begin, length uint32) bool {
	msg := buildCancelMsg(index, begin, length)

	return c.sendMsg(msg[:])
}

func (c *Client) Close() {
	c.close()
}

func (c *Client) HasPiece(index uint32) bool {
	return c.b.Marked(index)
}

func Connect(
	addr tracker.PeerAddress,
	ih torrent.InfoHash,
	pi id.Peer,
	eh EventHandlers,
) (*Client, error) {
	conn, err := dialTcpConn(addr.HostPort())
	if err != nil {
		return nil, err
	}

	client := &Client{
		addr:          addr,
		conn:          conn,
		stopHeartbeat: make(chan struct{}, 1),
	}

	if err := client.doHandshake(ih, pi); err != nil {
		return nil, err
	}

	client.setChoked(true)
	client.startHeartbeat()
	client.startMsgsHandler(eh)

	return client, nil
}
