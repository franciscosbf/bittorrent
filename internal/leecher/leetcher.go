package leecher

import (
	"context"
	"crypto/sha1"
	"fmt"
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
	maxPeers              = 30
	awaitTimeout          = 500 * time.Millisecond
)

type rb struct {
	c            *peer.Client
	index, begin uint32
	block        []byte
}

type piece struct {
	hash   torrent.PieceHash
	index  uint32
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

	d.rbs <- rb{c, index, begin, block}
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

func (d *downloader) requestPeers() ([]*peer.Client, error) {
	addrs, err := d.tcli.RequestPeers(tracker.Started)
	if err != nil {
		return nil, err
	}

	eh := peer.EventHandlers{
		ReceivedBlock:  d.receivedBlockEvent,
		RequestedBlock: d.requestedBlockEvent,
	}

	nPeers := len(addrs)
	if nPeers > maxPeers {
		nPeers = maxPeers
	}

	peers := []*peer.Client{}
	connectedPeers := make(chan *peer.Client, nPeers)
	for _, addr := range addrs[:nPeers] {
		go func(addr tracker.PeerAddress, eh peer.EventHandlers) {
			pCli, err := peer.Connect(addr, d.tmeta, d.pi, eh)
			if err == nil {
				pCli.SendBitfield(d.b)
				pCli.SendUnchoke()
				pCli.SendInterested()
			}
			connectedPeers <- pCli
		}(addr, eh)
	}
	for range nPeers {
		if pCli := <-connectedPeers; pCli != nil {
			fmt.Printf("got peer %v\n", pCli.Addr())
			peers = append(peers, pCli)
		}
	}

	return peers, nil
}

func (d *downloader) sortPieces(peers []*peer.Client) []*piece {
	nPieces := uint32(len(d.tmeta.Pieces))
	pieces := make([]*piece, nPieces)
	for i, hashPiece := range d.tmeta.Pieces {
		pieces[i] = &piece{hashPiece, uint32(i), d.tmeta.PieceLength, 0}
	}
	pieces[len(pieces)-1].length -= d.tmeta.TotalSize % nPieces

	for _, piece := range pieces {
		for _, pCli := range peers {
			if pCli.HasPiece(piece.index) {
				piece.occurs++
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

func (d *downloader) waitForBlock(
	pCli *peer.Client,
	pIndex, bStart, bLength uint32,
) (block []byte, repeat bool) {
	ctx, cancel := context.WithTimeout(context.Background(), awaitTimeout)
	defer cancel()

	select {
	case <-ctx.Done():
		return nil, false
	case rb := <-d.rbs:
		if rb.c != pCli {
			return nil, true
		}
		if pIndex != rb.index || bStart != rb.begin || bLength != uint32(len(rb.block)) {
			return nil, false
		}

		return rb.block, false
	}
}

func (d *downloader) closePeers(peers []*peer.Client) {
	for _, pCli := range peers {
		pCli.Close()
	}
}

func (d *downloader) download() error {
	defer d.fh.Close()

	peers, err := d.requestPeers()
	if err != nil {
		return err
	}

	fmt.Printf("Got %v peers\n", len(peers))

	time.Sleep(awaitTimeout)

	pieces := d.sortPieces(peers)

	for _, piece := range pieces {
		pHash := piece.hash
		_ = pHash
		pIndex := piece.index
		pLength := piece.length

		var piece []byte

		for bPos := range d.nBlocks {
			for {
				pCli := d.findPeerWithPiece(peers, pIndex)
				if pCli == nil {
					fmt.Println("fetching new peers")

					d.closePeers(peers)
					if peers, err = d.requestPeers(); err != nil {
						return err
					}
					fmt.Printf("Got %v peers\n", len(peers))

					continue
				}

				bStart := bPos * d.blockSz
				var bLength uint32
				if length := bStart + d.blockSz; length > pLength {
					bLength = d.blockSz - (length - pLength)
				} else {
					bLength = d.blockSz
				}
				if !pCli.SendRequest(pIndex, bStart, bLength) {
					fmt.Printf("failed to send request to %v\n", pCli.Addr())
					continue
				}

				var block []byte
				var repeat bool
				for {
					if block, repeat = d.waitForBlock(pCli, pIndex, bStart, bLength); !repeat {
						break
					}
					fmt.Printf("repeating with %v\n", pCli.Addr())
				}
				if block != nil {
					piece = append(piece, block...)
					fmt.Printf("%v | pIndex: %v, bStart: %v, bLength: %v\n", pCli.Addr(), pIndex, bStart, bLength)
					break
				}
			}
		}

		d.spreadHavePiece(peers, pIndex)

		fmt.Printf("got piece %v\n", pIndex)

		if sha1.Sum(piece) != pHash {
			fmt.Println("piece hash doesn't match")
		}

		// TODO: check piece hash, write if right or try again

		d.sd.AddDownloaded(pLength)
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
