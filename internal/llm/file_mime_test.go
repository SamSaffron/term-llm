package llm

import (
	"mime"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// sortedKeys returns the keys of a MIME set in deterministic order.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestCanonicalUploadMediaType(t *testing.T) {
	for _, tc := range []struct {
		name      string
		filename  string
		mediaType string
		want      string
	}{
		// The reported bug: Chrome labels .rb uploads text/x-ruby-script, which
		// the Responses API rejects because it is not an accepted token.
		{"ruby chrome label", "common_helper.rb", "text/x-ruby-script", "text/x-ruby"},
		{"ruby linux label", "common_helper.rb", "application/x-ruby", "text/x-ruby"},
		{"ruby missing label", "common_helper.rb", "", "text/x-ruby"},
		{"ruby canonical label", "common_helper.rb", "text/x-ruby", "text/x-ruby"},
		{"ruby label with parameters", "common_helper.rb", "text/x-ruby-script; charset=utf-8", "text/x-ruby"},
		{"ruby label without filename", "", "text/x-ruby-script", "text/x-ruby"},
		{"ruby uppercase label", "common_helper.rb", "TEXT/X-RUBY-SCRIPT", "text/x-ruby"},
		{"ruby octet stream", "common_helper.rb", "application/octet-stream", "text/x-ruby"},

		// Extension repairs an unrecognized client label.
		{"tsx label", "App.tsx", "application/x-tiled-tsx", "text/tsx"},
		{"bat label", "run.bat", "application/x-bat", "text/plain"},

		// A label that names binary content is a claim about the bytes and is
		// never overridden without evidence that they are text.
		{"binary label beats markdown extension", "photo.md", "image/png", "image/png"},
		{"binary label beats markdown extension in zip", "archive.md", "application/zip", "application/zip"},
		{"binary label beats typescript extension", "app.ts", "video/mp2t", "video/mp2t"},
		{"binary label beats python extension", "tool.py", "application/gzip", "application/gzip"},
		{"audio label preserved", "clip.rb", "audio/mpeg", "audio/mpeg"},
		{"font label preserved", "text.md", "font/otf", "font/otf"},
		{"wasm preserved", "app.md", "application/wasm", "application/wasm"},

		// Aliases for the source-code labels browsers and Linux mime.types emit.
		{"python script label", "tool.py", "text/x-script.python", "text/x-python"},
		{"python alias label", "tool.py", "application/x-python", "text/x-python"},
		{"python bare text label", "tool.py", "text/python", "text/x-python"},
		{"go label", "main.go", "text/x-go", "text/x-golang"},
		{"shell label", "build.sh", "text/x-shellscript", "text/x-sh"},
		{"shell app label", "build.sh", "application/x-shellscript", "text/x-sh"},
		{"sql label", "query.sql", "application/sql", "text/x-sql"},
		{"sql text label", "query.sql", "text/sql", "text/x-sql"},
		{"rust label", "main.rs", "text/rust", "text/x-rust"},
		{"toml label", "config.toml", "text/x-toml", "application/toml"},
		{"yaml label", "config.yaml", "application/x-yaml", "application/yaml"},
		{"yaml text label", "config.yml", "text/yaml", "application/yaml"},
		{"c++ source label", "main.cpp", "text/x-c++src", "text/x-c++"},
		{"c++ header label", "util.hpp", "text/x-c++hdr", "text/x-c++"},
		{"c source label", "main.c", "text/x-csrc", "text/x-c"},
		{"c header label", "util.h", "text/x-chdr", "text/x-c"},
		{"java label", "Main.java", "text/x-java-source", "text/x-java"},
		{"javascript label", "app.js", "application/x-javascript", "application/javascript"},
		{"tsv label", "data.tsv", "text/tab-separated-values", "text/tsv"},
		{"vcard label", "contact.vcf", "text/vcard", "text/x-vcard"},
		{"julia label", "solver.jl", "text/julia", "text/x-julia"},
		{"xml alias label", "doc.xml", "application/xml", "text/xml"},
		{"toml alias label", "config.toml", "application/x-toml", "application/toml"},

		// The generic "client had no idea" label is not a binary claim, so the
		// extension is still allowed to speak for it.
		{"octet stream uses extension", "run.bat", "application/octet-stream", "text/plain"},
		{"octet stream uses source extension", "main.go", "application/octet-stream", "text/x-golang"},

		// Types term-llm has no opinion about must pass through untouched, so the
		// canonicalizer is safe to call on every upload including images.
		{"png untouched", "chart.png", "image/png", "image/png"},
		{"jpeg untouched", "photo.jpeg", "image/jpeg", "image/jpeg"},
		{"jpg alias untouched", "photo.jpg", "image/jpg", "image/jpeg"},
		{"pdf untouched", "doc.pdf", "application/pdf", "application/pdf"},
		{"zip untouched", "archive.zip", "application/zip", "application/zip"},
		{"octet stream untouched", "x.bin", "application/octet-stream", "application/octet-stream"},
		{"unknown untouched", "x.unknownext", "application/x-unknown", "application/x-unknown"},
		{"svg untouched", "icon.svg", "image/svg+xml", "image/svg+xml"},
		{"multi dot extension", "archive.tar.gz", "application/gzip", "application/gzip"},
		{"empty everything", "", "", ""},
		{"no extension", "Makefile", "", ""},

		// Accepted tokens survive round trips unchanged.
		{"accepted docx", "brief.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"accepted csv", "data.csv", "text/csv", "text/csv"},
		{"uppercase accepted", "data.csv", "APPLICATION/OCTET-STREAM", "text/csv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalUploadMediaType(tc.filename, tc.mediaType); got != tc.want {
				t.Fatalf("CanonicalUploadMediaType(%q, %q) = %q, want %q", tc.filename, tc.mediaType, got, tc.want)
			}
		})
	}
}

// TestCanonicalTextUploadMediaType covers the entry point used once the upload
// bytes are known to be text. Only here may the filename extension override a
// binary-sounding label, which is what repairs Chrome's video/mp2t for .ts source
// files without relabelling a real MPEG transport stream.
func TestCanonicalTextUploadMediaType(t *testing.T) {
	for _, tc := range []struct {
		name      string
		filename  string
		mediaType string
		want      string
	}{
		{"typescript chrome label", "app.ts", "video/mp2t", "text/x-typescript"},
		{"typescript chrome label uppercase extension", "App.TS", "video/mp2t", "text/x-typescript"},
		{"typescript chrome label with parameters", "app.ts", "video/mp2t; charset=utf-8", "text/x-typescript"},
		{"binary label with ruby extension", "helper.rb", "application/zip", "text/x-ruby"},
		{"binary label with markdown extension", "notes.md", "image/png", "text/markdown"},
		{"binary label without known extension", "song.mp3", "audio/mpeg", "audio/mpeg"},
		{"binary label without filename", "", "video/mp2t", "video/mp2t"},

		// Confirming text must not disturb the ordinary resolution order.
		{"alias", "helper.rb", "text/x-ruby-script", "text/x-ruby"},
		{"accepted token", "brief.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"unknown label no extension", "Makefile", "application/x-unknown", "application/x-unknown"},
		{"empty everything", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalTextUploadMediaType(tc.filename, tc.mediaType); got != tc.want {
				t.Fatalf("CanonicalTextUploadMediaType(%q, %q) = %q, want %q", tc.filename, tc.mediaType, got, tc.want)
			}
		})
	}
}

func TestIsTextLikeMediaType(t *testing.T) {
	for _, tc := range []struct {
		mediaType string
		want      bool
	}{
		{"text/plain", true},
		{"text/markdown", true},
		{"text/csv", true},
		{"text/x-ruby", true},
		{"text/x-hypothetical-new-language", true},
		{"text/plain; charset=utf-8", true},
		{"application/json", true},
		{"application/x-ndjson", true},
		{"application/json5", true},
		{"application/xml", true},
		{"application/yaml", true},
		{"application/toml", true},
		{"application/javascript", true},
		{"application/typescript", true},
		{"application/graphql", true},
		{"application/x-protobuf", true},
		{"application/x-terraform", true},
		{"application/csv", true},
		{"application/x-ruby", true},       // alias resolves to text/x-ruby
		{"application/x-javascript", true}, // alias resolves to application/javascript

		{"", false},
		{"application/octet-stream", false},
		{"application/pdf", false},
		{"application/zip", false},
		{"application/msword", false},
		{"application/rtf", false},
		{"application/vnd.ms-excel", false},
		{"application/vnd.google-apps.spreadsheet", false},
		{"image/png", false},
		{"image/svg+xml", false},
		{"video/mp4", false},
	} {
		if got := IsTextLikeMediaType(tc.mediaType); got != tc.want {
			t.Errorf("IsTextLikeMediaType(%q) = %v, want %v", tc.mediaType, got, tc.want)
		}
	}
}

// acceptedFileMIMEType reports whether the token would survive the Responses API
// file input validation.
func acceptedFileMIMEType(mediaType string) bool {
	return openAIResponsesAcceptedFileMIMETypeSet[mediaType]
}

func TestOpenAIResponsesAcceptedFileMIMETypesAreCanonical(t *testing.T) {
	if len(openAIResponsesAcceptedFileMIMETypes) == 0 {
		t.Fatal("accepted file MIME type list is empty")
	}
	seen := map[string]bool{}
	for _, mediaType := range openAIResponsesAcceptedFileMIMETypes {
		switch {
		case mediaType == "":
			t.Fatal("accepted file MIME type list contains an empty entry")
		case mediaType != strings.ToLower(mediaType):
			t.Errorf("accepted MIME type %q is not lowercase", mediaType)
		case strings.Contains(mediaType, "*"):
			t.Errorf("accepted MIME type %q is a wildcard; the API only matches exact tokens", mediaType)
		case !strings.Contains(mediaType, "/"):
			t.Errorf("accepted MIME type %q is not a type/subtype pair", mediaType)
		case NormalizeMediaType(mediaType) != mediaType:
			t.Errorf("accepted MIME type %q is not normalized (%q)", mediaType, NormalizeMediaType(mediaType))
		case seen[mediaType]:
			t.Errorf("accepted MIME type %q is duplicated", mediaType)
		}
		seen[mediaType] = true
	}
}

func TestUploadMediaTypeAliasesResolveToAcceptedTokens(t *testing.T) {
	if len(uploadMediaTypeAliases) == 0 {
		t.Fatal("alias table is empty")
	}
	for alias, canonical := range uploadMediaTypeAliases {
		if alias != NormalizeMediaType(alias) {
			t.Errorf("alias key %q is not a normalized MIME type", alias)
		}
		if alias == canonical {
			t.Errorf("alias %q maps to itself", alias)
		}
		if !acceptedFileMIMEType(canonical) {
			t.Errorf("alias %q targets %q, which the Responses API does not accept", alias, canonical)
		}
		if got := CanonicalUploadMediaType("", canonical); got != canonical {
			t.Errorf("alias target %q is not canonical (got %q)", canonical, got)
		}
	}
}

func TestExtensionCanonicalMediaTypesResolveToAcceptedTokens(t *testing.T) {
	if len(extensionCanonicalMediaTypes) == 0 {
		t.Fatal("extension table is empty")
	}
	for extension, canonical := range extensionCanonicalMediaTypes {
		if !strings.HasPrefix(extension, ".") || extension != strings.ToLower(extension) {
			t.Errorf("extension key %q must be a lowercase extension with a leading dot", extension)
		}
		if strings.Contains(extension, "/") {
			t.Errorf("extension key %q must not contain a path separator", extension)
		}
		if !acceptedFileMIMEType(canonical) {
			t.Errorf("extension %q targets %q, which the Responses API does not accept", extension, canonical)
		}
		if got := CanonicalUploadMediaType("upload"+extension, canonical); got != canonical {
			t.Errorf("extension target %q is not canonical (got %q)", canonical, got)
		}
	}
}

// TestCanonicalUploadMediaTypeRepairsKnownSourceLabels feeds the canonicalizer the
// labels browsers and OS MIME databases are documented to emit for source files,
// which is where the application/x-ruby style aliases come from. The labels are
// hardcoded on purpose: a live mime.TypeByExtension lookup reads /etc/mime.types
// and varies by distro and container, so it is only logged.
func TestCanonicalUploadMediaTypeRepairsKnownSourceLabels(t *testing.T) {
	for _, tc := range []struct{ extension, label string }{
		{".rb", "text/x-ruby-script"},   // Chrome
		{".rb", "application/x-ruby"},   // Linux mime.types
		{".py", "text/x-script.python"}, // Chrome
		{".py", "application/x-python"}, // macOS
		{".sh", "text/x-shellscript"},   // Linux mime.types
		{".sh", "application/x-shellscript"},
		{".go", "text/x-go"},
		{".rs", "text/rust"},
		{".sql", "application/sql"},
		{".toml", "text/x-toml"},
		{".yaml", "application/x-yaml"},
		{".ts", "video/mp2t"}, // Chrome: .ts collides with MPEG transport streams
		{".tsx", "application/x-tiled-tsx"},
		{".js", "application/x-javascript"},
		{".c", "text/x-csrc"},
		{".cpp", "text/x-c++src"},
		{".java", "text/x-java-source"},
		{".tsv", "text/tab-separated-values"},
		{".vcf", "text/vcard"},
		{".jl", "text/julia"},
		{".bat", "application/x-bat"},
		{".md", "application/octet-stream"},
		{".txt", "application/octet-stream"},
		{".rb", ""},
	} {
		filename := "upload" + tc.extension
		canonical := CanonicalTextUploadMediaType(filename, tc.label)
		if !acceptedFileMIMEType(canonical) {
			t.Errorf("%s: CanonicalTextUploadMediaType(%q, %q) = %q, which the Responses API does not accept", tc.extension, filename, tc.label, canonical)
		}
		if !IsTextLikeMediaType(canonical) {
			t.Errorf("%s: CanonicalTextUploadMediaType(%q, %q) = %q, which is not text-like", tc.extension, filename, tc.label, canonical)
		}
		// Diagnostic only: the host database is not part of the contract.
		t.Logf("%s: host mime.TypeByExtension = %q", tc.extension, NormalizeMediaType(mime.TypeByExtension(tc.extension)))
	}
}

func TestDefaultOpenAIResponsesFileUploadPolicyAcceptsOnlyExactTokens(t *testing.T) {
	policy := DefaultOpenAIResponsesFileUploadPolicy()
	if len(policy.NativeMimeTypes) == 0 {
		t.Fatal("default native MIME types are empty")
	}
	for _, mediaType := range policy.NativeMimeTypes {
		if strings.Contains(mediaType, "*") {
			t.Errorf("default native MIME type %q is a wildcard; the API only matches exact tokens", mediaType)
		}
		if !acceptedFileMIMEType(mediaType) {
			t.Errorf("default native MIME type %q is not in the accepted list", mediaType)
		}
	}
	if got := CanonicalUploadMediaType("common_helper.rb", "text/x-ruby-script"); !policy.AllowsNative(got, 1024) {
		t.Error("default native policy should allow the canonical .rb token")
	}
	if policy.AllowsNative("text/x-ruby-script", 1024) {
		t.Error("default native policy must not allow an unrecognized text label")
	}
}

// TestPortableTextEmbedTypesAreDerivedFromTheTextLikeSet pins the derivation: the
// portable allowlist is exactly the text/* wildcard plus the sorted text-like
// application types, so it can never be narrower than the canonicalizer's idea of
// text (it used to be a hand copy kept in sync by a guard test).
func TestPortableTextEmbedTypesAreDerivedFromTheTextLikeSet(t *testing.T) {
	portable := DefaultPortableTextFileUploadPolicy()
	if len(portable.TextEmbedMimeTypes) == 0 || portable.TextEmbedMimeTypes[0] != "text/*" {
		t.Fatalf("portable text embed types = %v, want text/* first", portable.TextEmbedMimeTypes)
	}
	want := append([]string{"text/*"}, sortedKeys(textLikeApplicationMIMETypes)...)
	if !slices.Equal(portable.TextEmbedMimeTypes, want) {
		t.Fatalf("portable text embed types = %v, want %v", portable.TextEmbedMimeTypes, want)
	}
	for mediaType := range textLikeApplicationMIMETypes {
		if !portable.AllowsTextEmbed(mediaType, 1024) {
			t.Errorf("portable policy does not embed %q", mediaType)
		}
	}
	// Alias keys resolve before the lookup, so the portable list must not carry
	// them as unreachable entries.
	for alias := range uploadMediaTypeAliases {
		if allowed := mimeSet(portable.TextEmbedMimeTypes)[alias]; allowed {
			t.Errorf("portable text embed types contain the alias key %q", alias)
		}
	}
}

// TestTextLikeMIMETypesExcludeAliasKeys keeps textLikeApplicationMIMETypes
// reachable: IsTextLikeMediaType resolves aliases first, so an alias key listed
// there would never be consulted.
func TestTextLikeMIMETypesExcludeAliasKeys(t *testing.T) {
	for mediaType := range textLikeApplicationMIMETypes {
		if canonical, ok := uploadMediaTypeAliases[mediaType]; ok {
			t.Errorf("text-like set contains the alias key %q, which resolves to %q before the lookup", mediaType, canonical)
		}
	}
}

// TestOpenAIResponsesAcceptedFileMIMETypesMatchVendorList compares the Go list
// against the pinned transcription of the vendor docs in testdata, so refreshing
// the vendor list is "update the txt, run the test, read the diff".
func TestOpenAIResponsesAcceptedFileMIMETypesMatchVendorList(t *testing.T) {
	path := filepath.Join("testdata", "openai_accepted_file_mime_types.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read vendor list: %v", err)
	}
	var vendor []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		vendor = append(vendor, line)
	}
	if len(vendor) == 0 {
		t.Fatalf("%s contains no MIME types", path)
	}
	if !slices.IsSorted(vendor) {
		t.Error("vendor list is not sorted")
	}
	for _, mediaType := range vendor {
		if accepted := openAIResponsesAcceptedFileMIMETypeSet[mediaType]; !accepted {
			t.Errorf("vendor list entry %q is missing from openAIResponsesAcceptedFileMIMETypes", mediaType)
		}
	}
	for _, mediaType := range openAIResponsesAcceptedFileMIMETypes {
		if !slices.Contains(vendor, mediaType) {
			t.Errorf("openAIResponsesAcceptedFileMIMETypes entry %q is missing from %s", mediaType, path)
		}
	}
}
