package leecher

import (
	"container/list"
	"context"
	"crypto/sha1"
	"log"
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
	awaitTimeout          = 1000 * time.Millisecond
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
	tmeta             *torrent.Metadata
	pi                id.Peer
	blockSz           uint32
	nBlocks           uint32
	rbs               chan rb
	b                 *pieces.Bitfield
	fh                *files.Handler
	sd                *stats.Download
	tCli              *tracker.Client
	outputPath        string
	alerterPeersBatch chan []*peer.Client
	alertHavePiece    chan uint32
	stopPiecesAlerter chan struct{}
	peersBatch        *list.List
	peer              chan *peer.Client
	withoutPeers      chan struct{}
	stopPeersManager  chan struct{}
	nextPeersRequest  time.Time
	connectedPeers    map[tracker.PeerAddress]struct{}
	pendingAddrs      []tracker.PeerAddress
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

func (d *downloader) connectToPeer(addr tracker.PeerAddress) *peer.Client {
	eh := peer.EventHandlers{
		ReceivedBlock:  d.receivedBlockEvent,
		RequestedBlock: d.requestedBlockEvent,
	}

	pCli, err := peer.Connect(addr, d.tmeta, d.pi, eh)

	if err == nil {
		pCli.SendBitfield(d.b)
		pCli.SendUnchoke()
		pCli.SendInterested()
	}

	return pCli
}

func (d *downloader) connectToPeers(addrs []tracker.PeerAddress) {
	peerConns := make(chan *peer.Client, len(addrs))

	for _, addr := range addrs {
		go func(addr tracker.PeerAddress) {
			peerConns <- d.connectToPeer(addr)
		}(addr)
	}

	for range len(addrs) {
		if pCli := <-peerConns; pCli != nil {
			d.peersBatch.PushBack(pCli)
			d.connectedPeers[pCli.Addr()] = struct{}{}
		}
	}
}

func (d *downloader) requestFirstPeers() error {
	addrs, err := d.fetchPeerAddrs()
	if err != nil {
		return err
	}

	d.connectToPeers(addrs)

	return nil
}

func (d *downloader) sortPiecesByRarity() []*piece {
	nPieces := uint32(len(d.tmeta.Pieces))
	pieces := make([]*piece, nPieces)
	for i, hashPiece := range d.tmeta.Pieces {
		pieces[i] = &piece{hashPiece, uint32(i), d.tmeta.PieceLength, 0}
	}
	pieces[len(pieces)-1].length -= d.tmeta.TotalSize % nPieces

	for _, piece := range pieces {
		for e := d.peersBatch.Front(); e != nil; e = e.Next() {
			if e.Value.(*peer.Client).HasPiece(piece.index) {
				piece.occurs++
			}
		}
	}

	sort.Slice(pieces, func(i, j int) bool {
		return pieces[i].occurs < pieces[j].occurs
	})

	return pieces
}

func (d *downloader) startPiecesAlerter() {
	go func() {
		peersBatch := []*peer.Client{}

		for {
			select {
			case peersBatch = <-d.alerterPeersBatch:
			case pIndex := <-d.alertHavePiece:
				for _, pCli := range peersBatch {
					if pCli.Closed() {
						continue
					}

					go func(pCli *peer.Client, pIndex uint32) {
						pCli.SendHave(pIndex)
					}(pCli, pIndex)
				}
			case <-d.stopPiecesAlerter:
				return
			}
		}
	}()
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

func (d *downloader) fetchPeerAddrs() ([]tracker.PeerAddress, error) {
	newAddrs := []tracker.PeerAddress{}

	if toAdd := maxPeers - len(d.connectedPeers); len(d.pendingAddrs) == 0 {
		if time.Now().Before(d.nextPeersRequest) {
			time.Sleep(time.Until(d.nextPeersRequest))
		}

		response, err := d.tCli.RequestPeers(tracker.Started)
		if err != nil {
			return nil, err
		}

		d.nextPeersRequest = time.Now().Add(response.RetryInterval)

		for checkedAddrs, addr := range response.Addrs {
			if checkedAddrs == toAdd {
				break
			}

			if _, ok := d.connectedPeers[addr]; !ok {
				newAddrs = append(newAddrs, addr)
			}
		}

		d.pendingAddrs = []tracker.PeerAddress{}
		for i := toAdd; i < len(response.Addrs); i++ {
			addr := response.Addrs[i]

			if _, ok := d.connectedPeers[addr]; !ok {
				d.pendingAddrs = append(d.pendingAddrs, addr)
			}
		}
	} else {
		if nPeending := len(d.pendingAddrs); nPeending < toAdd {
			toAdd = nPeending
		}

		newAddrs = d.pendingAddrs[:toAdd]
		d.pendingAddrs = d.pendingAddrs[toAdd:]
	}

	return newAddrs, nil
}

func (d *downloader) startPeersFetcher() {
	go func() {
		for {
			for {
				toRemove := []*list.Element{}
				givenPeers := 0

				for e := d.peersBatch.Front(); e != nil; e = e.Next() {
					pCli := e.Value.(*peer.Client)

					if pCli.Closed() {
						toRemove = append(toRemove, e)

						continue
					}

					if pCli.Choked() {
						continue
					}

					select {
					case d.peer <- pCli:
						givenPeers++
					case <-d.stopPeersManager:
						d.tCli.RequestPeers(tracker.Stopped)

						for e := d.peersBatch.Front(); e != nil; e = e.Next() {
							e.Value.(*peer.Client).Close()
						}
						return
					}
				}

				for _, e := range toRemove {
					delete(d.connectedPeers, e.Value.(*peer.Client).Addr())
					d.peersBatch.Remove(e)
				}

				if d.peersBatch.Len() == 0 || givenPeers == 0 {
					break
				}
			}

			if peerAddrs, err := d.fetchPeerAddrs(); err != nil {
				d.connectToPeers(peerAddrs)
			}

			if len(d.connectedPeers) == 0 {
				d.withoutPeers <- struct{}{}
				return
			}
		}
	}()
}

func (d *downloader) peekPeer() *peer.Client {
	var pCli *peer.Client

	select {
	case pCli = <-d.peer:
	case <-d.withoutPeers:
		log.Println("ran off peers...")
	}

	return pCli
}

func (d *downloader) download(output string) error {
	defer func() {
		d.stopPeersManager <- struct{}{}
		d.stopPiecesAlerter <- struct{}{}

		d.fh.Close()
	}()

	if err := d.requestFirstPeers(); err != nil {
		return err
	}

	log.Printf("got %v peers, warming up...", len(d.connectedPeers))

	time.Sleep(awaitTimeout)

	pieces := d.sortPiecesByRarity()

	d.startPeersFetcher()
	d.startPiecesAlerter()

	for _, piece := range pieces {
		pHash := piece.hash
		pIndex := piece.index
		pLength := piece.length

	retryPiece:
		var pCli *peer.Client

		for {
			if pCli = d.peekPeer(); pCli == nil {
				return nil
			}

			if pCli.HasPiece(pIndex) {
				break
			}
		}

		var piece []byte
		for bPos := range d.nBlocks {
		retryBlock:
			for {
				bStart := bPos * d.blockSz
				var bLength uint32
				if length := bStart + d.blockSz; length > pLength {
					bLength = d.blockSz - (length - pLength)
				} else {
					bLength = d.blockSz
				}
				if !pCli.SendRequest(pIndex, bStart, bLength) {
					log.Printf("| %v | failed to send request of block (piece %v, start: %v, length: %v)",
						pCli.Addr(), pIndex, bStart, bLength)

					if pCli = d.peekPeer(); pCli == nil {
						return nil
					}

					goto retryBlock
				}

				var block []byte
				var repeat bool
				for {
					if block, repeat = d.waitForBlock(pCli, pIndex, bStart, bLength); !repeat {
						break
					}

					log.Printf("| %v | retrying block (piece %v, start: %v, length: %v)\n",
						pCli.Addr(), pIndex, bStart, bLength)
				}
				if block != nil {
					piece = append(piece, block...)

					log.Printf("| %v | got block (piece %v, start: %v, length: %v)\n",
						pCli.Addr(), pIndex, bStart, bLength)

					break
				} else {
					if pCli = d.peekPeer(); pCli == nil {
						return nil
					}

					goto retryBlock
				}
			}
		}

		if sha1.Sum(piece) != pHash {
			log.Printf("hash of piece %v doesn't match, retrying", pIndex)
			goto retryPiece
		}

		d.sd.AddDownloaded(pLength)

		if err := d.fh.WritePiece(pIndex, piece); err != nil {
			log.Printf("failed to write piece %v: %v", pIndex, err)
			break
		}

		d.b.Mark(pIndex)
		d.alertHavePiece <- pIndex

		log.Printf("piece %v has been successfully written", pIndex)
	}

	d.fh.WriteFilesAndClose(output)

	return nil
}

func Download(file []byte, output string) error {
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
		tmeta:             tmeta,
		pi:                pi,
		blockSz:           blockSz,
		nBlocks:           nBlocks,
		rbs:               make(chan rb, nBlocks),
		b:                 pieces.NewBitfield(tmeta),
		fh:                fh,
		sd:                sd,
		tCli:              tracker.New(pi, tmeta, sd),
		outputPath:        output,
		alerterPeersBatch: make(chan []*peer.Client, maxPeers),
		alertHavePiece:    make(chan uint32, len(tmeta.Pieces)),
		stopPiecesAlerter: make(chan struct{}, 1),
		peer:              make(chan *peer.Client, 1),
		peersBatch:        list.New(),
		withoutPeers:      make(chan struct{}, 1),
		stopPeersManager:  make(chan struct{}, 1),
		nextPeersRequest:  time.Now(),
		connectedPeers:    map[tracker.PeerAddress]struct{}{},
		pendingAddrs:      []tracker.PeerAddress{},
	}

	return d.download(output)
}
