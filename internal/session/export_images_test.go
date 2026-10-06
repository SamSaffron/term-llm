package session

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/share"
)

func testImage(width, height int, opaque bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			alpha := uint8(255)
			if !opaque && x == 0 {
				alpha = 0
			}
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 40), G: uint8(y * 40), B: 200, A: alpha})
		}
	}
	return img
}

func testPNG(t *testing.T, width, height int, opaque bool) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, testImage(width, height, opaque)); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// testJPEGWithEXIF returns a JPEG carrying an APP1 EXIF segment with the given
// orientation and a recognizable marker standing in for GPS metadata.
func testJPEGWithEXIF(t *testing.T, width, height, orientation int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, testImage(width, height, true), &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	var tiff bytes.Buffer
	tiff.WriteString("MM")
	_ = binary.Write(&tiff, binary.BigEndian, uint16(42))
	_ = binary.Write(&tiff, binary.BigEndian, uint32(8))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(1))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(0x0112))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(3))
	_ = binary.Write(&tiff, binary.BigEndian, uint32(1))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(orientation))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(0))
	_ = binary.Write(&tiff, binary.BigEndian, uint32(0))
	tiff.WriteString("SECRET-GPS-MARKER")
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	segment := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(payload)+2))
	segment = append(segment, payload...)
	raw := encoded.Bytes()
	out := append([]byte{}, raw[:2]...)
	out = append(out, segment...)
	return append(out, raw[2:]...)
}

var allImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true}

func TestNormalizeExportImageAppliesOrientationAndStripsEXIF(t *testing.T) {
	raw := testJPEGWithEXIF(t, 8, 4, 6)
	if got := jpegEXIFOrientation(raw); got != 6 {
		t.Fatalf("orientation = %d, want 6", got)
	}
	data, mediaType, err := normalizeExportImage(raw, 2048, allImageTypes)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("Exif")) || bytes.Contains(data, []byte("SECRET-GPS-MARKER")) {
		t.Fatal("normalized image retained EXIF metadata")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if "image/"+format != mediaType {
		t.Fatalf("declared %s but encoded %s", mediaType, format)
	}
	if config.Width != 4 || config.Height != 8 {
		t.Fatalf("rotated size = %dx%d, want 4x8", config.Width, config.Height)
	}
}

func TestApplyEXIFOrientationMapsCorners(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{B: 255, A: 255})
	// Orientation 6 means the stored image must be rotated 90° clockwise.
	rotated := applyEXIFOrientation(src, 6)
	if rotated.Bounds().Dx() != 1 || rotated.Bounds().Dy() != 2 {
		t.Fatalf("bounds = %v", rotated.Bounds())
	}
	if r, _, _, _ := rotated.At(0, 0).RGBA(); r != 0xffff {
		t.Fatalf("top pixel should be the original left pixel")
	}
}

func TestNormalizeExportImageDownscalesAndRejectsNonImages(t *testing.T) {
	data, _, err := normalizeExportImage(testPNG(t, 3000, 30, true), 2048, allImageTypes)
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != 2048 || config.Height != 20 {
		t.Fatalf("downscaled config = %+v, err %v", config, err)
	}
	if _, _, err := normalizeExportImage([]byte("<svg onload=alert(1)>"), 2048, allImageTypes); err == nil {
		t.Fatal("non-raster input was accepted")
	}
	if _, _, err := normalizeExportImage(testPNG(t, 4, 4, true), 2048, map[string]bool{"image/gif": true}); err == nil {
		t.Fatal("encoded an image without a permitted target type")
	}
}

func TestNormalizeExportImageFlattensTransparencyForJPEGOnly(t *testing.T) {
	data, mediaType, err := normalizeExportImage(testPNG(t, 4, 4, false), 2048, map[string]bool{"image/jpeg": true})
	if err != nil || mediaType != "image/jpeg" {
		t.Fatalf("mediaType = %q, err %v", mediaType, err)
	}
	if _, format, err := image.DecodeConfig(bytes.NewReader(data)); err != nil || format != "jpeg" {
		t.Fatalf("format = %q, err %v", format, err)
	}
}

func imageShareFixture(t *testing.T) (*Session, []Message, string) {
	t.Helper()
	dir := t.TempDir()
	generated := filepath.Join(dir, "generated.png")
	shown := filepath.Join(dir, "shown.png")
	notImage := filepath.Join(dir, "secret.png")
	if err := os.WriteFile(generated, testPNG(t, 6, 6, true), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shown, testPNG(t, 5, 7, true), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notImage, []byte("TOP-SECRET-TEXT"), 0o600); err != nil {
		t.Fatal(err)
	}
	const reference = "0123456789abcdef0123456789abcdef"
	upload := base64.StdEncoding.EncodeToString(testPNG(t, 3, 3, true))
	sess := &Session{ID: "sess", Provider: "p", Model: "m", CreatedAt: time.Now()}
	messages := []Message{
		{ID: 1, Sequence: 1, Role: llm.RoleUser, TextContent: "draw it", Parts: []llm.Part{
			{Type: llm.PartText, Text: "draw it"},
			{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: upload}},
		}},
		{ID: 2, Sequence: 2, Role: llm.RoleAssistant, ResponseID: "r1", Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "image_generate", Arguments: []byte(`{}`)}}}},
		{ID: 3, Sequence: 3, Role: llm.RoleTool, ResponseID: "r1", Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{
			ID: "c1", Name: "image_generate", Content: "Generated image successfully.", Images: []string{generated, notImage},
		}}}},
		{ID: 4, Sequence: 4, Role: llm.RoleAssistant, ResponseID: "r1", Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "c2", Name: "show_media", Arguments: []byte(`{}`)}}}},
		{ID: 5, Sequence: 5, Role: llm.RoleTool, ResponseID: "r1", Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{
			ID: "c2", Name: "show_media", Content: "Media ready.", Media: []llm.MediaArtifact{{Reference: reference, StoredPath: shown, SourcePath: "/nonexistent", MediaType: "image/png", Name: "shown.png"}},
		}}}},
		{ID: 6, Sequence: 6, Role: llm.RoleAssistant, ResponseID: "r1", TextContent: "Here:\n\n![A chart](term-llm-media://" + reference + ")",
			Parts: []llm.Part{{Type: llm.PartText, Text: "Here:\n\n![A chart](term-llm-media://" + reference + ")"}}},
	}
	return sess, messages, dir
}

func bundleByName(files []share.File) map[string]share.File {
	out := map[string]share.File{}
	for _, file := range files {
		out[file.Name] = file
	}
	return out
}

func TestShareBundleAssetsMode(t *testing.T) {
	sess, messages, dir := imageShareFixture(t)
	files, err := ShareBundle(sess, messages, ExportOptions{Images: ExportImagesAssets, AssetMediaTypes: []string{"image/png", "image/jpeg"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := share.ValidateRequest(share.Request{RequestID: "r", Visibility: share.VisibilityUnlisted, Entrypoint: "index.html", Files: files}); err != nil {
		t.Fatalf("bundle fails protocol validation: %v", err)
	}
	byName := bundleByName(files)
	var assets []share.File
	for _, file := range files {
		if strings.HasPrefix(file.Name, "assets/") {
			assets = append(assets, file)
			if file.Role != share.RoleAsset || file.MediaType != "image/png" {
				t.Fatalf("asset metadata = %+v", file)
			}
			if _, format, err := image.DecodeConfig(bytes.NewReader(file.Content)); err != nil || format != "png" {
				t.Fatalf("asset %s is not a PNG: %v", file.Name, err)
			}
		}
	}
	if len(assets) != 3 {
		t.Fatalf("got %d assets, want upload + generated + shown", len(assets))
	}
	html := string(byName["index.html"].Content)
	markdown := string(byName["session.md"].Content)
	for _, asset := range assets {
		if !strings.Contains(html, `src="`+asset.Name+`"`) {
			t.Errorf("HTML does not reference %s", asset.Name)
		}
		if !strings.Contains(markdown, "]("+asset.Name+")") {
			t.Errorf("Markdown does not reference %s", asset.Name)
		}
	}
	if !strings.Contains(html, `img-src data: 'self'`) {
		t.Error("asset bundle CSP does not allow same-origin images")
	}
	if strings.Contains(html, "data:image/") {
		t.Error("asset bundle still inlines images")
	}
	for _, leaked := range []string{dir, "term-llm-media://", "TOP-SECRET-TEXT", "/nonexistent"} {
		if strings.Contains(html, leaked) || strings.Contains(markdown, leaked) {
			t.Errorf("bundle leaked %q", leaked)
		}
	}
	if !strings.Contains(html, `alt="A chart"`) {
		t.Error("referenced media lost its alt text")
	}
	if !strings.Contains(html, "Image omitted — unsupported format") {
		t.Error("non-image path was not reported as omitted")
	}
}

func TestShareBundleDeduplicatesAssets(t *testing.T) {
	sess, messages, _ := imageShareFixture(t)
	duplicate := messages[0]
	duplicate.ID, duplicate.Sequence = 7, 7
	messages = append(messages, duplicate)
	files, err := ShareBundle(sess, messages, ExportOptions{Images: ExportImagesAssets, AssetMediaTypes: []string{"image/png"}})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, file := range files {
		if strings.HasPrefix(file.Name, "assets/") {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("got %d assets, want 3 after deduplication", count)
	}
}

func TestShareBundleInlineAndNoneModes(t *testing.T) {
	sess, messages, _ := imageShareFixture(t)
	files, err := ShareBundle(sess, messages, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("inline mode produced %d files", len(files))
	}
	html := string(bundleByName(files)["index.html"].Content)
	if strings.Count(html, `src="data:image/png;base64,`) != 3 {
		t.Fatalf("inline mode did not embed all three images")
	}
	if strings.Contains(html, `'self'`) {
		t.Error("inline CSP was widened")
	}

	files, err = ShareBundle(sess, messages, ExportOptions{Images: ExportImagesNone})
	if err != nil {
		t.Fatal(err)
	}
	html = string(bundleByName(files)["index.html"].Content)
	if len(files) != 2 || strings.Contains(html, "data:image/") || strings.Contains(html, "<img") {
		t.Fatal("none mode included images")
	}
}

func TestAssetModeFallsBackToInlineWithoutUsableTypes(t *testing.T) {
	sess, messages, _ := imageShareFixture(t)
	files, err := ShareBundle(sess, messages, ExportOptions{Images: ExportImagesAssets, AssetMediaTypes: []string{"image/gif"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || !strings.Contains(string(bundleByName(files)["index.html"].Content), "data:image/png") {
		t.Fatal("expected inline fallback")
	}
}

func TestSelectShareResponseKeepsDisplayedImages(t *testing.T) {
	_, messages, _ := imageShareFixture(t)
	selection, err := SelectShare(messages, 6, ShareScopeResponse)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Messages) != 1 || len(selection.Media) != 1 {
		t.Fatalf("selection = %+v", selection)
	}
	var images, texts int
	for _, part := range selection.Messages[0].Parts {
		switch part.Type {
		case llm.PartImage:
			images++
		case llm.PartText:
			texts++
		case llm.PartToolCall, llm.PartToolResult:
			t.Fatal("response share exposed tool activity")
		}
	}
	if images != 2 || texts != 1 {
		t.Fatalf("got %d images and %d text parts", images, texts)
	}
	sess := &Session{ID: "sess", CreatedAt: time.Now()}
	files, err := ShareBundle(sess, selection.Messages, ExportOptions{ResponseOnly: true, Images: ExportImagesAssets, AssetMediaTypes: []string{"image/png"}, Media: selection.Media})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(files); got != 4 { // index.html, session.md, generated, shown
		t.Fatalf("response bundle has %d files", got)
	}
	if html := string(bundleByName(files)["index.html"].Content); strings.Contains(html, "image_generate") {
		t.Error("response share rendered tool details")
	}
}

func TestShareImageOptions(t *testing.T) {
	if mode, _ := ShareImageOptions(share.Capabilities{AssetMediaTypes: []string{"image/png"}}, false); mode != ExportImagesNone {
		t.Fatalf("excluded images mode = %q", mode)
	}
	if mode, types := ShareImageOptions(share.Capabilities{AssetMediaTypes: []string{"image/png"}}, true); mode != ExportImagesAssets || len(types) != 1 {
		t.Fatalf("asset mode = %q %v", mode, types)
	}
	if mode, _ := ShareImageOptions(share.Capabilities{}, true); mode != ExportImagesInline {
		t.Fatalf("legacy provider mode = %q", mode)
	}
}

func TestEncodeExportImageChoosesFormatByContent(t *testing.T) {
	flat := image.NewNRGBA(image.Rect(0, 0, 300, 200))
	noisy := image.NewNRGBA(image.Rect(0, 0, 300, 200))
	seed := uint32(1)
	for i := 0; i < len(flat.Pix); i += 4 {
		copy(flat.Pix[i:i+4], []byte{240, 240, 240, 255})
		if (i/4)%7 == 0 {
			copy(flat.Pix[i:i+4], []byte{20, 20, 20, 255})
		}
		seed = seed*1664525 + 1013904223
		copy(noisy.Pix[i:i+4], []byte{byte(seed >> 8), byte(seed >> 16), byte(seed >> 24), 255})
	}
	if _, mediaType, err := encodeExportImage(flat, allImageTypes); err != nil || mediaType != "image/png" {
		t.Fatalf("flat graphic encoded as %q, err %v", mediaType, err)
	}
	if _, mediaType, err := encodeExportImage(noisy, allImageTypes); err != nil || mediaType != "image/jpeg" {
		t.Fatalf("photographic image encoded as %q, err %v", mediaType, err)
	}
}
