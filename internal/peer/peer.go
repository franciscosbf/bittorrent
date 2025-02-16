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
	handshakeTimeout = 1500 * time.Millisecond
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
	interestMsg
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
	choked        atomic.Bool
	closed        atomic.Bool
	stopHeartbeat chan struct{}
	conn          net.Conn
	b             *pieces.Bitfield
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
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	peerId := make(chan id.Peer, 1)

	go func() {
		handshakeMsg := buildHandshakeMsg(ih, pi)
		if _, err := c.conn.Write(handshakeMsg); err != nil {
			return
		}

		ackSize := len(handshakeMsg) - len(pi.Raw())

		handshakeMsgAck := make([]byte, ackSize)
		if _, err := io.ReadFull(c.conn, handshakeMsgAck); err != nil {
			return
		}

		handshakeMsg = handshakeMsg[:ackSize]
		if !bytes.Equal(handshakeMsg[:22], handshakeMsgAck[:22]) ||
			!bytes.Equal(handshakeMsg[30:], handshakeMsgAck[30:]) {
			return
		}

		var rawPeerId [20]byte
		if _, err := io.ReadFull(c.conn, rawPeerId[:]); err != nil {
			return
		}

		peerId <- id.Peer(rawPeerId)
	}()

	select {
	case pi := <-peerId:
		c.pi = id.Peer(pi)

		return nil
	case <-ctx.Done():
		c.close()

		return ErrHandshakeFailed
	}
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

		rawContent := []byte{}

		for {
			var rawLengthPrefix [4]byte
			if _, err := io.ReadFull(c.conn, rawLengthPrefix[:]); err != nil {
				return
			}
			var lengthPrefix uint32
			binary.Decode(rawLengthPrefix[:], binary.BigEndian, &lengthPrefix)

			if lengthPrefix == 0 {
				continue
			}

			var rawMsgId [1]byte
			if _, err := io.ReadFull(c.conn, rawMsgId[:]); err != nil {
				return
			}
			mt := msgType(rawMsgId[0])

			contentSz := lengthPrefix - 1

			switch {
			case mt < haveMsg:
				switch mt {
				case chokeMsg:
					c.setChoked(true)
				case unchokeMsg:
					c.setChoked(false)
				case interestMsg, notInterestedMsg:
				}
				continue
			case mt > pieceMsg:
				if contentSz > 0 {
					if _, err := io.CopyN(io.Discard, c.conn, int64(contentSz)); err != nil {
						return
					}
					continue
				}
			}

			if uint32(len(rawContent)) < contentSz {
				rawContent = make([]byte, contentSz)
			}
			if _, err := io.ReadFull(c.conn, rawContent); err != nil {
				return
			}

			switch mt {
			case haveMsg:
				var index uint32
				binary.Decode(rawContent, binary.BigEndian, &index)

				if err := c.b.Mark(index); err != nil {
					return
				}
			case bitfieldMsg:
				if err := c.b.Overwrite(rawContent); err != nil {
					return
				}
			case requestMsg:
				var index, begin, length uint32
				binary.Decode(rawContent[:4], binary.BigEndian, &index)
				binary.Decode(rawContent[4:8], binary.BigEndian, &begin)
				binary.Decode(rawContent[8:], binary.BigEndian, &length)

				go eh.RequestedBlock(c, index, begin, length)
			case pieceMsg:
				var index, begin uint32
				binary.Decode(rawContent[:4], binary.BigEndian, &index)
				binary.Decode(rawContent[4:8], binary.BigEndian, &begin)

				block := rawContent[8:]

				go eh.ReceivedBlock(c, index, begin, block)
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

	if err := client.doHandshake(tmeta.InfoHash, pi); err != nil {
		return nil, err
	}

	client.setChoked(true)
	client.startHeartbeat()
	client.startMsgsHandler(eh)

	return client, nil
}
