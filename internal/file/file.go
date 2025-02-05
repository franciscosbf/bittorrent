package file

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/franciscosbf/bittorrent/internal/torrent"
	"github.com/google/uuid"
	"k8s.io/utils/lru"
)

const piecesCacheSize = 50

var (
	ErrInvalidPosition        = errors.New("file position is invalid")
	ErrTempFileCreationFailed = errors.New("failed to create temporary file")
	ErrReadTempFileFailed     = errors.New("failed to read temporary file")
	ErrWriteTempFileFailed    = errors.New("failed to write temporary file")
)

type ErrFinalFileFailed struct {
	path string
}

func (e ErrFinalFileFailed) Error() string {
	return fmt.Sprintf("failed to write file %v", e.path)
}

func calcTotalSize(files []torrent.File) int64 {
	var totalSize int64

	for _, file := range files {
		totalSize += int64(file.Length)
	}

	return totalSize
}

func closeAndDeleteFile(file *os.File) {
	file.Close()

	os.Remove(file.Name())
}

func createTempFile(fileSize int64) (*os.File, error) {
	tempDir := os.TempDir()
	filename := fmt.Sprintf("bittorrent-%v", uuid.New().String())

	tempFile, err := os.CreateTemp(tempDir, filename)
	if err != nil {
		return nil, ErrTempFileCreationFailed
	}

	if err := tempFile.Truncate(fileSize); err != nil {
		closeAndDeleteFile(tempFile)

		return nil, ErrTempFileCreationFailed
	}

	return tempFile, nil
}

type WriteBlock struct {
	Index uint32
	Data  []byte
}

type ReadBlock struct {
	Data []byte
}

type Handler struct {
	tempFileSize int64
	tempFile     *os.File
	pieceSize    uint32
	piecesCache  *lru.Cache
	files        []torrent.File
}

func (h *Handler) calcPieceStartPos(index uint32) int64 {
	return int64(index * h.pieceSize)
}

func (h *Handler) ReadBlock(index, begin, length uint32) ([]byte, error) {
	pieceStartPos := h.calcPieceStartPos(index)
	if pieceStartPos >= h.tempFileSize || begin+length > h.pieceSize {
		return nil, ErrInvalidPosition
	}

	if piece, ok := h.piecesCache.Get(index); ok {
		return piece.([]byte)[begin:length], nil
	}

	piece := make([]byte, h.pieceSize)

	if _, err := h.tempFile.ReadAt(piece, pieceStartPos); err != nil {
		return nil, ErrReadTempFileFailed
	}

	h.piecesCache.Add(index, piece)

	return piece[begin:length], nil
}

func (h *Handler) WritePiece(index uint32, piece []byte) error {
	pieceStartPos := h.calcPieceStartPos(index)
	if pieceStartPos+int64(len(piece)) > h.tempFileSize {
		return ErrInvalidPosition
	}

	if _, err := h.tempFile.WriteAt(piece, pieceStartPos); err != nil {
		return ErrWriteTempFileFailed
	}

	h.piecesCache.Add(index, piece)

	return nil
}

func (h *Handler) WriteFiles(location string) error {
	if h.tempFile == nil {
		return nil
	}

	var fileStartPos int64
	for _, file := range h.files {
		path := filepath.Join(location, file.Path)
		length := int64(file.Length)

		if _, err := h.tempFile.Seek(fileStartPos, io.SeekStart); err != nil {
			return ErrFinalFileFailed{path}
		}

		fileStartPos += length

		finalFile, err := os.Open(path)
		if err != nil {
			return ErrFinalFileFailed{path}
		}

		_, err = io.CopyN(finalFile, h.tempFile, length)
		finalFile.Close()
		if err != nil {
			return ErrFinalFileFailed{path}
		}
	}

	return nil
}

func (h *Handler) Close() {
	closeAndDeleteFile(h.tempFile)
}

func Start(totalPieces, pieceSize uint32, files []torrent.File) (*Handler, error) {
	tempFileSize := calcTotalSize(files)
	tempFile, err := createTempFile(tempFileSize)
	if err != nil {
		return nil, err
	}

	h := &Handler{
		tempFileSize: tempFileSize,
		tempFile:     tempFile,
		pieceSize:    pieceSize,
		piecesCache:  lru.New(piecesCacheSize),
		files:        files,
	}

	return h, nil
}
