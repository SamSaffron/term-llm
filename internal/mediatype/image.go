package mediatype

import (
	"bytes"
	"net/http"
	"strings"
)

func imageType(b []byte) string {
	signatures := []struct {
		magic []byte
		mime  string
	}{
		{[]byte("\x89PNG\r\n\x1a\n"), "image/png"}, {[]byte{0xff, 0xd8, 0xff}, "image/jpeg"},
		{[]byte("GIF87a"), "image/gif"}, {[]byte("GIF89a"), "image/gif"}, {[]byte("BM"), "image/bmp"},
	}
	for _, v := range signatures {
		if bytes.HasPrefix(b, v.magic) {
			return v.mime
		}
	}
	if len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return "image/webp"
	}
	mime := strings.Split(http.DetectContentType(b), ";")[0]
	if f, ok := Lookup(mime); ok && f.Kind == Image {
		return mime
	}
	return ""
}
