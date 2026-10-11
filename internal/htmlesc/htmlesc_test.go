package htmlesc

import (
	"html/template"
	"strings"
	"testing"
)

// html/template is the reference implementation. Using it in tests is fine:
// test binaries are never shipped, so its linker cost does not matter here.
var reference = template.Must(template.New("ref").Parse(
	`{{define "text"}}<p>{{.}}</p>{{end}}` +
		`{{define "title"}}<title>{{.}}</title>{{end}}` +
		`{{define "attr"}}<p data-x="{{.}}"></p>{{end}}` +
		`{{define "url"}}<a href="{{.}}"></a>{{end}}` +
		`{{define "frag"}}<a href="#{{.}}"></a>{{end}}`,
))

func render(t *testing.T, name string, data any) string {
	t.Helper()
	var b strings.Builder
	if err := reference.ExecuteTemplate(&b, name, data); err != nil {
		t.Fatalf("reference %s: %v", name, err)
	}
	return b.String()
}

var samples = []string{
	"",
	"plain",
	`<script>alert("x")</script>`,
	"a & b + c ' d \" e > f",
	"nul\x00byte",
	"invalid \xff\xfe utf8",
	"unicode é 中文 ﷐ ￿ emoji 🙂",
	"/chat/dist/hub.css?v=abc+def&x=1",
	"javascript:alert(1)",
	"JavaScript:alert(1)",
	"data:image/png;base64,iVBORw0KGgo+/=",
	"https://example.com/a b?q=1#frag",
	"mailto:sam@example.com",
	"relative/path:with-colon",
	"100%25 and 100% and %zz",
	"audio-1",
	"spaces and (parens) and [brackets] !$*,;=?@",
}

func TestEscapersMatchHTMLTemplate(t *testing.T) {
	for _, s := range samples {
		if got, want := "<p>"+Text(s)+"</p>", render(t, "text", s); got != want {
			t.Errorf("Text(%q) = %q, want %q", s, got, want)
		}
		if got, want := "<title>"+Text(s)+"</title>", render(t, "title", s); got != want {
			t.Errorf("Text(%q) in title = %q, want %q", s, got, want)
		}
		if got, want := `<p data-x="`+Text(s)+`"></p>`, render(t, "attr", s); got != want {
			t.Errorf("Text(%q) in attr = %q, want %q", s, got, want)
		}
		if got, want := `<a href="`+URL(s)+`"></a>`, render(t, "url", s); got != want {
			t.Errorf("URL(%q) = %q, want %q", s, got, want)
		}
		if got, want := `<a href="`+TrustedURL(s)+`"></a>`, render(t, "url", template.URL(s)); got != want { // #nosec G203 -- reference rendering in tests.
			t.Errorf("TrustedURL(%q) = %q, want %q", s, got, want)
		}
		if got, want := `<a href="#`+URLComponent(s)+`"></a>`, render(t, "frag", s); got != want {
			t.Errorf("URLComponent(%q) = %q, want %q", s, got, want)
		}
	}
}
