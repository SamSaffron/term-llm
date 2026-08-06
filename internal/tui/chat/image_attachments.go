package chat

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

const maxPastedImageSize = 20 * 1024 * 1024

var readClipboardImage = clipboard.ReadImage

// ImageAttachment represents an image attached to the next outgoing user message.
type ImageAttachment struct {
	MediaType string
	Data      []byte
	Path      string
}

func (m *Model) maybeAttachImageFromPaste(msg tea.KeyPressMsg) bool {
	if !isImagePasteAttempt(msg) {
		return false
	}
	return m.maybeAttachImageFromClipboard()
}

func (m *Model) maybeAttachImageFromClipboard() bool {
	imgData, err := readClipboardImage()
	if err != nil || len(imgData) == 0 {
		return false
	}

	if len(imgData) > maxPastedImageSize {
		return true
	}

	mediaType := detectImageMediaType(imgData)
	if !strings.HasPrefix(mediaType, "image/") {
		return false
	}

	m.images = append(m.images, ImageAttachment{
		MediaType: mediaType,
		Data:      imgData,
	})
	m.selectedImage = -1
	return true
}

func isImagePasteAttempt(msg tea.KeyPressMsg) bool {
	switch strings.ToLower(msg.String()) {
	case "ctrl+v", "ctrl+shift+v", "shift+insert", "super+v", "cmd+v":
		return true
	default:
		return false
	}
}

func (m *Model) selectFirstImageAttachment() bool {
	if len(m.images) == 0 || m.selectedImage >= 0 {
		return false
	}
	m.selectedImage = 0
	m.selectedInterjection = -1
	return true
}

func (m *Model) handleImageAttachmentKeys(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if len(m.images) == 0 {
		return false, nil
	}
	if m.selectedImage < 0 {
		if m.textarea.Value() == "" && msg.String() == "up" {
			return m.selectFirstImageAttachment(), nil
		}
		return false, nil
	}

	switch msg.String() {
	case "up":
		if m.selectedImage > 0 {
			m.selectedImage--
			return true, nil
		}
		// The first chip sits between the composer and prompt history.
		return m.handlePromptHistoryKey(msg)
	case "down":
		if m.selectedImage < len(m.images)-1 {
			m.selectedImage++
		} else {
			m.selectedImage = -1
		}
		return true, nil
	case "left":
		if m.selectedImage > 0 {
			m.selectedImage--
		}
		return true, nil
	case "right":
		if m.selectedImage < len(m.images)-1 {
			m.selectedImage++
		}
		return true, nil
	case "delete", "backspace":
		idx := m.selectedImage
		m.images = append(m.images[:idx], m.images[idx+1:]...)
		if len(m.images) == 0 {
			m.selectedImage = -1
		} else if idx >= len(m.images) {
			m.selectedImage = len(m.images) - 1
		}
		return true, nil
	case "esc":
		m.selectedImage = -1
		return true, nil
	default:
		// Any other key returns focus to the composer before it is handled.
		m.selectedImage = -1
		return false, nil
	}
}

func (m *Model) imagePartList() []llm.Part {
	if len(m.images) == 0 {
		return nil
	}
	indirectVision := m.engine != nil && m.engine.IndirectVision()
	parts := make([]llm.Part, 0, len(m.images))
	for i := range m.images {
		img := &m.images[i]
		if indirectVision && img.Path == "" {
			img.Path = saveChatImageAttachment(img.Data, img.MediaType)
		}
		part := llm.Part{
			Type:      llm.PartImage,
			ImageData: &llm.ToolImageData{MediaType: img.MediaType, Base64: base64.StdEncoding.EncodeToString(img.Data)},
		}
		if indirectVision {
			part.ImagePath = img.Path
		}
		parts = append(parts, part)
	}
	return parts
}

func (m *Model) imageAttachmentLabels() []string {
	labels := make([]string, 0, len(m.images))
	for i := range m.images {
		labels = append(labels, fmt.Sprintf("image %d", i+1))
	}
	return labels
}

func saveChatImageAttachment(data []byte, mediaType string) string {
	dataDir, err := session.GetDataDir()
	if err != nil {
		return ""
	}
	uploadsDir := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0o700); err != nil {
		return ""
	}
	ext := imageExtensionForMediaType(mediaType)
	f, err := os.CreateTemp(uploadsDir, "pasted_image_*"+ext)
	if err != nil {
		return ""
	}
	path := f.Name()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		_ = os.Remove(path)
		return ""
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(path)
		return ""
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return ""
	}
	return path
}

func imageExtensionForMediaType(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func detectImageMediaType(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	mediaType := http.DetectContentType(data)
	if mediaType == "application/octet-stream" {
		return "image/png"
	}
	return mediaType
}
