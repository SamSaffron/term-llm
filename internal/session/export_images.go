package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register WebP decoding for exported images

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/share"
)

// ExportImageMode controls how transcript images are represented.
type ExportImageMode string

const (
	// ExportImagesInline embeds normalized images as data: URIs in the HTML
	// transcript, within a small total budget. It is the default.
	ExportImagesInline ExportImageMode = ""
	// ExportImagesAssets emits normalized images as separate bundle files under
	// assets/ and references them with relative URLs from HTML and Markdown.
	ExportImagesAssets ExportImageMode = "assets"
	// ExportImagesNone omits every image.
	ExportImagesNone ExportImageMode = "none"
)

const (
	maxExportImageSourceBytes = 64 << 20
	maxExportImagePixels      = 50_000_000
	maxExportImageDimension   = 20_000

	inlineExportImageMaxEdge = 1600
	assetExportImageMaxEdge  = 2048
	// Flat-graphics PNG output above this size is replaced by JPEG when the
	// image is opaque and the JPEG is smaller.
	exportImagePNGPreferredBytes = 1 << 20
	exportImageJPEGQuality       = 85

	maxExportAssetBytes      = 8 << 20
	maxExportAssetTotalBytes = 24 << 20
	// Leave room for index.html and session.md within the protocol file limit.
	maxExportAssets = share.MaxRequestFiles - 2

	exportAssetDir = "assets/"
)

// exportImageSource identifies image bytes stored in a session, either inline
// (base64) or as a local file path recorded by a tool or upload.
type exportImageSource struct {
	Base64    string
	MediaType string
	Path      string
	Alt       string
}

func (s exportImageSource) key() string {
	if s.Base64 != "" {
		sum := sha256.Sum256([]byte(s.Base64))
		return "b64:" + hex.EncodeToString(sum[:])
	}
	if s.Path != "" {
		return "path:" + filepath.Clean(s.Path)
	}
	return ""
}

type exportImageResult struct {
	URL       string
	MediaType string
	Omitted   bool
	Reason    string
}

type exportAsset struct {
	Name      string
	MediaType string
	Content   []byte
}

// exportImages resolves session images once per export so HTML and Markdown
// renderings share the same budget and asset names.
type exportImages struct {
	mode       ExportImageMode
	allowed    map[string]bool
	maxEdge    int
	budget     int
	used       int
	cache      map[string]exportImageResult
	assets     []exportAsset
	assetNames map[string]bool
}

func newExportImages(opts ExportOptions) *exportImages {
	images := &exportImages{
		mode:       opts.Images,
		allowed:    map[string]bool{},
		cache:      map[string]exportImageResult{},
		assetNames: map[string]bool{},
	}
	switch opts.Images {
	case ExportImagesAssets:
		images.maxEdge = assetExportImageMaxEdge
		images.budget = maxExportAssetTotalBytes
		for _, mediaType := range opts.AssetMediaTypes {
			mediaType = strings.ToLower(strings.TrimSpace(mediaType))
			if slices.Contains(share.AssetImageMediaTypes, mediaType) {
				images.allowed[mediaType] = true
			}
		}
		if !images.allowed["image/png"] && !images.allowed["image/jpeg"] {
			// Assets are only useful when at least one re-encoding target is
			// accepted; otherwise keep the transcript self-contained.
			images.mode = ExportImagesInline
		}
	}
	if images.mode == ExportImagesInline {
		images.maxEdge = inlineExportImageMaxEdge
		images.budget = maxHTMLExportInlineImageBytes
		images.allowed = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true}
	}
	return images
}

func (e *exportImages) assetsMode() bool {
	return e != nil && e.mode == ExportImagesAssets
}

func (e *exportImages) resolve(source exportImageSource) exportImageResult {
	if e == nil || e.mode == ExportImagesNone {
		return exportImageResult{Omitted: true, MediaType: normalizedMediaType(source.MediaType), Reason: "not included"}
	}
	key := source.key()
	if key == "" {
		return exportImageResult{Omitted: true, MediaType: normalizedMediaType(source.MediaType), Reason: "unavailable"}
	}
	if cached, ok := e.cache[key]; ok {
		return cached
	}
	result := e.resolveUncached(source)
	e.cache[key] = result
	return result
}

func (e *exportImages) resolveUncached(source exportImageSource) exportImageResult {
	declared := normalizedMediaType(source.MediaType)
	if e.used >= e.budget || (e.mode == ExportImagesAssets && len(e.assets) >= maxExportAssets) {
		// Avoid decoding images that cannot fit once the budget is spent.
		return exportImageResult{Omitted: true, MediaType: declared, Reason: "size limit reached"}
	}
	raw, err := readExportImageSource(source)
	if err != nil {
		return exportImageResult{Omitted: true, MediaType: declared, Reason: "unavailable"}
	}
	data, mediaType, err := normalizeExportImage(raw, e.maxEdge, e.allowed)
	if err != nil {
		return exportImageResult{Omitted: true, MediaType: declared, Reason: "unsupported format"}
	}
	limit := maxExportAssetBytes
	if e.mode == ExportImagesInline {
		limit = e.budget
	}
	if len(data) > limit || e.used+len(data) > e.budget {
		return exportImageResult{Omitted: true, MediaType: mediaType, Reason: "size limit reached"}
	}
	if e.mode == ExportImagesInline {
		e.used += len(data)
		return exportImageResult{URL: "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), MediaType: mediaType}
	}
	sum := sha256.Sum256(data)
	name := exportAssetDir + hex.EncodeToString(sum[:10]) + exportImageExtension(mediaType)
	if !e.assetNames[name] {
		if len(e.assets) >= maxExportAssets {
			return exportImageResult{Omitted: true, MediaType: mediaType, Reason: "size limit reached"}
		}
		e.assetNames[name] = true
		e.used += len(data)
		e.assets = append(e.assets, exportAsset{Name: name, MediaType: mediaType, Content: data})
	}
	return exportImageResult{URL: name, MediaType: mediaType}
}

func normalizedMediaType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if before, _, ok := strings.Cut(value, ";"); ok {
		value = strings.TrimSpace(before)
	}
	return value
}

func exportImageExtension(mediaType string) string {
	switch mediaType {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}

func readExportImageSource(source exportImageSource) ([]byte, error) {
	if source.Base64 != "" {
		if base64.StdEncoding.DecodedLen(len(source.Base64)) > maxExportImageSourceBytes {
			return nil, fmt.Errorf("image exceeds source limit")
		}
		return base64.StdEncoding.DecodeString(source.Base64)
	}
	if source.Path == "" {
		return nil, fmt.Errorf("image has no source")
	}
	file, err := os.Open(source.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxExportImageSourceBytes {
		return nil, fmt.Errorf("image source is not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxExportImageSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxExportImageSourceBytes {
		return nil, fmt.Errorf("image exceeds source limit")
	}
	return data, nil
}

// normalizeExportImage decodes untrusted image bytes, applies JPEG EXIF
// orientation, bounds the longest edge, and re-encodes without metadata. Only
// pixel data survives, so EXIF/XMP fields such as GPS location are dropped.
// Small static-size GIFs are passed through so animations survive; GIF has no
// EXIF block.
func normalizeExportImage(raw []byte, maxEdge int, allowed map[string]bool) ([]byte, string, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	switch format {
	case "png", "jpeg", "gif", "webp":
	default:
		return nil, "", fmt.Errorf("unsupported image format %q", format)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxExportImageDimension || config.Height > maxExportImageDimension ||
		int64(config.Width)*int64(config.Height) > maxExportImagePixels {
		return nil, "", fmt.Errorf("image dimensions are out of range")
	}
	if format == "gif" && allowed["image/gif"] && config.Width <= maxEdge && config.Height <= maxEdge {
		if _, err := gif.DecodeAll(bytes.NewReader(raw)); err == nil {
			return raw, "image/gif", nil
		}
	}
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	img := downscaleExportImage(decoded, maxEdge)
	if format == "jpeg" {
		img = applyEXIFOrientation(img, jpegEXIFOrientation(raw))
	}
	return encodeExportImage(img, allowed)
}

func downscaleExportImage(src image.Image, maxEdge int) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if maxEdge <= 0 || (width <= maxEdge && height <= maxEdge) {
		return src
	}
	if width >= height {
		height = max(1, height*maxEdge/width)
		width = maxEdge
	} else {
		width = max(1, width*maxEdge/height)
		height = maxEdge
	}
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Src, nil)
	return dst
}

func encodeExportImage(img image.Image, allowed map[string]bool) ([]byte, string, error) {
	opaque := imageIsOpaque(img)
	allowPNG, allowJPEG := allowed["image/png"], allowed["image/jpeg"]
	// Photographic content goes straight to JPEG; PNG is reserved for
	// transparency and flat graphics such as screenshots, where it is both
	// smaller and sharper. This avoids an expensive speculative PNG encode.
	if allowJPEG && (!allowPNG || (opaque && looksPhotographic(img))) {
		return encodeExportJPEG(img, opaque)
	}
	if !allowPNG {
		return nil, "", fmt.Errorf("no permitted image encoding")
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, "", err
	}
	if opaque && allowJPEG && out.Len() > exportImagePNGPreferredBytes {
		if jpegData, mediaType, err := encodeExportJPEG(img, true); err == nil && len(jpegData) < out.Len() {
			return jpegData, mediaType, nil
		}
	}
	return out.Bytes(), "image/png", nil
}

func encodeExportJPEG(img image.Image, opaque bool) ([]byte, string, error) {
	if !opaque {
		img = flattenOnWhite(img)
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: exportImageJPEGQuality}); err != nil {
		return nil, "", err
	}
	return out.Bytes(), "image/jpeg", nil
}

// looksPhotographic samples up to ~16k pixels and reports whether they contain
// many distinct colours, which distinguishes photos and rendered artwork from
// screenshots, diagrams, and other flat graphics.
func looksPhotographic(img image.Image) bool {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return false
	}
	step := 1
	for (width/step)*(height/step) > 16384 {
		step++
	}
	colors := make(map[uint32]struct{}, 4096)
	for y := bounds.Min.Y; y < bounds.Max.Y; y += step {
		for x := bounds.Min.X; x < bounds.Max.X; x += step {
			r, g, b, _ := img.At(x, y).RGBA()
			colors[(r>>8)<<16|(g>>8)<<8|b>>8] = struct{}{}
		}
	}
	return len(colors) > 4096
}

func imageIsOpaque(img image.Image) bool {
	if opaque, ok := img.(interface{ Opaque() bool }); ok {
		return opaque.Opaque()
	}
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return false
			}
		}
	}
	return true
}

func flattenOnWhite(img image.Image) image.Image {
	bounds := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, bounds.Min, draw.Over)
	return dst
}

// jpegEXIFOrientation returns the EXIF orientation (1-8) from a JPEG APP1
// segment, or 1 when absent or malformed.
func jpegEXIFOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	for pos := 2; pos+4 <= len(data); {
		if data[pos] != 0xFF {
			return 1
		}
		marker := data[pos+1]
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			pos += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			return 1
		}
		length := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		if length < 2 || pos+2+length > len(data) {
			return 1
		}
		segment := data[pos+4 : pos+2+length]
		if marker == 0xE1 && len(segment) >= 6 && string(segment[:6]) == "Exif\x00\x00" {
			return tiffOrientation(segment[6:])
		}
		pos += 2 + length
	}
	return 1
}

func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 1
	}
	offset := int(order.Uint32(tiff[4:8]))
	if offset < 8 || offset+2 > len(tiff) {
		return 1
	}
	count := int(order.Uint16(tiff[offset : offset+2]))
	for i := 0; i < count; i++ {
		entry := offset + 2 + i*12
		if entry+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[entry:entry+2]) != 0x0112 {
			continue
		}
		if order.Uint16(tiff[entry+2:entry+4]) != 3 { // SHORT
			return 1
		}
		value := int(order.Uint16(tiff[entry+8 : entry+10]))
		if value < 1 || value > 8 {
			return 1
		}
		return value
	}
	return 1
}

// applyEXIFOrientation transforms img so it displays upright once the EXIF
// orientation tag has been stripped by re-encoding.
func applyEXIFOrientation(img image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return img
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	dstWidth, dstHeight := width, height
	if orientation >= 5 {
		dstWidth, dstHeight = height, width
	}
	src := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(src, src.Bounds(), img, bounds.Min, draw.Src)
	dst := image.NewNRGBA(image.Rect(0, 0, dstWidth, dstHeight))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var dx, dy int
			switch orientation {
			case 2:
				dx, dy = width-1-x, y
			case 3:
				dx, dy = width-1-x, height-1-y
			case 4:
				dx, dy = x, height-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = height-1-y, x
			case 7:
				dx, dy = height-1-y, width-1-x
			case 8:
				dx, dy = y, width-1-x
			}
			si := src.PixOffset(x, y)
			di := dst.PixOffset(dx, dy)
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}

// toolResultDisplayedImages returns image sources that clients display beside
// a tool result without a Markdown reference: legacy image paths (for example
// image_generate output) and unreferenced image media artifacts.
func toolResultDisplayedImages(result *llm.ToolResult) []exportImageSource {
	if result == nil {
		return nil
	}
	var sources []exportImageSource
	seen := map[string]bool{}
	add := func(source exportImageSource) {
		key := source.key()
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		sources = append(sources, source)
	}
	for _, item := range result.Media {
		if strings.TrimSpace(item.Reference) != "" || !isExportImageMediaType(item.MediaType) {
			continue
		}
		add(exportImageSource{Path: item.Path(), MediaType: item.MediaType, Alt: exportMediaAlt(item)})
	}
	for _, path := range result.Images {
		add(exportImageSource{Path: path, Alt: exportImageAltForTool(result.Name)})
	}
	return sources
}

func isExportImageMediaType(mediaType string) bool {
	mediaType = normalizedMediaType(mediaType)
	return mediaType == "" || strings.HasPrefix(mediaType, "image/")
}

func exportMediaAlt(item llm.MediaArtifact) string {
	for _, value := range []string{item.Caption, item.Name} {
		if value = strings.Join(strings.Fields(value), " "); value != "" {
			return value
		}
	}
	return "Image"
}

func exportImageAltForTool(name string) string {
	if name == "image_generate" {
		return "Generated image"
	}
	return "Image"
}

func partImageSource(part llm.Part, alt string) exportImageSource {
	source := exportImageSource{Path: part.ImagePath, Alt: alt}
	if part.ImageData != nil {
		source.Base64 = part.ImageData.Base64
		source.MediaType = part.ImageData.MediaType
		if source.Base64 != "" {
			source.Path = ""
		}
	}
	return source
}

// ShareImageOptions selects the image representation for a share provider.
// include=false omits every image; providers advertising PNG or JPEG assets
// receive separate files, and all others receive bounded inline images.
func ShareImageOptions(capabilities share.Capabilities, include bool) (ExportImageMode, []string) {
	if !include {
		return ExportImagesNone, nil
	}
	if capabilities.SupportsAssetMediaType("image/png") || capabilities.SupportsAssetMediaType("image/jpeg") {
		return ExportImagesAssets, append([]string(nil), capabilities.AssetMediaTypes...)
	}
	return ExportImagesInline, nil
}
