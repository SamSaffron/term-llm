package ask

// UploadHooks are the upload and image helpers shared with the serve command.
// They stay in package cmd (serve owns them), so cmd installs them into Uploads
// before preparing ask input.
type UploadHooks struct {
	// SaveBytes stores raw bytes in the app-owned uploads directory.
	SaveBytes func(filename string, raw []byte, maxBytes int64) (string, error)
	// ResizeImage downsizes an image for LLM use, returning bytes and media type.
	ResizeImage func(data []byte, mediaType string) ([]byte, string)
	// ImageDimensions returns EXIF-aware display dimensions (0, 0 on failure).
	ImageDimensions func(raw []byte) (width, height int)
	// NormalizeMediaType returns the canonical media type for an upload.
	NormalizeMediaType func(filename, mediaType string, raw []byte) string
	// Dir returns the uploads directory, or "" when unavailable.
	Dir func() string
	// MaxImagePixels is the decode pixel limit.
	MaxImagePixels int64
}

// Uploads holds the installed helpers. It must be set by the command layer.
var Uploads UploadHooks
