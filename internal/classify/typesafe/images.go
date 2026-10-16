package typesafe

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/classify"
)

// Image formats for Options.ImageFormat.
const (
	// ImageFormatAssets is TypeSafe's image format: a top-level "assets" map
	// whose reference strings are written into the state. Requests carrying
	// images also send the TypeSafe-Danger-Zone-Images opt-in header.
	ImageFormatAssets = "assets"
	// ImageFormatInline sends a top-level "images" array of data URLs beside
	// the state, as Clef and compatible System One servers accept.
	ImageFormatInline = "inline"

	imagesHeader = "TypeSafe-Danger-Zone-Images"
)

// asset is one entry of the TypeSafe "assets" map.
type asset struct {
	Type       string `json:"type"`
	DataBase64 string `json:"data_base64"`
	Format     string `json:"format"`
}

// newAssetRef returns a reference string that cannot plausibly occur in the
// request text by accident, as TypeSafe recommends.
var newAssetRef = func() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("typesafe: generate image reference: %w", err)
	}
	return "image-" + hex.EncodeToString(b[:]), nil
}

var assetFormats = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/jpg": "jpg"}

func (c *Client) wireRequest(req classify.Request) (wireRequest, error) {
	wire := wireRequest{State: req.State, Model: req.Model, Questions: req.Questions}
	if len(req.Images) == 0 {
		return wire, nil
	}
	if c.imageFormat == ImageFormatInline {
		if len(bytes.TrimSpace(wire.State)) == 0 {
			wire.State = json.RawMessage(`""`) // Image-only request; System One requires state.
		}
		wire.Images = req.Images
		return wire, nil
	}
	refs := make([]string, len(req.Images))
	wire.Assets = make(map[string]asset, len(req.Images))
	for i, image := range req.Images {
		a, err := imageAsset(i, image)
		if err != nil {
			return wire, err
		}
		if refs[i], err = newAssetRef(); err != nil {
			return wire, err
		}
		wire.Assets[refs[i]] = a
	}
	state, err := assetState(req.State, refs)
	if err != nil {
		return wire, err
	}
	wire.State = state
	return wire, nil
}

// imageAsset converts a base64 data:image URL to a TypeSafe asset.
func imageAsset(i int, image string) (asset, error) {
	header, payload, ok := strings.Cut(image, ",")
	mime, isBase64 := strings.CutSuffix(strings.TrimPrefix(strings.ToLower(header), "data:"), ";base64")
	format := assetFormats[mime]
	if !ok || !isBase64 || format == "" || payload == "" {
		return asset{}, fmt.Errorf("typesafe: image %d must be a PNG or JPEG base64 data URL", i+1)
	}
	return asset{Type: "image", DataBase64: payload, Format: format}, nil
}

// assetState places the image references in the state: the reference itself
// for one image-only input, an array for several, and alongside any text or
// JSON state as {"state": ..., "images": [...]}.
func assetState(state json.RawMessage, refs []string) (json.RawMessage, error) {
	var value any = refs
	if len(bytes.TrimSpace(state)) > 0 {
		value = struct {
			State  json.RawMessage `json:"state"`
			Images []string        `json:"images"`
		}{state, refs}
	} else if len(refs) == 1 {
		value = refs[0]
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("typesafe: encode image state: %w", err)
	}
	return encoded, nil
}
