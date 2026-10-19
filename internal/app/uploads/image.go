package uploads

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
)

const MaxImageMetadataBytes = 1 << 20

// ImageDisplayDimensions returns decoded dimensions adjusted for JPEG EXIF orientation.
func ImageDisplayDimensions(raw []byte) (width, height int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0
	}
	width, height = cfg.Width, cfg.Height
	if orientation := jpegEXIFOrientation(raw); orientation >= 5 && orientation <= 8 {
		width, height = height, width
	}
	return width, height
}

// UploadedImageDimensions accepts client dimensions only when they exactly
// match the server-derived, EXIF-aware display dimensions.
func UploadedImageDimensions(raw []byte, clientWidth, clientHeight int) (width, height int) {
	width, height = ImageDisplayDimensions(raw)
	if width <= 0 || height <= 0 {
		return 0, 0
	}
	if clientWidth == width && clientHeight == height {
		return clientWidth, clientHeight
	}
	return width, height
}

// JSONImageDimension decodes one positive platform-sized JSON image dimension.
func JSONImageDimension(raw json.RawMessage) int {
	var value int64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value <= 0 || value > int64(^uint(0)>>1) {
		return 0
	}
	return int(value)
}

func jpegEXIFOrientation(raw []byte) int {
	if len(raw) < 4 || raw[0] != 0xff || raw[1] != 0xd8 {
		return 0
	}
	for offset := 2; offset+4 <= len(raw); {
		if raw[offset] != 0xff {
			offset++
			continue
		}
		for offset < len(raw) && raw[offset] == 0xff {
			offset++
		}
		if offset >= len(raw) {
			return 0
		}
		marker := raw[offset]
		offset++
		if marker == 0xd9 || marker == 0xda {
			return 0
		}
		if marker == 0x00 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			continue
		}
		if offset+2 > len(raw) {
			return 0
		}
		segmentLength := int(binary.BigEndian.Uint16(raw[offset : offset+2]))
		if segmentLength < 2 || offset+segmentLength > len(raw) {
			return 0
		}
		payload := raw[offset+2 : offset+segmentLength]
		if marker == 0xe1 && len(payload) >= 6 && bytes.Equal(payload[:6], []byte("Exif\x00\x00")) {
			if orientation := tiffOrientation(payload[6:]); orientation != 0 {
				return orientation
			}
		}
		offset += segmentLength
	}
	return 0
}

func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 0
	}
	ifdOffset := uint64(order.Uint32(tiff[4:8]))
	if ifdOffset+2 > uint64(len(tiff)) {
		return 0
	}
	entryCount := uint64(order.Uint16(tiff[ifdOffset : ifdOffset+2]))
	entriesStart := ifdOffset + 2
	if entryCount > (uint64(len(tiff))-entriesStart)/12 {
		return 0
	}
	for index := uint64(0); index < entryCount; index++ {
		entryOffset := entriesStart + index*12
		entry := tiff[entryOffset : entryOffset+12]
		if order.Uint16(entry[0:2]) != 0x0112 || order.Uint16(entry[2:4]) != 3 || order.Uint32(entry[4:8]) != 1 {
			continue
		}
		orientation := int(order.Uint16(entry[8:10]))
		if orientation >= 1 && orientation <= 8 {
			return orientation
		}
		return 0
	}
	return 0
}
