package torrent

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"

	"github.com/jackpal/bencode-go"
)

var ErrParseFailed = errors.New("failed to parse torrent file")

type ErrInvalidFile string

func (e ErrInvalidFile) Error() string {
	return string(e)
}

type InfoHash [20]byte

func (ih InfoHash) Raw() [20]byte {
	return [20]byte(ih)
}

func (ih InfoHash) String() string {
	raw := ih.Raw()
	return fmt.Sprintf("%v", hex.EncodeToString(raw[:]))
}

type TrackerUrl url.URL

func (tu *TrackerUrl) FormattedUrl() string {
	return ((*url.URL)(tu)).String()
}

func (tu *TrackerUrl) String() string {
	return fmt.Sprintf("%v", tu.FormattedUrl())
}

type Piece [20]byte

func (p Piece) Raw() [20]byte {
	return [20]byte(p)
}

func (p Piece) String() string {
	raw := p.Raw()
	return fmt.Sprintf("%v", hex.EncodeToString(raw[:]))
}

type File struct {
	Path   string
	Length uint64
}

func (f File) String() string {
	return fmt.Sprintf("{Path: %v, Length: %v}", f.Path, f.Length)
}

type Torrent struct {
	Announce    *TrackerUrl
	PieceLength uint64
	Pieces      []Piece
	Files       []File
}

func (t *Torrent) String() string {
	return fmt.Sprintf("{Announce: %v, PieceLength: %v, Pieces: %v, Files: %v}",
		t.Announce, t.PieceLength, t.Pieces, t.Files)
}

func Parse(file io.Reader) (*Torrent, error) {
	var torrentFile struct {
		Announce string `bencode:"announce"`
		Info     struct {
			PieceLength uint64 `bencode:"piece length"`
			Pieces      string `bencode:"pieces"`
			Name        string `bencode:"name"`
			Length      uint64 `bencode:"length"`
			Files       []struct {
				Length uint64   `bencode:"length"`
				Path   []string `bencode:"path"`
			} `bencode:"files"`
		} `bencode:"info"`
	}
	if err := bencode.Unmarshal(file, &torrentFile); err != nil {
		return nil, ErrParseFailed
	}

	announce, err := url.Parse(torrentFile.Announce)
	if err != nil {
		return nil, ErrInvalidFile("invalid announce field")
	}

	pieceLength := torrentFile.Info.PieceLength

	if len(torrentFile.Info.Pieces)%20 != 0 {
		return nil, ErrInvalidFile("invalid pieces field")
	}

	pieces := []Piece{}
	rawPieces := []byte(torrentFile.Info.Pieces)
	for i := range len(torrentFile.Info.Pieces) / 20 {
		start := i * 20
		end := start + 20
		raw := rawPieces[start:end]

		piece := Piece(raw[:])
		pieces = append(pieces, piece)
	}

	files := []File{}
	if len(torrentFile.Info.Files) == 0 {
		file := File{
			Path:   torrentFile.Info.Name,
			Length: torrentFile.Info.Length,
		}
		files = append(files, file)
	} else {
		baseDir := torrentFile.Info.Name
		for _, rawFile := range torrentFile.Info.Files {
			path := filepath.Join(baseDir, filepath.Join(rawFile.Path...))
			file := File{
				Path:   path,
				Length: rawFile.Length,
			}
			files = append(files, file)
		}
	}

	torrent := &Torrent{
		Announce:    (*TrackerUrl)(announce),
		PieceLength: pieceLength,
		Pieces:      pieces,
		Files:       files,
	}

	return torrent, nil
}
