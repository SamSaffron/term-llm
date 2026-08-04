package mentions

import (
	"errors"
	"os"
)

var errSecureOpenUnavailable = errors.New("descriptor-relative secure open is unavailable on this platform")

type secureMentionRoot interface {
	Open(string) (*os.File, error)
	Stat(string) (os.FileInfo, error)
	Close() error
}
