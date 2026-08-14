package cliwire

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeConnectRequestMessages(t *testing.T) {
	compressed := new(bytes.Buffer)
	gzipWriter := gzip.NewWriter(compressed)
	_, _ = gzipWriter.Write([]byte("compressed protobuf marker"))
	_ = gzipWriter.Close()
	payload := append(connectEnvelope(0, []byte("plain protobuf marker")), connectEnvelope(1, compressed.Bytes())...)
	capture := append([]byte(nil), http2ClientPreface...)
	capture = append(capture, http2Frame(http2DataFrameType, 0, 1, payload[:7])...)
	capture = append(capture, http2Frame(http2DataFrameType, 1, 1, payload[7:])...)
	dir := t.TempDir()
	requestPath := filepath.Join(dir, "request.bin")
	if err := os.WriteFile(requestPath, capture, 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := decodeConnectRequestMessages(requestPath, dir, "cursor")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("decoded files = %v", files)
	}
	for i, want := range []string{"plain protobuf marker", "compressed protobuf marker"} {
		body, err := os.ReadFile(filepath.Join(dir, filepath.Base(files[i])))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Fatalf("message %d = %q, want %q", i, body, want)
		}
	}
}

func connectEnvelope(flags byte, body []byte) []byte {
	out := make([]byte, 5, 5+len(body))
	out[0] = flags
	binary.BigEndian.PutUint32(out[1:5], uint32(len(body)))
	return append(out, body...)
}

func http2Frame(frameType, flags byte, streamID uint32, payload []byte) []byte {
	out := make([]byte, 9, 9+len(payload))
	out[0] = byte(len(payload) >> 16)
	out[1] = byte(len(payload) >> 8)
	out[2] = byte(len(payload))
	out[3] = frameType
	out[4] = flags
	binary.BigEndian.PutUint32(out[5:9], streamID)
	return append(out, payload...)
}
