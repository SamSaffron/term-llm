package termimage

import (
	"bytes"
	"fmt"
	stdimage "image"
	"image/color"

	"golang.org/x/image/draw"
)

func renderANSI(img stdimage.Image, req Request, cellW, cellH int, cacheKey string) (Result, error) {
	cols, rows := fitCells(img, req, cellW, cellH)
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}

	// One terminal row is represented by an upper-half block with separate
	// foreground/background colors, so render two sampled image rows per cell row.
	target := stdimage.NewNRGBA(stdimage.Rect(0, 0, cols, rows*2))
	draw.CatmullRom.Scale(target, target.Bounds(), img, img.Bounds(), draw.Over, nil)

	bg := normalizeBackground(req.Background)
	var b bytes.Buffer
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			top := compositeNRGBA(target.NRGBAAt(x, y*2), bg)
			bottom := compositeNRGBA(target.NRGBAAt(x, y*2+1), bg)
			fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀", top.R, top.G, top.B, bottom.R, bottom.G, bottom.B)
		}
		b.WriteString("\x1b[0m")
		if y < rows-1 {
			b.WriteByte('\n')
		}
	}
	s := b.String()
	return Result{
		Protocol:    ProtocolANSI,
		Display:     s,
		Full:        s,
		WidthCells:  cols,
		HeightCells: rows,
		CacheKey:    cacheKey,
	}, nil
}

func compositeNRGBA(src color.NRGBA, bg color.NRGBA) color.NRGBA {
	if src.A == 255 {
		return src
	}
	if src.A == 0 {
		return bg
	}
	alpha := float64(src.A) / 255.0
	return color.NRGBA{
		R: uint8(float64(src.R)*alpha + float64(bg.R)*(1-alpha) + 0.5),
		G: uint8(float64(src.G)*alpha + float64(bg.G)*(1-alpha) + 0.5),
		B: uint8(float64(src.B)*alpha + float64(bg.B)*(1-alpha) + 0.5),
		A: 255,
	}
}
