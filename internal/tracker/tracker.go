package tracker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/franciscosbf/bittorrent/internal/id"
	"github.com/franciscosbf/bittorrent/internal/stats"
	"github.com/franciscosbf/bittorrent/internal/torrent"
	"github.com/imroc/req/v3"

	"github.com/jackpal/bencode-go"
)

var (
	ErrRequestFailed = errors.New("failed to request peers")
	ErrParseFailed   = errors.New("failed to parse tracker response")
)

type ErrTracker string

func (et ErrTracker) Error() string {
	return string(et)
}

type Event string

const (
	Started Event = "started"
	Stopped Event = "stopped"
)

type PeerAddress string

func (pa PeerAddress) HostPort() string {
	return string(pa)
}

func (pa PeerAddress) String() string {
	return pa.HostPort()
}

type Response struct {
	RetryInterval time.Duration
	Addrs         []PeerAddress
}

type Client struct {
	tu        *torrent.TrackerUrl
	hc        *req.Client
	pi        id.Peer
	ih        torrent.InfoHash
	sd        *stats.Download
	trackerId string
}

const (
	fakePort       uint16 = 6881
	requestTimeout        = 4 * time.Second
)

func (c *Client) RequestPeers(e Event) (*Response, error) {
	ih := c.ih.Raw()
	pi := c.pi.Raw()
	port := strconv.FormatUint(uint64(fakePort), 10)
	uploaded := strconv.FormatUint(uint64(c.sd.Uploaded()), 10)
	downloaded := strconv.FormatUint(uint64(c.sd.Downloaded()), 10)
	left := strconv.FormatUint(uint64(c.sd.Left()), 10)

	request := c.hc.R().
		SetQueryParam("info_hash", string(ih[:])).
		SetQueryParam("peer_id", string(pi[:])).
		SetQueryParam("port", port).
		SetQueryParam("uploaded", uploaded).
		SetQueryParam("downloaded", downloaded).
		SetQueryParam("left", left).
		SetQueryParam("compact", "1").
		SetQueryParam("event", string(e))

	if c.trackerId != "" {
		request.SetQueryParam("trackerid", c.trackerId)
	}

	response, err := request.Get(c.tu.FormattedUrl())
	if err != nil {
		return nil, ErrRequestFailed
	}

	var trackerResponse struct {
		FailureReason string `bencode:"failure reason"`
		Interval      uint32 `bencode:"interval"`
		TrackerId     string `bencode:"tracker id"`
		Peers         string `bencode:"peers"`
	}
	if err := bencode.Unmarshal(response.Body, &trackerResponse); err != nil {
		return nil, ErrParseFailed
	}

	if trackerResponse.FailureReason != "" {
		return nil, ErrTracker(trackerResponse.FailureReason)
	}

	if len(trackerResponse.Peers)%6 != 0 {
		return nil, ErrParseFailed
	}

	addrs := []PeerAddress{}
	rawPeers := []byte(trackerResponse.Peers)
	for i := range len(rawPeers) / 6 {
		start := i * 6
		end := start + 6
		raw := rawPeers[start:end]

		var port uint16
		if _, err := binary.Decode(raw[4:], binary.BigEndian, &port); err != nil {
			return nil, ErrParseFailed
		}
		addr := fmt.Sprintf("%v.%v.%v.%v:%v", raw[0], raw[1], raw[2], raw[3], port)
		addrs = append(addrs, PeerAddress(addr))
	}

	retryInterval := time.Duration(trackerResponse.Interval) * time.Second

	c.trackerId = trackerResponse.TrackerId

	return &Response{
		Addrs:         addrs,
		RetryInterval: retryInterval,
	}, nil
}

func New(
	pi id.Peer,
	tmeta *torrent.Metadata,
	sd *stats.Download,
) *Client {
	hc := req.C().SetTimeout(requestTimeout)

	return &Client{
		tu:        tmeta.Announce,
		hc:        hc,
		pi:        pi,
		ih:        tmeta.InfoHash,
		sd:        sd,
		trackerId: "",
	}
}
