package llm

import (
	"bufio"
	"io"

	"github.com/samsaffron/term-llm/internal/llm/core/stream"
)

type sseDecoderOptions = stream.DecoderOptions

type sseDecoder = stream.Decoder

var (
	sseEventField = stream.EventField
	sseDataField  = stream.DataField
	sseDoneData   = stream.DoneData
)

func newSSEDecoder(r io.Reader, opts sseDecoderOptions) *sseDecoder {
	return stream.NewDecoder(r, opts)
}

func readSSELine(reader *bufio.Reader) (string, bool, error) { return stream.ReadLine(reader) }

func readSSELineBytes(reader *bufio.Reader) ([]byte, bool, error) {
	return stream.ReadLineBytes(reader)
}

func readSSEEvent(reader *bufio.Reader) (string, string, bool, error) {
	return stream.ReadEvent(reader)
}

func readSSEEventBytes(reader *bufio.Reader) (string, []byte, bool, error) {
	return stream.ReadEventBytes(reader)
}
