package stats

import "sync/atomic"

type Download struct {
	uploaded   atomic.Uint64
	downloaded atomic.Uint64
	left       atomic.Uint64
}

func (d *Download) AddUploaded(size uint64) {
	d.uploaded.Add(size)
}

func (d *Download) Uploaded() uint64 {
	return d.uploaded.Load()
}

func (d *Download) AddDownloaded(size uint64) {
	d.downloaded.Add(size)
}

func (d *Download) Downloaded() uint64 {
	return d.downloaded.Load()
}

func (d *Download) SubLeft(size uint64) {
	d.left.Add(-size)
}

func (d *Download) Left() uint64 {
	return d.left.Load()
}

func New(torrentSize uint64) *Download {
	d := &Download{}

	d.left.Store(torrentSize)

	return d
}
