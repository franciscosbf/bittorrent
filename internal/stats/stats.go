package stats

import (
	"sync/atomic"

	"github.com/franciscosbf/bittorrent/internal/torrent"
)

type Download struct {
	toDownload uint32
	uploaded   atomic.Uint32
	downloaded atomic.Uint32
	left       atomic.Uint32
}

func (d *Download) AddUploaded(size uint32) {
	d.uploaded.Add(size)
}

func (d *Download) Uploaded() uint32 {
	return d.uploaded.Load()
}

func (d *Download) AddDownloaded(size uint32) {
	d.downloaded.Add(size)
}

func (d *Download) Downloaded() uint32 {
	return d.downloaded.Load()
}

func (d *Download) Left() uint32 {
	return d.toDownload - d.Downloaded()
}

func New(tmeta *torrent.Metadata) *Download {
	d := &Download{toDownload: tmeta.TotalSize}

	return d
}
