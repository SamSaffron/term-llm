// Package authorship handles display-only proxy identity and author directory caching.
package authorship

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/term-llm/term-llm/internal/authoridentity"
	"github.com/term-llm/term-llm/internal/linkeddevices"
	"github.com/term-llm/term-llm/internal/pairing"
	"github.com/term-llm/term-llm/internal/session"
)

type Identity struct {
	Author       session.Author
	LinkedDevice bool
}
type identityContextKey struct{}

func FromContext(ctx context.Context) *Identity {
	identity, _ := ctx.Value(identityContextKey{}).(*Identity)
	return identity
}

func decodedIdentityValue(value string, byteLimit, runeLimit int) string {
	value, err := new(mime.WordDecoder).DecodeHeader(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	value = strings.TrimSpace(value)
	if !authoridentity.ValidValue(value, byteLimit, runeLimit) {
		return ""
	}
	return value
}

func Resolve(r *http.Request, header, nameHeader, emailHeader string) *Identity {
	if header == "" {
		return nil
	}
	if device, ok := linkeddevices.DeviceFromRequest(r); ok {
		if device.ApprovedBy == nil || !authoridentity.ValidValue(device.ApprovedBy.ID, 256, 0) {
			return nil
		}
		return &Identity{Author: session.Author{ID: device.ApprovedBy.ID, Name: deviceName(device.ApprovedBy), Email: device.ApprovedBy.Email}, LinkedDevice: true}
	}
	id := strings.TrimSpace(singleIdentityHeader(r.Header, header))
	if !authoridentity.ValidValue(id, 256, 0) {
		return nil
	}
	name := decodedIdentityValue(singleIdentityHeader(r.Header, nameHeader), 0, 128)
	if name == "" {
		name = id
	}
	return &Identity{Author: session.Author{ID: id, Name: name, Email: decodedIdentityValue(singleIdentityHeader(r.Header, emailHeader), 256, 0)}}
}

func Middleware(next http.HandlerFunc, header, nameHeader, emailHeader string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if identity := Resolve(r, header, nameHeader, emailHeader); identity != nil {
			r = r.WithContext(context.WithValue(r.Context(), identityContextKey{}, identity))
		}
		next(w, r)
	}
}

// CreatedByFilter accepts only the display-only personal filter, never arbitrary IDs.
func CreatedByFilter(r *http.Request) (string, error) {
	value := r.URL.Query().Get("created_by")
	if value == "" {
		return "", nil
	}
	id := ViewerID(r.Context())
	if value != "me" || id == "" {
		return "", fmt.Errorf("created_by must be me and requires an identified viewer")
	}
	return id, nil
}

func PairingApprover(ctx context.Context) *pairing.Person {
	if identity := FromContext(ctx); identity != nil {
		a := identity.Author
		return &pairing.Person{ID: a.ID, Name: a.Name, Email: a.Email}
	}
	return nil
}

func AddViewerCapability(w http.ResponseWriter, r *http.Request, payload map[string]any, stores ...session.Store) {
	if identity := FromContext(r.Context()); identity != nil {
		viewer := identity.Author
		if identity.LinkedDevice && len(stores) > 0 {
			if directory, ok := session.AsAuthorStore(stores[0]); ok {
				if authors, err := directory.GetAuthors(r.Context(), []string{viewer.ID}); err == nil {
					if stored, exists := authors[viewer.ID]; exists {
						viewer = stored
					}
				}
			}
		}
		if viewer.Name == "" {
			viewer.Name = viewer.ID
		}
		w.Header().Set("Cache-Control", "private, no-store")
		payload["viewer"] = viewer
		// Hash untrusted IDs rather than placing raw header bytes in an HTTP header.
		etag := strings.TrimSuffix(w.Header().Get("ETag"), `"`)
		if etag == "" {
			etag = `W/"capabilities`
		}
		w.Header().Set("ETag", fmt.Sprintf(`%s-viewer-%x"`, etag, sha256.Sum256([]byte(identity.Author.ID))))
	}
}

func RequireCreatedBy(w http.ResponseWriter, r *http.Request, fail func(http.ResponseWriter, int, string, string)) (string, bool) {
	id, err := CreatedByFilter(r)
	if err != nil {
		fail(w, 400, "invalid_request_error", err.Error())
	}
	if err == nil && id != "" {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	return id, err == nil
}

// Log reports configured header names without recording identity values.
func Log(w io.Writer, header, name, email string) {
	if header != "" {
		fmt.Fprintf(w, "identity: reading %s (name: %s, email: %s)\n", header, name, email)
	}
}

// Ambiguous proxy claims must never select an arbitrary first value.
func singleIdentityHeader(header http.Header, name string) string {
	values := header.Values(name)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}
func deviceName(person *pairing.Person) string {
	if person.Name != "" {
		return person.Name
	}
	return person.ID
}
