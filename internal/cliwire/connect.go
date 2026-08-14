package cliwire

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	http2DataFrameType     = 0
	http2PaddedFlag        = 0x8
	maxDecodedMessageBytes = 128 << 20
)

var http2ClientPreface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

// decodeConnectRequestMessages extracts Connect/gRPC-style envelopes from a
// captured HTTP/2 request stream. Cursor Agent uses this framing and may gzip
// individual protobuf messages. The decoded protobuf bytes remain deliberately
// unmodified so an audit sees exactly what the CLI supplied at that layer.
func decodeConnectRequestMessages(requestPath, outputDir, baseName string) ([]string, error) {
	data, err := os.ReadFile(requestPath)
	if err != nil {
		return nil, fmt.Errorf("read HTTP/2 request capture: %w", err)
	}
	if !bytes.HasPrefix(data, http2ClientPreface) {
		return nil, nil
	}
	streams := make(map[uint32][]byte)
	for offset := len(http2ClientPreface); offset < len(data); {
		if len(data)-offset < 9 {
			return nil, errors.New("truncated HTTP/2 frame header")
		}
		length := int(data[offset])<<16 | int(data[offset+1])<<8 | int(data[offset+2])
		frameType := data[offset+3]
		flags := data[offset+4]
		streamID := binary.BigEndian.Uint32(data[offset+5:offset+9]) & 0x7fffffff
		offset += 9
		if length > len(data)-offset {
			return nil, errors.New("truncated HTTP/2 frame payload")
		}
		payload := data[offset : offset+length]
		offset += length
		if frameType != http2DataFrameType || streamID == 0 || len(payload) == 0 {
			continue
		}
		if flags&http2PaddedFlag != 0 {
			padding := int(payload[0])
			if padding+1 > len(payload) {
				return nil, errors.New("invalid HTTP/2 DATA padding")
			}
			payload = payload[1 : len(payload)-padding]
		}
		streams[streamID] = append(streams[streamID], payload...)
	}
	streamIDs := make([]int, 0, len(streams))
	for id := range streams {
		streamIDs = append(streamIDs, int(id))
	}
	sort.Ints(streamIDs)
	var files []string
	for _, numericID := range streamIDs {
		streamID := uint32(numericID)
		payload := streams[streamID]
		for message := 1; len(payload) > 0; message++ {
			if len(payload) < 5 {
				return files, fmt.Errorf("truncated Connect envelope on stream %d", streamID)
			}
			flags := payload[0]
			length := int(binary.BigEndian.Uint32(payload[1:5]))
			payload = payload[5:]
			if length > len(payload) {
				return files, fmt.Errorf("truncated Connect message on stream %d", streamID)
			}
			body := payload[:length]
			payload = payload[length:]
			if flags&1 != 0 {
				reader, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					return files, fmt.Errorf("open compressed Connect message: %w", err)
				}
				decoded, readErr := io.ReadAll(io.LimitReader(reader, maxDecodedMessageBytes+1))
				closeErr := reader.Close()
				if readErr != nil {
					return files, fmt.Errorf("decompress Connect message: %w", readErr)
				}
				if closeErr != nil {
					return files, fmt.Errorf("close compressed Connect message: %w", closeErr)
				}
				if len(decoded) > maxDecodedMessageBytes {
					return files, errors.New("decompressed Connect message exceeds audit limit")
				}
				body = decoded
			}
			name := fmt.Sprintf("%s-request-stream-%d-message-%03d.bin", baseName, streamID, message)
			file, err := openPrivateFile(filepath.Join(outputDir, name))
			if err != nil {
				return files, err
			}
			if _, err := file.Write(body); err != nil {
				file.Close()
				return files, fmt.Errorf("write decoded Connect message: %w", err)
			}
			if err := file.Close(); err != nil {
				return files, fmt.Errorf("close decoded Connect message: %w", err)
			}
			files = append(files, filepath.Join("connections", name))
		}
	}
	return files, nil
}
