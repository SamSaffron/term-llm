package uploads

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/session"
)

var (
	ErrUploadNotFound         = errors.New("upload not found")
	ErrInvalidUpload          = errors.New("invalid upload")
	ErrUploadTooLarge         = errors.New("upload exceeds 20 MB limit")
	errUploadStoreUnavailable = errors.New("upload store unavailable")
)

// Reference is immutable metadata for a completed original. References belong
// to the authenticated node/store principal, not to a draft or session. There
// is deliberately no expiry or completed-file garbage collection.
type Reference struct {
	UploadID  string `json:"upload_id"`
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
}

type referenceMetadata struct {
	Reference
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
}

const maxMetadataBytes = 4096

// ValidateMetadata bounds client-controlled labels before any bytes are staged.
// Empty originals are supported, like legacy arbitrary-file attachments.
func ValidateMetadata(filename, mediaType string) error {
	if filename == "" || len(filename) > 200 || !utf8.ValidString(filename) || filename == "." || filename == ".." || strings.ContainsAny(filename, "/\\") {
		return fmt.Errorf("%w: invalid filename (maximum 200 bytes, no path separators)", ErrInvalidUpload)
	}
	for _, r := range filename {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: control character in filename", ErrInvalidUpload)
		}
	}
	if len(mediaType) > 255 || strings.ContainsAny(mediaType, "\r\n\x00") {
		return fmt.Errorf("%w: invalid content type", ErrInvalidUpload)
	}
	if _, _, err := mime.ParseMediaType(mediaType); err != nil {
		return fmt.Errorf("%w: invalid content type", ErrInvalidUpload)
	}
	return nil
}

// IsReferenceID reports whether id has the canonical opaque reference format.
func IsReferenceID(id string) bool {
	if len(id) != 47 || !strings.HasPrefix(id, "upl_") {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(id[4:])
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == id[4:]
}

// referenceRoot confines all IO to the store. Private staging and metadata are
// siblings of public downloads, never descendants. No startup sweeping occurs.
func referenceRoot(create bool) (*os.Root, error) {
	dir, err := session.GetDataDir()
	if err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	names := []string{"uploads", "upload-refs"}
	if create {
		names = append(names, "upload-staging")
	}
	for _, name := range names {
		if create {
			if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				root.Close()
				return nil, err
			}
		}
		info, err := root.Lstat(name)
		if !create && errors.Is(err, os.ErrNotExist) {
			root.Close()
			return nil, ErrUploadNotFound
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, fmt.Errorf("%w: unavailable upload directory", ErrInvalidUpload)
		}
	}
	return root, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// StoreReference streams one original to a private 0600 staging file. The
// metadata link is the publication point: readers cannot resolve a partial
// upload. Exclusive hard links cannot replace any existing original/metadata.
// A failed request removes only its own staging and unpublished original.
func StoreReference(ctx context.Context, filename, mediaType string, body io.Reader) (Reference, error) {
	if err := ValidateMetadata(filename, mediaType); err != nil {
		return Reference{}, err
	}
	root, err := referenceRoot(true)
	if err != nil {
		return Reference{}, fmt.Errorf("%w: %v", errUploadStoreUnavailable, err)
	}
	defer root.Close()
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return Reference{}, fmt.Errorf("generate upload ID: %w", err)
	}
	id := "upl_" + base64.RawURLEncoding.EncodeToString(entropy[:])
	stage := "upload-staging/" + id
	f, err := root.OpenFile(stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Reference{}, fmt.Errorf("stage upload: %w", err)
	}
	defer root.Remove(stage)
	defer f.Close()
	hash := sha256.New()
	var head prefixWriter
	size, err := io.Copy(io.MultiWriter(f, hash, &head), io.LimitReader(contextReader{ctx, body}, MaxAttachmentBytes+1))
	if err != nil {
		return Reference{}, fmt.Errorf("read upload: %w", err)
	}
	if size > MaxAttachmentBytes {
		return Reference{}, ErrUploadTooLarge
	}
	if err := f.Sync(); err != nil {
		return Reference{}, fmt.Errorf("sync upload: %w", err)
	}
	if err := f.Close(); err != nil {
		return Reference{}, fmt.Errorf("close upload: %w", err)
	}
	ref := Reference{id, filename, NormalizeMediaType(filename, mediaType, head), size}
	metadata := referenceMetadata{ref, 1, hex.EncodeToString(hash.Sum(nil))}
	if err := publishReference(ctx, root, stage, metadata); err != nil {
		return Reference{}, err
	}
	return ref, nil
}

// prefixWriter retains only enough data for MIME sniffing; originals are never
// buffered in memory on ingestion (the Hub still buffers each bounded body).
type prefixWriter []byte

func (p *prefixWriter) Write(b []byte) (int, error) {
	n := len(b)
	if remaining := 8192 - len(*p); remaining > 0 {
		*p = append(*p, b[:min(remaining, len(b))]...)
	}
	return n, nil
}

func publishReference(ctx context.Context, root *os.Root, stage string, metadata referenceMetadata) error {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	metaStage := stage + ".json"
	f, err := root.OpenFile(metaStage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("stage metadata: %w", err)
	}
	defer root.Remove(metaStage)
	defer f.Close()
	if _, err := f.Write(encoded); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync metadata: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close metadata: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	original := "uploads/" + metadata.UploadID
	if err := root.Link(stage, original); err != nil {
		return fmt.Errorf("publish original: %w", err)
	}
	if err := root.Link(metaStage, "upload-refs/"+metadata.UploadID+".json"); err != nil {
		root.Remove(original)
		return fmt.Errorf("publish metadata: %w", err)
	}
	return syncUploadDirectories(root)
}

func syncUploadDirectories(root *os.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	// Sync the publication directories for restart durability. Directory Sync is
	// unsupported on Windows, where File.Sync above still flushes file contents.
	for _, name := range []string{"uploads", "upload-refs"} {
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil && !errors.Is(err, os.ErrInvalid) {
			return fmt.Errorf("sync upload directory: %w", err)
		}
	}
	return nil
}

// readReferenceFile verifies the directory entry and the actual opened file
// identity before a bounded read. Root additionally prevents symlink escapes.
func readReferenceFile(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrUploadNotFound
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrInvalidUpload
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, ErrInvalidUpload
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return nil, ErrInvalidUpload
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) != opened.Size() || int64(len(raw)) > limit {
		return nil, ErrInvalidUpload
	}
	return raw, nil
}

// ResolveReference reopens immutable metadata and verifies the original's size
// and digest. No caller-supplied paths are accepted and no new copy is saved.
func ResolveReference(id string) (Reference, string, []byte, error) {
	if !IsReferenceID(id) {
		return Reference{}, "", nil, ErrInvalidUpload
	}
	root, err := referenceRoot(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrUploadNotFound) {
			return Reference{}, "", nil, ErrUploadNotFound
		}
		return Reference{}, "", nil, ErrInvalidUpload
	}
	defer root.Close()
	encoded, err := readReferenceFile(root, "upload-refs/"+id+".json", maxMetadataBytes)
	if err != nil {
		return Reference{}, "", nil, err
	}
	metadata, err := decodeReferenceMetadata(encoded, id)
	if err != nil {
		return Reference{}, "", nil, err
	}
	raw, err := readReferenceFile(root, "uploads/"+id, MaxAttachmentBytes)
	if err != nil {
		return Reference{}, "", nil, err
	}
	digest := sha256.Sum256(raw)
	if int64(len(raw)) != metadata.SizeBytes || hex.EncodeToString(digest[:]) != metadata.SHA256 {
		return Reference{}, "", nil, ErrInvalidUpload
	}
	return metadata.Reference, filepath.Join(root.Name(), "uploads", id), raw, nil
}

func decodeReferenceMetadata(encoded []byte, id string) (referenceMetadata, error) {
	var metadata referenceMetadata
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return metadata, ErrInvalidUpload
	}
	if decoder.Decode(new(any)) != io.EOF || metadata.Version != 1 || metadata.UploadID != id || metadata.SizeBytes < 0 || metadata.SizeBytes > MaxAttachmentBytes || len(metadata.SHA256) != 64 {
		return metadata, ErrInvalidUpload
	}
	if _, err := hex.DecodeString(metadata.SHA256); err != nil {
		return metadata, ErrInvalidUpload
	}
	if err := ValidateMetadata(metadata.Filename, metadata.MediaType); err != nil {
		return metadata, ErrInvalidUpload
	}
	return metadata, nil
}

// LookupReference reads only bounded private metadata for download labels. The
// caller must separately validate/open the original before serving it; content
// digest verification is performed by ResolveReference before model admission.
func LookupReference(id string) (Reference, error) {
	if !IsReferenceID(id) {
		return Reference{}, ErrInvalidUpload
	}
	root, err := referenceRoot(false)
	if err != nil {
		return Reference{}, ErrInvalidUpload
	}
	defer root.Close()
	encoded, err := readReferenceFile(root, "upload-refs/"+id+".json", maxMetadataBytes)
	if err != nil {
		return Reference{}, err
	}
	metadata, err := decodeReferenceMetadata(encoded, id)
	return metadata.Reference, err
}
