package termimage

import (
	"bytes"
	"encoding/base64"
	"fmt"
	stdimage "image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/BourgeoisBear/rasterm"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const cacheLimit = 100

var (
	cacheMu     sync.Mutex
	renderCache = make(map[string]cacheEntry)
	cacheOrder  []string

	imageIDCounter uint32
)

type cacheEntry struct {
	result Result
}

// ClearCache clears cached terminal image render results.
func ClearCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	renderCache = make(map[string]cacheEntry)
	cacheOrder = nil
}

// Render renders an image using the current process environment.
func Render(req Request) (Result, error) {
	return RenderWithEnvironment(req, DefaultEnvironment())
}

// RenderWithEnvironment renders an image using an explicit environment. It is
// primarily useful for deterministic tests and debugging.
func RenderWithEnvironment(req Request, env Environment) (Result, error) {
	req = normalizeRequest(req)
	strategy := Select(req, env)
	if strategy.Protocol == ProtocolNone {
		return Result{Protocol: ProtocolNone, TextFallback: req.Path, Warnings: strategy.Warnings}, nil
	}

	info, err := os.Stat(req.Path)
	if err != nil {
		return Result{Protocol: strategy.Protocol, TextFallback: req.Path, Warnings: strategy.Warnings}, fmt.Errorf("stat image: %w", err)
	}

	cellW, cellH := resolveCellSize(req)
	baseKey := buildCacheKey(req, strategy.Protocol, info.ModTime().UnixNano(), info.Size(), cellW, cellH)
	if cached, ok := getCached(baseKey); ok {
		return cached, nil
	}

	img, err := loadImage(req.Path)
	if err != nil {
		return Result{Protocol: strategy.Protocol, TextFallback: req.Path, Warnings: strategy.Warnings}, fmt.Errorf("load image: %w", err)
	}

	var result Result
	switch strategy.Protocol {
	case ProtocolKitty:
		result, err = renderKitty(img, req, cellW, cellH, baseKey)
	case ProtocolITerm:
		result, err = renderITerm(img, req, cellW, cellH, baseKey)
	case ProtocolSixel:
		result, err = renderSixel(img, req, cellW, cellH, baseKey)
	case ProtocolANSI:
		result, err = renderANSI(img, req, cellW, cellH, baseKey)
	default:
		result = Result{Protocol: ProtocolNone, TextFallback: req.Path, CacheKey: baseKey}
	}
	if err != nil {
		return Result{Protocol: strategy.Protocol, TextFallback: req.Path, Warnings: strategy.Warnings, CacheKey: baseKey}, err
	}
	result.TextFallback = req.Path
	result.Warnings = append(result.Warnings, strategy.Warnings...)
	if result.CacheKey == "" {
		result.CacheKey = baseKey
	}
	debugf(env, "path=%s mode=%s strategy=%s protocol=%s cells=%dx%d upload=%d display=%d", req.Path, req.Mode, strategy.Name, result.Protocol, result.WidthCells, result.HeightCells, len(result.Upload), len(result.Display))
	putCached(baseKey, result)
	return result, nil
}

func normalizeRequest(req Request) Request {
	req.Path = strings.TrimSpace(req.Path)
	req.Mode = normalizeMode(req.Mode)
	req.Protocol = normalizeProtocol(req.Protocol)
	if req.MaxCols <= 0 {
		req.MaxCols = DefaultMaxCols
	}
	if req.MaxRows <= 0 {
		req.MaxRows = DefaultMaxRows
	}
	if req.MaxCols < 1 {
		req.MaxCols = 1
	}
	if req.MaxRows < 1 {
		req.MaxRows = 1
	}
	return req
}

func getCached(key string) (Result, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	entry, ok := renderCache[key]
	return entry.result, ok
}

func putCached(key string, result Result) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if _, exists := renderCache[key]; exists {
		renderCache[key] = cacheEntry{result: result}
		return
	}
	for len(renderCache) >= cacheLimit && len(cacheOrder) > 0 {
		oldest := cacheOrder[0]
		cacheOrder = cacheOrder[1:]
		delete(renderCache, oldest)
	}
	renderCache[key] = cacheEntry{result: result}
	cacheOrder = append(cacheOrder, key)
}

func buildCacheKey(req Request, protocol Protocol, modTime int64, size int64, cellW, cellH int) string {
	bg := normalizeBackground(req.Background)
	return fmt.Sprintf("%s|%d|%d|%s|%s|%dx%d|cell=%dx%d|bg=%d,%d,%d,%d", req.Path, modTime, size, req.Mode, protocol, req.MaxCols, req.MaxRows, cellW, cellH, bg.R, bg.G, bg.B, bg.A)
}

func resolveCellSize(req Request) (int, int) {
	cellW := req.CellWidthPx
	cellH := req.CellHeightPx
	if cellW <= 0 || cellH <= 0 {
		if w, h, ok := terminalCellSizeFromTTY(); ok {
			if cellW <= 0 {
				cellW = w
			}
			if cellH <= 0 {
				cellH = h
			}
		}
	}
	if cellW <= 0 {
		cellW = DefaultCellWidthPx
	}
	if cellH <= 0 {
		cellH = DefaultCellHeightPx
	}
	return cellW, cellH
}

func nextImageID() uint32 {
	id := atomic.AddUint32(&imageIDCounter, 1)
	return (id % 16777215) + 1
}

func loadImage(path string) (stdimage.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := stdimage.Decode(f)
	if err != nil {
		return nil, err
	}
	return img, nil
}

func scaledForCells(img stdimage.Image, cols, rows, cellW, cellH int) stdimage.Image {
	bounds := img.Bounds()
	targetW := cols * cellW
	targetH := rows * cellH
	if targetW <= 0 || targetH <= 0 {
		return img
	}
	if bounds.Dx() == targetW && bounds.Dy() == targetH {
		return img
	}
	dst := stdimage.NewRGBA(stdimage.Rect(0, 0, targetW, targetH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
	return dst
}

func fitCells(img stdimage.Image, req Request, cellW, cellH int) (int, int) {
	bounds := img.Bounds()
	maxCols := min(req.MaxCols, len(rowColDiacritics))
	maxRows := min(req.MaxRows, len(rowColDiacritics))
	if maxCols < 1 {
		maxCols = 1
	}
	if maxRows < 1 {
		maxRows = 1
	}
	cols := int(math.Ceil(float64(bounds.Dx()) / float64(cellW)))
	rows := int(math.Ceil(float64(bounds.Dy()) / float64(cellH)))
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	if cols > maxCols || rows > maxRows {
		scale := math.Min(float64(maxCols)/float64(cols), float64(maxRows)/float64(rows))
		if scale <= 0 {
			scale = 1
		}
		cols = int(math.Floor(float64(cols) * scale))
		rows = int(math.Floor(float64(rows) * scale))
		if cols < 1 {
			cols = 1
		}
		if rows < 1 {
			rows = 1
		}
	}
	if cols > maxCols {
		cols = maxCols
	}
	if rows > maxRows {
		rows = maxRows
	}
	return cols, rows
}

func renderITerm(img stdimage.Image, req Request, cellW, cellH int, cacheKey string) (Result, error) {
	cols, rows := fitCells(img, req, cellW, cellH)
	img = scaledForCells(img, cols, rows, cellW, cellH)
	var buf bytes.Buffer
	if err := rasterm.ItermWriteImage(&buf, img); err != nil {
		return Result{}, err
	}
	s := buf.String()
	return Result{Protocol: ProtocolITerm, Display: s, Full: s, WidthCells: cols, HeightCells: rows, CacheKey: cacheKey}, nil
}

func renderSixel(img stdimage.Image, req Request, cellW, cellH int, cacheKey string) (Result, error) {
	cols, rows := fitCells(img, req, cellW, cellH)
	img = scaledForCells(img, cols, rows, cellW, cellH)
	var buf bytes.Buffer
	paletted := convertToPaletted(img)
	if err := rasterm.SixelWriteImage(&buf, paletted); err != nil {
		return Result{}, err
	}
	s := buf.String()
	return Result{Protocol: ProtocolSixel, Display: s, Full: s, WidthCells: cols, HeightCells: rows, CacheKey: cacheKey}, nil
}

func encodePNGBase64(img stdimage.Image) (string, error) {
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pngBuf.Bytes()), nil
}

func convertToPaletted(img stdimage.Image) *stdimage.Paletted {
	bounds := img.Bounds()
	palette := make(color.Palette, 256)
	idx := 0
	for r := 0; r < 6; r++ {
		for g := 0; g < 6; g++ {
			for b := 0; b < 6; b++ {
				palette[idx] = color.RGBA{R: uint8(r * 51), G: uint8(g * 51), B: uint8(b * 51), A: 255}
				idx++
			}
		}
	}
	for i := 0; i < 40; i++ {
		gray := uint8(i * 255 / 39)
		palette[idx] = color.RGBA{R: gray, G: gray, B: gray, A: 255}
		idx++
	}
	paletted := stdimage.NewPaletted(bounds, palette)
	draw.FloydSteinberg.Draw(paletted, bounds, img, bounds.Min)
	return paletted
}

func normalizeBackground(c color.Color) color.NRGBA {
	if c == nil {
		return color.NRGBA{R: 0x1d, G: 0x20, B: 0x21, A: 0xff}
	}
	bg := color.NRGBAModel.Convert(c).(color.NRGBA)
	if bg.A == 0 {
		return color.NRGBA{R: 0x1d, G: 0x20, B: 0x21, A: 0xff}
	}
	if bg.A < 255 {
		alpha := float64(bg.A) / 255.0
		fallback := color.NRGBA{R: 0x1d, G: 0x20, B: 0x21, A: 0xff}
		return color.NRGBA{
			R: uint8(float64(bg.R)*alpha + float64(fallback.R)*(1-alpha) + 0.5),
			G: uint8(float64(bg.G)*alpha + float64(fallback.G)*(1-alpha) + 0.5),
			B: uint8(float64(bg.B)*alpha + float64(fallback.B)*(1-alpha) + 0.5),
			A: 255,
		}
	}
	return bg
}
