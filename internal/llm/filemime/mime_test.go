package filemime

import "testing"

func TestNormalizeAndCanonicalizeUploadMediaType(t *testing.T) {
	if got := NormalizeMediaType("IMAGE/JPG; charset=utf-8"); got != "image/jpeg" {
		t.Fatalf("NormalizeMediaType = %q, want image/jpeg", got)
	}
	if got := CanonicalUploadMediaType("notes.txt", "application/octet-stream"); got != "text/plain" {
		t.Fatalf("CanonicalUploadMediaType = %q, want text/plain", got)
	}
}
