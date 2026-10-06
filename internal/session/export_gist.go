package session

import (
	"fmt"
	"slices"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents/gist"
	"github.com/samsaffron/term-llm/internal/share"
)

// ShareFiles builds the canonical self-contained HTML and Markdown transcript
// pair. Images are inlined into HTML within a bounded budget; asset mode is
// ignored because the map form cannot carry binary files.
func ShareFiles(sess *Session, messages []Message, opts ExportOptions) (map[string]string, error) {
	if opts.Images == ExportImagesAssets {
		opts.Images = ExportImagesInline
	}
	files, err := ShareBundle(sess, messages, opts)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(files))
	for _, file := range files {
		result[file.Name] = string(file.Content)
	}
	return result, nil
}

// ShareBundle builds a share protocol bundle: index.html, session.md, and, in
// ExportImagesAssets mode, normalized images under assets/ referenced by both
// renderings with relative URLs.
func ShareBundle(sess *Session, messages []Message, opts ExportOptions) ([]share.File, error) {
	images := newExportImages(opts)
	html, err := exportToHTML(sess, messages, opts, images)
	if err != nil {
		return nil, fmt.Errorf("render HTML transcript: %w", err)
	}
	markdown := exportToMarkdown(sess, messages, opts, images)
	files := []share.File{
		{Name: "index.html", MediaType: "text/html; charset=utf-8", Role: "entrypoint", Content: []byte(html)},
		{Name: "session.md", MediaType: "text/markdown; charset=utf-8", Role: "transcript", Content: []byte(markdown)},
	}
	for _, asset := range images.assets {
		files = append(files, share.File{Name: asset.Name, MediaType: asset.MediaType, Role: share.RoleAsset, Content: asset.Content})
	}
	slices.SortFunc(files, func(a, b share.File) int { return strings.Compare(a.Name, b.Name) })
	return files, nil
}

// GistFiles is retained for the explicit GitHub-only export commands.
func GistFiles(sess *Session, messages []Message, opts ExportOptions) (map[string]string, error) {
	return ShareFiles(sess, messages, opts)
}

// GistPreviewURL returns the gisthost preview URL for a valid gist ID.
func GistPreviewURL(id string) string {
	return gist.PreviewURL(id)
}
