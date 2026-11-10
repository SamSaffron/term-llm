package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"testing"

	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
)

func TestCodeModeImagesExplicitBoundedAndCellScoped(t *testing.T) {
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	e := newCodeEngine()
	cell := admitTestCell(t, e)
	handle, err := cell.saveImage(base64.StdEncoding.EncodeToString(buffer.Bytes()), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	value, _ := json.Marshal(map[string]any{"type": "image", "handle": handle})
	part, err := cell.Image(value)
	if err != nil || part.ImageData.Width != 2 || part.ImageData.Height != 3 {
		t.Fatalf("%+v %v", part, err)
	}
	if _, err = cell.Image(json.RawMessage(`"forged"`)); err == nil {
		t.Fatal("forged image accepted")
	}
	if _, err = cell.Image(json.RawMessage(`{"type":"image","url":"http://127.0.0.1/secret"}`)); err == nil {
		t.Fatal("URL accepted as image authority")
	}
	cell.Close()
	second, err := e.admitCodeCell(context.Background(), "second", corestream.Sender{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err = second.Image(value); err == nil {
		t.Fatal("cross-cell handle accepted")
	}
}
