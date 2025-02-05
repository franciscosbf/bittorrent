package file

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/franciscosbf/bittorrent/internal/torrent"
	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
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

func calcTempFileSize(files []torrent.File) int64 {
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
	piecesCache  *lru.Cache[uint32, []byte]
	files        []torrent.File
}

func (h *Handler) close() {
	closeAndDeleteFile(h.tempFile)
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
		return piece[begin:length], nil
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

func (h *Handler) Close() {
	if h.tempFile == nil {
		return
	}

	h.close()
}

func (h *Handler) WriteFilesAndClose(location string) error {
	if h.tempFile == nil {
		return nil
	}

	defer h.close()

	fileStartPos := h.tempFileSize
	for i := len(h.files) - 1; i >= 0; i-- {
		file := h.files[i]
		length := int64(file.Length)
		fileStartPos -= length
		path := filepath.Join(location, file.Path)

		if _, err := h.tempFile.Seek(fileStartPos, io.SeekStart); err != nil {
			return ErrFinalFileFailed{path}
		}

		dir, _ := filepath.Split(path)
		if err := os.MkdirAll(dir, 0751); err != nil {
			return ErrFinalFileFailed{path}
		}

		finalFile, err := os.Create(path)
		if err != nil {
			return ErrFinalFileFailed{path}
		}

		_, err = io.CopyN(finalFile, h.tempFile, length)
		finalFile.Close()
		if err != nil {
			os.Remove(finalFile.Name())
			return ErrFinalFileFailed{path}
		}

		reducedTempFileSize := fileStartPos
		if err := h.tempFile.Truncate(reducedTempFileSize); err != nil {
			return ErrFinalFileFailed{path}
		}
	}

	return nil
}

func Start(totalPieces, pieceSize uint32, files []torrent.File) (*Handler, error) {
	tempFileSize := calcTempFileSize(files)
	tempFile, err := createTempFile(tempFileSize)
	if err != nil {
		return nil, err
	}

	piecesCache, _ := lru.New[uint32, []byte](piecesCacheSize)
	h := &Handler{
		tempFileSize: tempFileSize,
		tempFile:     tempFile,
		pieceSize:    pieceSize,
		piecesCache:  piecesCache,
		files:        files,
	}

	return h, nil
}
