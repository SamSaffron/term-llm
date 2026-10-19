package session

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// rewriteExportMediaText is used only for Markdown output. Parsed code ranges
// protect literal examples, while HTML presentation uses the AST transformer.
func rewriteExportMediaText(value string, replace func(string) string) string {
	if !strings.Contains(value, "term-llm-media://") {
		return value
	}
	source := []byte(value)
	doc := goldmark.New().Parser().Parse(text.NewReader(source))
	var protected []text.Segment
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			for i := 0; i < node.Lines().Len(); i++ {
				protected = append(protected, node.Lines().At(i))
			}
		case *ast.CodeSpan:
			for child := node.FirstChild(); child != nil; child = child.NextSibling() {
				if part, ok := child.(*ast.Text); ok {
					protected = append(protected, part.Segment)
				}
			}
		}
		return ast.WalkContinue, nil
	})
	var out strings.Builder
	last := 0
	for _, match := range htmlMediaReferencePattern.FindAllStringIndex(value, -1) {
		literal := false
		for _, segment := range protected {
			if match[0] < segment.Stop && match[1] > segment.Start {
				literal = true
				break
			}
		}
		out.WriteString(value[last:match[0]])
		token := value[match[0]:match[1]]
		if literal {
			out.WriteString(token)
		} else {
			out.WriteString(replace(token))
		}
		last = match[1]
	}
	out.WriteString(value[last:])
	return out.String()
}

func replaceMarkdownAudioReferences(value string, media map[string]core.MediaArtifact) string {
	return rewriteExportMediaText(value, func(token string) string {
		match := htmlMediaReferencePattern.FindStringSubmatch(token)
		item, ok := media[strings.ToLower(match[2])]
		if !ok || !strings.HasPrefix(normalizedMediaType(item.MediaType), "audio/") {
			return token
		}
		return markdownAudioPlaceholder(item)
	})
}

func markdownAudioPlaceholder(item core.MediaArtifact) string {
	name := exportAudioName(item)
	// The placeholder is text even if a malicious filename contains Markdown.
	name = strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`, "<", "&lt;", ">", "&gt;", "*", `\*`, "_", `\_`, "`", "\\`").Replace(name)
	return "Audio: " + name + " (not included in Markdown export)"
}
