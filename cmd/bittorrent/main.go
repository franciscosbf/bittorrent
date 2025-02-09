package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/franciscosbf/bittorrent/internal/leecher"
)

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	fmt.Println()

	os.Exit(1)
}

func main() {
	var (
		filename string
		output   string
	)

	flag.StringVar(&filename, "torrent", "", "torrent file")
	flag.StringVar(&output, "output", "", "output path")
	flag.Parse()

	if filename == "" {
		die("Missing torrent file")
	}

	if output == "" {
		die("Missing output path")
	}

	file, err := os.ReadFile(filename)
	if err != nil {
		die("Failed to read torrent file: %v", err)
	}

	if err := leecher.Download(file, output); err != nil {
		die("Failed to complete torrent donwload: %v", err)
	}
}
