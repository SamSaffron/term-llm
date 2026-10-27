// go-qrcode
// Copyright 2014 Tom Harwood

package qrcode

import (
	"log"

	bitset "github.com/samsaffron/term-llm/internal/qrcode/bitset"
)

// Error detection/recovery capacity.
//
// There are several levels of error detection/recovery capacity. Higher levels
// of error recovery are able to correct more errors, with the trade-off of
// increased symbol size.
type RecoveryLevel int

const (
	// Level L: 7% error recovery.
	Low RecoveryLevel = iota

	// Level M: 15% error recovery. Good default choice.
	Medium

	// Level Q: 25% error recovery.
	High

	// Level H: 30% error recovery.
	Highest
)

// qrCodeVersion describes the data length and encoding order of a single QR
// Code version. There are 40 versions numbers x 4 recovery levels == 160
// possible qrCodeVersion structures.
type qrCodeVersion struct {
	// Version number (1-40 inclusive).
	version int

	// Error recovery level.
	level RecoveryLevel

	dataEncoderType dataEncoderType

	// Encoded data can be split into multiple blocks. Each block contains data
	// and error recovery bytes.
	//
	// Larger QR Codes contain more blocks.
	block []block

	// Number of bits required to pad the combined data & error correction bit
	// stream up to the symbol's full capacity.
	numRemainderBits int
}

type block struct {
	numBlocks int

	// Total codewords (numCodewords == numErrorCodewords+numDataCodewords).
	numCodewords int

	// Number of data codewords.
	numDataCodewords int
}

const (
	formatInfoLengthBits  = 15
	versionInfoLengthBits = 18
)

// formatInfo returns the 15-bit Format Information value for a QR
// code.
func (v qrCodeVersion) formatInfo(maskPattern int) *bitset.Bitset {
	formatID := 0

	switch v.level {
	case Low:
		formatID = 0x08 // 0b01000
	case Medium:
		formatID = 0x00 // 0b00000
	case High:
		formatID = 0x18 // 0b11000
	case Highest:
		formatID = 0x10 // 0b10000
	default:
		log.Panicf("Invalid level %d", v.level)
	}

	if maskPattern < 0 || maskPattern > 7 {
		log.Panicf("Invalid maskPattern %d", maskPattern)
	}

	formatID |= maskPattern & 0x7

	result := bitset.New()

	result.AppendUint32(formatBitSequence[formatID].regular, formatInfoLengthBits)

	return result
}

// versionInfo returns the 18-bit Version Information value for a QR Code.
//
// Version Information is applicable only to QR Codes versions 7-40 inclusive.
// nil is returned if Version Information is not required.
func (v qrCodeVersion) versionInfo() *bitset.Bitset {
	if v.version < 7 {
		return nil
	}

	result := bitset.New()
	result.AppendUint32(versionBitSequence[v.version], 18)

	return result
}

// numDataBits returns the data capacity in bits.
func (v qrCodeVersion) numDataBits() int {
	numDataBits := 0
	for _, b := range v.block {
		numDataBits += 8 * b.numBlocks * b.numDataCodewords // 8 bits in a byte
	}

	return numDataBits
}

// chooseQRCodeVersion chooses the most suitable QR Code version for a stated
// data length in bits, the error recovery level required, and the data encoder
// used.
//
// The chosen QR Code version is the smallest version able to fit numDataBits
// and the optional terminator bits required by the specified encoder.
//
// On success the chosen QR Code version is returned.
func chooseQRCodeVersion(level RecoveryLevel, encoder *dataEncoder, numDataBits int) *qrCodeVersion {
	var chosenVersion *qrCodeVersion

	for _, v := range versions {
		if v.level != level {
			continue
		} else if v.version < encoder.minVersion {
			continue
		} else if v.version > encoder.maxVersion {
			break
		}

		numFreeBits := v.numDataBits() - numDataBits

		if numFreeBits >= 0 {
			chosenVersion = &v
			break
		}
	}

	return chosenVersion
}

func (v qrCodeVersion) numTerminatorBitsRequired(numDataBits int) int {
	numFreeBits := v.numDataBits() - numDataBits

	var numTerminatorBits int

	switch {
	case numFreeBits >= 4:
		numTerminatorBits = 4
	default:
		numTerminatorBits = numFreeBits
	}

	return numTerminatorBits
}

// numBlocks returns the number of blocks.
func (v qrCodeVersion) numBlocks() int {
	numBlocks := 0

	for _, b := range v.block {
		numBlocks += b.numBlocks
	}

	return numBlocks
}

// numBitsToPadToCodeword returns the number of bits required to pad data of
// length numDataBits upto the nearest codeword size.
func (v qrCodeVersion) numBitsToPadToCodeword(numDataBits int) int {
	if numDataBits == v.numDataBits() {
		return 0
	}

	return (8 - numDataBits%8) % 8
}

// symbolSize returns the size of the QR Code symbol in number of modules (which
// is both the width and height, since QR codes are square). The QR Code has
// size symbolSize() x symbolSize() pixels. This does not include the quiet
// zone.
func (v qrCodeVersion) symbolSize() int {
	return 21 + (v.version-1)*4
}

// quietZoneSize returns the number of pixels of border space on each side of
// the QR Code. The quiet space assists with decoding.
func (v qrCodeVersion) quietZoneSize() int {
	return 4
}

// getQRCodeVersion returns the QR Code version by version number and recovery
// level. Returns nil if the requested combination is not defined.
func getQRCodeVersion(level RecoveryLevel, version int) *qrCodeVersion {
	for _, v := range versions {
		if v.level == level && v.version == version {
			return &v
		}
	}

	return nil
}
