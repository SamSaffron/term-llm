package presentation

import "github.com/samsaffron/term-llm/internal/llm/core"

// ImageURLFunc materializes a base64-encoded inline image and returns a
// browser-accessible URL. It is provided by the caller (e.g. the serve layer)
// so this package stays free of HTTP concerns.
// Returns ("", false) when the image cannot be materialized or served.
type ImageURLFunc func(mediaType, base64Data string) (url string, ok bool)

// ContentPartsPayload converts ordered ToolContentParts into serve-event map
// entries. Images are resolved to browser URLs via imageURL; parts that yield
// empty content or fail URL resolution are dropped. Returns nil when every
// part is dropped or the input is empty, preserving the caller's nil-check
// idiom to suppress the JSON key entirely.
func ContentPartsPayload(parts []core.ToolContentPart, imageURL ImageURLFunc) []map[string]any {
	if len(parts) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case core.ToolContentPartText:
			if p.Text != "" {
				out = append(out, map[string]any{"type": "text", "text": p.Text})
			}
		case core.ToolContentPartImageData:
			if p.ImageData == nil {
				continue
			}
			url, ok := imageURL(p.ImageData.MediaType, p.ImageData.Base64)
			if !ok || url == "" {
				continue
			}
			entry := map[string]any{"type": "image", "url": url}
			if p.ImageData.MediaType != "" {
				entry["media_type"] = p.ImageData.MediaType
			}
			if p.ImageData.Width > 0 {
				entry["width"] = p.ImageData.Width
			}
			if p.ImageData.Height > 0 {
				entry["height"] = p.ImageData.Height
			}
			out = append(out, entry)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
