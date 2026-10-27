package qrcode

import (
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteColorFileRejectsInvalidContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"numeric", strings.Repeat("0", 7090)},
		{"alphanumeric", strings.Repeat("A", 4297)},
		{"byte", strings.Repeat("#", 2954)},
	}
	for _, test := range tests {
		for _, level := range []RecoveryLevel{Low, Medium, High, Highest} {
			t.Run(fmt.Sprintf("%s/level-%d", test.name, level), func(t *testing.T) {
				filename := filepath.Join(t.TempDir(), "qr.png")
				if err := WriteColorFile(test.content, level, 256, color.White, color.Black, filename); err == nil {
					t.Fatal("WriteColorFile accepted invalid content")
				}
				if _, err := os.Stat(filename); !os.IsNotExist(err) {
					t.Fatalf("WriteColorFile must not create a file on error; stat error = %v", err)
				}
			})
		}
	}
}

func TestTimingPatternsDoNotOccupySeparators(t *testing.T) {
	for version := 1; version <= 40; version++ {
		for _, quietZone := range []int{0, 4} {
			t.Run(fmt.Sprintf("version-%d/quiet-zone-%d", version, quietZone), func(t *testing.T) {
				v := getQRCodeVersion(Low, version)
				size := v.symbolSize()
				m := &regularSymbol{version: *v, size: size, symbol: newSymbol(size, quietZone)}

				// Draw timing patterns alone: finder patterns own the separators
				// at coordinates 7 and size-8, even though both draw them white.
				m.addTimingPatterns()
				for i := 0; i < size; i++ {
					wantUsed := i >= 8 && i <= size-9
					for _, point := range [][2]int{{i, 6}, {6, i}} {
						x, y := point[0], point[1]
						if gotUsed := !m.symbol.empty(x, y); gotUsed != wantUsed {
							t.Errorf("timing module (%d, %d) used = %t, want %t", x, y, gotUsed, wantUsed)
						}
						if wantUsed && m.symbol.get(x, y) != (i%2 == 0) {
							t.Errorf("timing module (%d, %d) has incorrect alternating value", x, y)
						}
					}
				}
			})
		}
	}
}
