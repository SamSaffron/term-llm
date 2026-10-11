package session

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var kindExportAudio = ast.NewNodeKind("ExportAudio")

type exportAudioNode struct {
	ast.BaseInline
	block htmlExportBlock
}

func (n *exportAudioNode) Kind() ast.NodeKind { return kindExportAudio }
func (n *exportAudioNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

type exportMediaTransformer struct {
	media  map[string]llm.MediaArtifact
	images *exportImages
	audio  *exportAudio
}

// Transform only parsed image nodes: fenced/inline code and raw HTML can never
// produce a player. Keep the node inline so placement in lists, tables and
// surrounding prose is preserved without invalid nested block markup.
func (t *exportMediaTransformer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	source := reader.Source()
	var nodes []*ast.Image
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if image, ok := node.(*ast.Image); ok && entering && strings.HasPrefix(string(image.Destination), "term-llm-media://") {
			nodes = append(nodes, image)
		}
		return ast.WalkContinue, nil
	})
	for _, node := range nodes {
		t.replace(node, source)
	}
}

func (t *exportMediaTransformer) replace(node *ast.Image, source []byte) {
	reference := strings.ToLower(strings.TrimPrefix(string(node.Destination), "term-llm-media://"))
	item, ok := t.media[reference]
	label := strings.Join(strings.Fields(string(node.Text(source))), " ")
	if !ok {
		placeholder := "Media unavailable in exported transcript"
		if label != "" {
			placeholder = "Media: " + label + " — unavailable in exported transcript"
		}
		node.Parent().ReplaceChild(node.Parent(), node, ast.NewString([]byte(placeholder)))
		return
	}
	kind := normalizedMediaType(item.MediaType)
	if strings.HasPrefix(kind, "audio/") {
		unwrapAudioLink(node)
		audio := buildHTMLAudio(t.audio, item, label)
		node.Parent().ReplaceChild(node.Parent(), node, &exportAudioNode{block: htmlExportBlock{Kind: "audio", Audio: &audio}})
		return
	}
	if label == "" {
		label = strings.TrimSpace(item.Name)
	}
	if isExportImageMediaType(kind) {
		resolved := t.images.resolve(exportImageSource{Path: item.Path(), MediaType: kind, Alt: label})
		if !resolved.Omitted {
			node.Destination = []byte(resolved.URL)
			if node.FirstChild() == nil && label != "" {
				node.AppendChild(node, ast.NewString([]byte(label)))
			}
			return
		}
	}
	name := "Image"
	if strings.HasPrefix(kind, "video/") {
		name = "Video"
	}
	placeholder := name + " — not embedded in exported transcript"
	if label != "" {
		placeholder = name + ": " + label + " — not embedded in exported transcript"
	}
	node.Parent().ReplaceChild(node.Parent(), node, ast.NewString([]byte(placeholder)))
}

type exportAudioRenderer struct{}

func (r *exportAudioRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindExportAudio, r.render)
}

func (r *exportAudioRenderer) render(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	block := node.(*exportAudioNode).block
	if block.Audio != nil {
		writeExportAudio(w, block.Audio)
	}
	return ast.WalkSkipChildren, nil
}

func newHTMLExportMarkdown(media map[string]llm.MediaArtifact, images *exportImages, audio *exportAudio) goldmark.Markdown {
	// The same audio renderer handles typed audio blocks and AST nodes.
	// No raw HTML is injected into Markdown and Goldmark unsafe mode stays off.
	return goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(&exportMediaTransformer{media: media, images: images, audio: audio}, 100))),
		goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(&exportAudioRenderer{}, 100))),
	)
}

// An audio card includes controls and a download anchor. Drop any wrapping
// Markdown link while retaining its children, rather than nesting interactive
// elements (and anchors) inside another anchor.
func unwrapAudioLink(node ast.Node) {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		link, ok := parent.(*ast.Link)
		if !ok {
			continue
		}
		container := link.Parent()
		for child := link.FirstChild(); child != nil; child = link.FirstChild() {
			container.InsertBefore(container, link, child)
		}
		container.RemoveChild(container, link)
		return
	}
}
