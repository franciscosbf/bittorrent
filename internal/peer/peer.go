package peer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
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
	connTimeout      = 1500 * time.Millisecond
	handshakeTimeout = 2 * time.Second
	heartbeatTimeout = 2 * time.Minute
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

func buildHaveMsg(index uint32) [9]byte {
	buff := [9]byte{0, 0, 0, 5, 4}

	binary.Encode(buff[5:], binary.BigEndian, index)

	return buff
}

func buildBitfieldMsg(bitfield []byte) []byte {
	buff := make([]byte, 4+1+len(bitfield))

	binary.Encode(buff, binary.BigEndian, uint32(1+len(bitfield)))
	buff[4] = 5

	copy(buff[5:], bitfield)

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

type msgType byte

const (
	chokeMsg msgType = iota
	unchokeMsg
	interestedMsg
	notInterestedMsg
	haveMsg
	bitfieldMsg
	requestMsg
	pieceMsg
)

type EventHandlers struct {
	ReceivedBlock  func(c *Client, index, begin uint32, block []byte)
	RequestedBlock func(c *Client, index, begin, length uint32)
}

type Client struct {
	pi            id.Peer
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

	handshakeMsg = handshakeMsg[:ackSize]
	if !bytes.Equal(handshakeMsg[:22], handshakeMsgAck[:22]) ||
		!bytes.Equal(handshakeMsg[30:], handshakeMsgAck[30:]) {

		return ErrHandshakeFailed
	}

	peerId := make([]byte, 20)
	if _, err := c.conn.Read(peerId); err != nil {
		return ErrHandshakeFailed
	}

	c.pi = id.Peer(peerId)

	return nil
}

func (c *Client) keepBeating() bool {
	ctx, cancel := context.WithTimeout(context.Background(), heartbeatTimeout)
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

		for {
			rawLengthPrefix := [4]byte{}
			if _, err := c.conn.Read(rawLengthPrefix[:]); err != nil {
				return
			}
			var lengthPrefix uint32
			binary.Decode(rawLengthPrefix[:], binary.BigEndian, &lengthPrefix)

			if lengthPrefix == 0 {
				continue
			}

			rawMsgId := [1]byte{}
			if _, err := c.conn.Read(rawMsgId[:]); err != nil {
				return
			}
			mt := msgType(rawMsgId[0])

			if mt > 9 {
				return
			}

			var buff bytes.Buffer

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
				if _, err := io.CopyN(&buff, c.conn, int64(lengthPrefix)-1); err != nil {
					return
				}
				data := buff.Bytes()

				var index uint32
				binary.Decode(data, binary.BigEndian, &index)

				if err := c.b.Mark(index); err != nil {
					return
				}
			case bitfieldMsg:
				if _, err := io.CopyN(&buff, c.conn, int64(lengthPrefix)-1); err != nil {
					return
				}
				data := buff.Bytes()

				if err := c.b.Overwrite(data); err != nil {
					return
				}
			case requestMsg:
				if _, err := io.CopyN(&buff, c.conn, int64(lengthPrefix)-1); err != nil {
					return
				}
				data := buff.Bytes()

				var index, begin, length uint32
				binary.Decode(data[:4], binary.BigEndian, &index)
				binary.Decode(data[4:8], binary.BigEndian, &begin)
				binary.Decode(data[8:], binary.BigEndian, &length)

				go eh.RequestedBlock(c, index, begin, length)
			case pieceMsg:
				if _, err := io.CopyN(&buff, c.conn, int64(lengthPrefix)-1); err != nil {
					return
				}
				data := buff.Bytes()

				var index, begin uint32
				binary.Decode(data[:4], binary.BigEndian, &index)
				binary.Decode(data[4:8], binary.BigEndian, &begin)

				block := data[8:]

				go eh.ReceivedBlock(c, index, begin, block)
			default:
			}
		}
	}()
}

func (c *Client) Id() id.Peer {
	return c.pi
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

func (c *Client) Close() {
	c.close()
}

func (c *Client) HasPiece(index uint32) bool {
	marked, _ := c.b.Marked(index)

	return marked
}

func Connect(
	addr tracker.PeerAddress,
	tmeta *torrent.Metadata,
	pi id.Peer,
	eh EventHandlers,
) (*Client, error) {
	conn, err := dialTcpConn(addr.HostPort())
	if err != nil {
		return nil, err
	}

	b := pieces.NewBitfield(tmeta)
	client := &Client{
		addr:          addr,
		conn:          conn,
		stopHeartbeat: make(chan struct{}, 1),
		b:             b,
	}

	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	handshakeRet := make(chan error, 1)
	go func() {
		handshakeRet <- client.doHandshake(tmeta.InfoHash, pi)
	}()
	select {
	case <-ctx.Done():
		client.Close()
		return nil, ErrHandshakeFailed
	case err := <-handshakeRet:
		if err != nil {
			return nil, err
		}
	}

	client.setChoked(true)
	client.startHeartbeat()
	client.startMsgsHandler(eh)

	return client, nil
}
