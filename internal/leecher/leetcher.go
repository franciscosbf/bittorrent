package leecher

import (
	"sort"
	"time"

	"github.com/franciscosbf/bittorrent/internal/files"
	"github.com/franciscosbf/bittorrent/internal/id"
	"github.com/franciscosbf/bittorrent/internal/peer"
	"github.com/franciscosbf/bittorrent/internal/pieces"
	"github.com/franciscosbf/bittorrent/internal/stats"
	"github.com/franciscosbf/bittorrent/internal/torrent"
	"github.com/franciscosbf/bittorrent/internal/tracker"
)

const (
	defaultBlockSz uint32 = 16384
	maxPeers       uint32 = 30
	awaitTimeout          = 1500 * time.Millisecond
)

type rb struct {
	index, begin uint32
	block        []byte
}

type piece struct {
	hash   torrent.PieceHash
	pos    uint32
	length uint32
	occurs uint32
}

type downloader struct {
	tmeta      *torrent.Metadata
	pi         id.Peer
	blockSz    uint32
	nBlocks    uint32
	rbs        chan rb
	b          *pieces.Bitfield
	fh         *files.Handler
	sd         *stats.Download
	tcli       *tracker.Client
	outputPath string
}

func (d *downloader) receivedBlockEvent(c *peer.Client, index, begin uint32, block []byte) {
	if marked, err := d.b.Marked(index); err != nil {
		c.Close()
		return
	} else if marked {
		return
	}

	d.rbs <- rb{index, begin, block}

	d.sd.AddDownloaded(uint32(len(block)))
}

func (d *downloader) requestedBlockEvent(c *peer.Client, index, begin, length uint32) {
	if marked, err := d.b.Marked(index); err != nil {
		c.Close()
		return
	} else if !marked {
		return
	}

	block, err := d.fh.ReadBlock(index, begin, length)
	switch err {
	case nil:
		if c.SendPieceBlock(index, begin, block) {
			d.sd.AddUploaded(uint32(length))
		}
	case files.ErrInvalidFilePosition, files.ErrInvalidBlockPosition:
		c.Close()
		return
	default:
	}
}

func (d *downloader) requestPeers() ([]*peer.Client, time.Time, error) {
	tPeers, err := d.tcli.RequestPeers(tracker.Started)
	if err != nil {
		return nil, time.Time{}, err
	}

	eh := peer.EventHandlers{
		ReceivedBlock:  d.receivedBlockEvent,
		RequestedBlock: d.requestedBlockEvent,
	}
	peers := []*peer.Client{}
	connectedPeers := make(chan *peer.Client, len(tPeers.Addrs))
	for _, addr := range tPeers.Addrs {
		go func(addr tracker.PeerAddress) {
			pCli, err := peer.Connect(addr, d.tmeta, d.pi, eh)
			if err == nil {
				pCli.SendBitfield(d.b)
				pCli.SendUnchoke()
				pCli.SendInterested()
			}
			connectedPeers <- pCli
		}(addr)
	}
	for range tPeers.Addrs {
		if pCli := <-connectedPeers; pCli != nil {
			peers = append(peers, pCli)
		}
	}

	nextRequest := time.Now().Add(tPeers.RetryInterval)

	return peers, nextRequest, nil
}

func (d *downloader) sortPieces(peers []*peer.Client) []*piece {
	pieceHashes := d.tmeta.Pieces
	pieces := make([]*piece, len(d.tmeta.Pieces))
	for i, h := range pieceHashes {
		pieces = append(pieces, &piece{h, uint32(i), d.tmeta.PieceLength, 0})
	}
	pieces[len(pieces)-1].length -= d.tmeta.TotalSize % uint32(len(pieceHashes))

	for i := range len(d.tmeta.Pieces) {
		for _, pCli := range peers {
			if pCli.HasPiece(uint32(i)) {
				pieces[i].occurs++
			}
		}
	}

	sort.Slice(pieces, func(i, j int) bool {
		return pieces[i].occurs < pieces[j].occurs
	})

	return pieces
}

func (d *downloader) spreadHavePiece(peers []*peer.Client, index uint32) {
	for _, pCli := range peers {
		if pCli.Closed() || !pCli.Interested() {
			continue
		}

		go func(pCli *peer.Client) { pCli.SendHave(index) }(pCli)
	}
}

func (d *downloader) findPeerWithPiece(peers []*peer.Client, index uint32) *peer.Client {
	for _, pCli := range peers {
		if !pCli.Closed() && !pCli.Choked() && pCli.HasPiece(index) {
			return pCli
		}
	}

	return nil
}

func (d *downloader) download() error {
	defer d.fh.Close()

	peers, nextPeersRequest, err := d.requestPeers()
	if err != nil {
		return err
	}

	time.Sleep(awaitTimeout)

	pieces := d.sortPieces(peers)

	_ = peers
	_ = nextPeersRequest

	for _, p := range pieces {
		pHash := p.hash
		_ = pHash
		pPos := p.pos
		pLength := p.length

		for bPos := range d.nBlocks {
			_ = bPos
			for {
				pCli := d.findPeerWithPiece(peers, pPos)
				if pCli == nil {
					if !time.Now().After(nextPeersRequest) {
						time.Sleep(time.Until(nextPeersRequest))
					}

					if peers, nextPeersRequest, err = d.requestPeers(); err != nil {
						return err
					}

					continue
				}

				bStart := bPos * d.blockSz
				var bLength uint32
				if length := bStart + d.blockSz; length > pLength {
					bLength = d.blockSz - (length - pLength)
				} else {
					bLength = d.blockSz
				}
				if !pCli.SendRequest(pPos, bStart, bLength) {
					continue
				}

				// TODO: wait for block or try again
			}

			// TODO: check piece hash, write if right or try again
		}
	}

	return nil
}

func Download(file []byte, outputPath string) error {
	tmeta, err := torrent.Parse(file)
	if err != nil {
		return err
	}

	fh, err := files.Start(tmeta)
	if err != nil {
		return err
	}

	var nBlocks, blockSz uint32
	if tmeta.PieceLength < defaultBlockSz {
		nBlocks = tmeta.PieceLength
		blockSz = tmeta.PieceLength
	} else {
		nBlocks = tmeta.PieceLength / defaultBlockSz
		blockSz = defaultBlockSz
	}

	pi := id.NewPeerId()

	sd := stats.New(tmeta)

	d := &downloader{
		tmeta:      tmeta,
		pi:         pi,
		blockSz:    blockSz,
		nBlocks:    nBlocks,
		rbs:        make(chan rb, nBlocks),
		b:          pieces.NewBitfield(tmeta),
		fh:         fh,
		sd:         sd,
		tcli:       tracker.New(pi, tmeta, sd),
		outputPath: outputPath,
	}

	return d.download()
}
