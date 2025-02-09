package main

import (
	"fmt"
	"os"

	"github.com/franciscosbf/bittorrent/internal/leecher"
)

func main() {
	filename := os.Args[1]
	file, _ := os.ReadFile(filename)
	fmt.Println(leecher.Download(file, "/tmp"))

	// tmeta, err := torrent.Parse(file)
	// if err != nil {
	// 	fmt.Println(err)
	// 	return
	// }
	//
	// peerId := []byte("-BOWxxx-yyyyyyyyyyyy")
	// pi := id.Peer(peerId)
	// sd := stats.New(tmeta)
	// tk := tracker.New(pi, tmeta, sd)
	// peers, err := tk.RequestPeers(tracker.Started)
	// if err != nil {
	// 	fmt.Println(err)
	// 	return
	// }
	//
	// hldrs := peer.EventHandlers{
	// 	ReceivedBlock: func(c *peer.Client, index, begin uint32, block []byte) {
	// 		fmt.Println(index, " ", begin, " ", len(block))
	// 	},
	// 	RequestedBlock: func(c *peer.Client, index, begin, length uint32) {},
	// }
	//
	// fmt.Println(tmeta.PieceLength)
	// var bSz uint32 = 16384
	// for _, addr := range peers.Addrs {
	// 	cli, err := peer.Connect(addr, tmeta, pi, hldrs)
	// 	if err != nil {
	// 		fmt.Println(err)
	// 		continue
	// 	}
	// 	cli.SendBitfield(pieces.NewBitfield(tmeta))
	// 	cli.SendUnchoke()
	// 	cli.SendInterested()
	// 	time.Sleep(2 * time.Second)
	// 	if !cli.HasPiece(0) {
	// 		continue
	// 	}
	// 	fmt.Println(cli.Id(), " ", cli.Choked(), " ", cli.Interested())
	// 	for i := range tmeta.PieceLength / bSz {
	// 		cli.SendRequest(0, i*bSz, bSz)
	// 	}
	// 	time.Sleep(5 * time.Second)
	// 	cli.Close()
	// 	return
	// }
	//
	// fmt.Println(client.NewPeerId())

	// fh, err := file.Start(4, 4, []torrent.File{{"a/b/c/file.txt", 3}, {"a/b/c/file.bat", 6}, {"a/b/file.gba", 5}})
	// if err != nil {
	// 	panic(err)
	// }
	// if err := fh.WritePiece(0, []byte{'a', '1', '1', 'a'}); err != nil {
	// 	panic(err)
	// }
	// if err := fh.WritePiece(1, []byte{'b', '2', '2', 'b'}); err != nil {
	// 	panic(err)
	// }
	// if err := fh.WritePiece(2, []byte{'c', '3', '3', 'c'}); err != nil {
	// 	panic(err)
	// }
	// if err := fh.WritePiece(3, []byte{'d', 'd'}); err != nil {
	// 	panic(err)
	// }
	// block, err := fh.ReadBlock(1, 1, 4)
	// fmt.Println(string(block), " ", err)
	// fmt.Println(fh.WriteFilesAndClose("/tmp/test"))
}
