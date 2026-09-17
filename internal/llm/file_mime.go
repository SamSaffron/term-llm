package llm

import (
	"path/filepath"
	"strings"
)

// openAIResponsesAcceptedFileMIMETypes is the exact set of MIME tokens the OpenAI
// Responses API accepts inside a data URL (file_data, image_url) for file input.
// It is transcribed verbatim (lowercased, deduped, sorted) from the "Full list of
// accepted file types" table in
// https://developers.openai.com/api/docs/guides/file-inputs
//
// The API validates the token exactly; anything outside this set is rejected with
//
//	Expected a base64-encoded data URL with a valid file MIME type ... but got
//	unsupported MIME type "<token>"
//
// so this list must never contain wildcards. A text/* entry admits any client
// label (Chrome sends text/x-ruby-script for .rb) and then hard-fails every turn
// that replays the stored upload. Unrecognized labels are repaired by
// CanonicalUploadMediaType instead.
var openAIResponsesAcceptedFileMIMETypes = []string{
	"application/csv",
	"application/graphql",
	"application/javascript",
	"application/json",
	"application/json5",
	"application/msword",
	"application/pdf",
	"application/rtf",
	"application/toml",
	"application/typescript",
	"application/vnd.apple.iwork",
	"application/vnd.apple.keynote",
	"application/vnd.apple.pages",
	"application/vnd.google-apps.document",
	"application/vnd.google-apps.presentation",
	"application/vnd.google-apps.spreadsheet",
	"application/vnd.ms-excel",
	"application/vnd.ms-powerpoint",
	"application/vnd.oasis.opendocument.text",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/x-awk",
	"application/x-bash",
	"application/x-graphql",
	"application/x-httpd-php",
	"application/x-httpd-php-source",
	"application/x-iif",
	"application/x-json5",
	"application/x-ndjson",
	"application/x-patch",
	"application/x-php",
	"application/x-powershell",
	"application/x-protobuf",
	"application/x-rust",
	"application/x-scala",
	"application/x-sql",
	"application/x-subrip",
	"application/x-terraform",
	"application/x-toml",
	"application/x-yaml",
	"application/yaml",
	"message/rfc822",
	"text/calendar",
	"text/css",
	"text/csv",
	"text/html",
	"text/javascript",
	"text/jsx",
	"text/markdown",
	"text/plain",
	"text/rtf",
	"text/srt",
	"text/tsv",
	"text/tsx",
	"text/vbscript",
	"text/vtt",
	"text/x-asm",
	"text/x-astro",
	"text/x-awk",
	"text/x-bash",
	"text/x-c",
	"text/x-c++",
	"text/x-clojure",
	"text/x-cmake",
	"text/x-csharp",
	"text/x-dart",
	"text/x-diff",
	"text/x-dockerfile",
	"text/x-ejs",
	"text/x-elixir",
	"text/x-erb",
	"text/x-erlang",
	"text/x-go",
	"text/x-golang",
	"text/x-gradle",
	"text/x-graphql",
	"text/x-groovy",
	"text/x-handlebars",
	"text/x-haskell",
	"text/x-hcl",
	"text/x-iif",
	"text/x-ini",
	"text/x-jade",
	"text/x-java",
	"text/x-jinja2",
	"text/x-julia",
	"text/x-kotlin",
	"text/x-less",
	"text/x-liquid",
	"text/x-lisp",
	"text/x-lua",
	"text/x-makefile",
	"text/x-mustache",
	"text/x-objectivec",
	"text/x-objectivec++",
	"text/x-patch",
	"text/x-perl",
	"text/x-php",
	"text/x-properties",
	"text/x-protobuf",
	"text/x-pug",
	"text/x-python",
	"text/x-r",
	"text/x-rst",
	"text/x-ruby",
	"text/x-rust",
	"text/x-sass",
	"text/x-scala",
	"text/x-script.python",
	"text/x-scss",
	"text/x-sh",
	"text/x-shellscript",
	"text/x-sql",
	"text/x-subrip",
	"text/x-swift",
	"text/x-terraform",
	"text/x-tex",
	"text/x-tmpl",
	"text/x-toml",
	"text/x-twig",
	"text/x-typescript",
	"text/x-vcard",
	"text/x-yaml",
	"text/x-zsh",
	"text/xml",
}

// openAIResponsesAcceptedFileMIMETypeSet is the lookup form of the list above.
var openAIResponsesAcceptedFileMIMETypeSet = mimeSet(openAIResponsesAcceptedFileMIMETypes)

// mimeSet indexes exact MIME tokens for membership tests.
func mimeSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

// uploadMediaTypeAliases maps MIME labels that browsers, operating systems, and
// language tooling emit onto the token OpenAI accepts for the same content. Every
// target is a member of openAIResponsesAcceptedFileMIMETypes (verified by test).
// Keys are lowercase; keep the table alphabetized.
var uploadMediaTypeAliases = map[string]string{
	// Rust: Linux mime.types says text/rust; text/x-rust is accepted.
	"application/rust": "text/x-rust",
	// SQL: Linux mime.types says application/sql and some tools say text/sql.
	"application/sql": "text/x-sql",
	// JavaScript: legacy Netscape-era label.
	"application/x-javascript": "application/javascript",
	// Perl: Linux mime.types says application/x-perl.
	"application/x-perl": "text/x-perl",
	// Python: Chrome and Python's mimetypes module disagree; only text/x-python
	// is accepted, so the other spellings normalize onto it.
	"application/x-python": "text/x-python",
	// Ruby: Chrome labels .rb text/x-ruby-script and Linux mime.types says
	// application/x-ruby. Only text/x-ruby is accepted.
	"application/x-ruby": "text/x-ruby",
	// Shell: several spellings in the wild, one accepted token.
	"application/x-sh":          "text/x-sh",
	"application/x-shellscript": "text/x-sh",
	// TOML/YAML: several spellings in the wild; the parameter-free application/*
	// tokens are the canonical ones.
	"application/x-toml": "application/toml",
	"application/x-yaml": "application/yaml",
	// XML: only text/xml is accepted (application/xml is the older spelling).
	"application/xml": "text/xml",
	// Julia: Linux mime.types says text/julia.
	"text/julia":  "text/x-julia",
	"text/python": "text/x-python",
	"text/rust":   "text/x-rust",
	"text/sql":    "text/x-sql",
	// TSV: Go's mime package reports text/tab-separated-values for .tsv while
	// the API accepts text/tsv.
	"text/tab-separated-values": "text/tsv",
	// vCard: Linux mime.types says text/vcard.
	"text/vcard": "text/x-vcard",
	// C/C++: Linux mime.types splits sources and headers into four tokens.
	"text/x-c++hdr": "text/x-c++",
	"text/x-c++src": "text/x-c++",
	"text/x-chdr":   "text/x-c",
	"text/x-csrc":   "text/x-c",
	// Go: both tokens are accepted; normalize to one so stored rows and fresh
	// uploads stay comparable.
	"text/x-go": "text/x-golang",
	// Java: Eclipse and older tooling emit text/x-java-source.
	"text/x-java-source": "text/x-java",
	// Ruby: the actual repair this table exists for. Chrome labels .rb uploads
	// text/x-ruby-script and Linux mime.types says application/x-ruby; only
	// text/x-ruby is accepted, so the rejected spelling must map onto it.
	"text/x-ruby-script": "text/x-ruby",
	// Text labels the API does accept, normalized onto their single accepted
	// spelling so stored rows and fresh uploads stay comparable.
	"text/x-script.python": "text/x-python",
	"text/x-shellscript":   "text/x-sh",
	"text/x-toml":          "application/toml",
	"text/x-yaml":          "application/yaml",
	"text/yaml":            "application/yaml",
}

// extensionCanonicalMediaTypes maps a filename extension to the token OpenAI
// accepts for that kind of source or text file. It covers the extensions term-llm
// already treats as text uploads (see isTextUploadExtension) plus the source-code
// formats listed here, so a label the API has never heard of (Chrome's
// video/mp2t for .ts, application/x-tiled-tsx for .tsx, application/x-bat for
// .bat) is replaced by the token the API actually accepts for the same file.
// Keys are lowercase with a leading dot; every target is a member of
// openAIResponsesAcceptedFileMIMETypes (verified by test).
var extensionCanonicalMediaTypes = map[string]string{
	".asm":        "text/x-asm",
	".bash":       "text/x-bash",
	".bat":        "text/plain",
	".c":          "text/x-c",
	".cc":         "text/x-c++",
	".cjs":        "application/javascript",
	".clj":        "text/x-clojure",
	".cmake":      "text/x-cmake",
	".conf":       "text/plain",
	".cpp":        "text/x-c++",
	".cs":         "text/x-csharp",
	".css":        "text/css",
	".csv":        "text/csv",
	".cxx":        "text/x-c++",
	".dart":       "text/x-dart",
	".diff":       "text/x-diff",
	".dockerfile": "text/x-dockerfile",
	".erl":        "text/x-erlang",
	".ex":         "text/x-elixir",
	".exs":        "text/x-elixir",
	".fish":       "text/x-sh",
	".go":         "text/x-golang",
	".gradle":     "text/x-gradle",
	".graphql":    "application/graphql",
	".h":          "text/x-c",
	".hh":         "text/x-c++",
	".hcl":        "text/x-hcl",
	".hpp":        "text/x-c++",
	".hs":         "text/x-haskell",
	".htm":        "text/html",
	".html":       "text/html",
	".ini":        "text/x-ini",
	".java":       "text/x-java",
	".jl":         "text/x-julia",
	".js":         "application/javascript",
	".json":       "application/json",
	".jsonl":      "application/x-ndjson",
	".jsx":        "text/jsx",
	".kt":         "text/x-kotlin",
	".less":       "text/x-less",
	".log":        "text/plain",
	".lua":        "text/x-lua",
	".markdown":   "text/markdown",
	".md":         "text/markdown",
	".mjs":        "application/javascript",
	".patch":      "text/x-patch",
	".php":        "text/x-php",
	".pl":         "text/x-perl",
	".properties": "text/x-properties",
	".proto":      "text/x-protobuf",
	".ps1":        "application/x-powershell",
	".py":         "text/x-python",
	".r":          "text/x-r",
	".rb":         "text/x-ruby",
	".rs":         "text/x-rust",
	".rst":        "text/x-rst",
	".sass":       "text/x-sass",
	".scss":       "text/x-scss",
	".sh":         "text/x-sh",
	".sql":        "text/x-sql",
	".srt":        "text/srt",
	".swift":      "text/x-swift",
	".tex":        "text/x-tex",
	".text":       "text/plain",
	".tf":         "text/x-terraform",
	".toml":       "application/toml",
	".ts":         "text/x-typescript",
	".tsv":        "text/tsv",
	".tsx":        "text/tsx",
	".txt":        "text/plain",
	".vcf":        "text/x-vcard",
	".vtt":        "text/vtt",
	".xml":        "text/xml",
	".yaml":       "application/yaml",
	".yml":        "application/yaml",
	".zsh":        "text/x-zsh",
}

// textLikeApplicationMIMETypes are the non-text/* MIME types whose bytes are
// still plain readable text. It mirrors the text and code rows of
// openAIResponsesAcceptedFileMIMETypes minus the markup/document types (rtf,
// msword, ...) that are not useful to inline. Spreadsheet types stay here because
// the Responses builder decides separately whether a spreadsheet should travel
// natively.
//
// Alias keys must not be listed: IsTextLikeMediaType resolves aliases before the
// lookup, so an alias key here would be unreachable (a guard test enforces this).
var textLikeApplicationMIMETypes = mimeSet([]string{
	"application/csv",
	"application/graphql",
	"application/javascript",
	"application/json",
	"application/json5",
	"application/toml",
	"application/typescript",
	"application/x-awk",
	"application/x-bash",
	"application/x-graphql",
	"application/x-httpd-php",
	"application/x-json5",
	"application/x-ndjson",
	"application/x-patch",
	"application/x-php",
	"application/x-powershell",
	"application/x-protobuf",
	"application/x-rust",
	"application/x-scala",
	"application/x-sql",
	"application/x-terraform",
	"application/yaml",
})

// binaryContentMediaTypes are application/* labels that name a binary or
// compressed container rather than text. Labels like these are a positive claim
// about the bytes, unlike application/octet-stream, which only means "the client
// had no idea".
var binaryContentMediaTypes = mimeSet([]string{
	"application/gzip",
	"application/vnd.rar",
	"application/wasm",
	"application/x-7z-compressed",
	"application/x-gzip",
	"application/x-rar-compressed",
	"application/x-tar",
	"application/zip",
})

// namesBinaryContent reports whether a normalized label claims the bytes are
// binary content rather than text.
func namesBinaryContent(mediaType string) bool {
	for _, prefix := range []string{"audio/", "font/", "image/", "video/"} {
		if strings.HasPrefix(mediaType, prefix) {
			return true
		}
	}
	return binaryContentMediaTypes[mediaType]
}

// CanonicalUploadMediaType maps a client- or OS-supplied label onto a token the
// provider accepts. It never overrides a label that names binary content, so it
// is safe for uploads of unknown provenance.
func CanonicalUploadMediaType(filename, mediaType string) string {
	return canonicalUploadMediaType(filename, mediaType, false)
}

// CanonicalTextUploadMediaType additionally lets the filename extension override
// a binary-sounding label. Callers must have confirmed the bytes are text:
// browsers guess from the extension and get it wrong in both directions (Chrome
// reports TypeScript source as video/mp2t because .ts collides with MPEG
// transport streams).
func CanonicalTextUploadMediaType(filename, mediaType string) string {
	return canonicalUploadMediaType(filename, mediaType, true)
}

// canonicalUploadMediaType is the shared resolution order behind the two exported
// entry points:
//
//  1. normalize the label (lowercase, drop parameters);
//  2. a known alias resolves to its canonical token;
//  3. an already-accepted token is returned unchanged;
//  4. a label naming binary content is returned unchanged unless textConfirmed,
//     in which case the filename extension wins, so Chrome's video/mp2t for a .ts
//     source file becomes the accepted typescript token while a real MPEG
//     transport stream keeps its label;
//  5. the filename extension wins over an unrecognized label;
//  6. otherwise the normalized label is returned unchanged.
//
// Step 6 keeps the function a no-op for content it has no opinion about - an
// unrecognized label that no alias, extension, or text confirmation repairs - so
// it is safe to call on every upload, images included.
func canonicalUploadMediaType(filename, mediaType string, textConfirmed bool) string {
	mediaType = NormalizeMediaType(mediaType)
	if canonical, ok := uploadMediaTypeAliases[mediaType]; ok {
		return canonical
	}
	if openAIResponsesAcceptedFileMIMETypeSet[mediaType] {
		return mediaType
	}
	if namesBinaryContent(mediaType) && !textConfirmed {
		return mediaType
	}
	if canonical, ok := extensionCanonicalMediaTypes[strings.ToLower(filepath.Ext(strings.TrimSpace(filename)))]; ok {
		return canonical
	}
	return mediaType
}

// IsTextLikeMediaType reports whether content of this type is safe and useful
// to inline into a prompt as ordinary text.
func IsTextLikeMediaType(mediaType string) bool {
	mediaType = NormalizeMediaType(mediaType)
	if mediaType == "" {
		return false
	}
	if canonical, ok := uploadMediaTypeAliases[mediaType]; ok {
		mediaType = canonical
	}
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	return textLikeApplicationMIMETypes[mediaType]
}
