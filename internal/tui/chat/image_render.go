package chat

import (
	"fmt"
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/termimage"
	"github.com/samsaffron/term-llm/internal/ui"
)

const chatImageMaxRows = 30

func (m *Model) configureImageRenderer() {
	if m == nil || m.chatRenderer == nil {
		return
	}
	if m.altScreen {
		m.chatRenderer.SetImageRenderer(m.renderViewportImageArtifact)
		return
	}
	m.chatRenderer.SetImageRenderer(nil)
}

func (m *Model) imageArtifactRenderer() ui.ImageArtifactRenderer {
	if m == nil || !m.altScreen {
		return nil
	}
	return m.renderViewportImageArtifact
}

func (m *Model) renderViewportImageArtifact(path string) ui.ImageArtifact {
	path = strings.TrimSpace(path)
	artifact := ui.ImageArtifact{Caption: ui.ImageArtifactCaption(path)}
	if path == "" {
		return artifact
	}

	result, err := termimage.Render(termimage.Request{
		Path:               path,
		MaxCols:            m.imageMaxCols(),
		MaxRows:            m.imageMaxRows(),
		Mode:               termimage.ModeViewport,
		Protocol:           termimage.ProtocolAuto,
		Background:         m.imageBackground(),
		AllowEscapeUploads: true,
	})
	if err != nil {
		artifact.Warnings = append(artifact.Warnings, err.Error())
		return artifact
	}

	artifact.Display = result.Display
	artifact.Upload = result.Upload
	artifact.CacheKey = result.CacheKey
	artifact.Height = result.HeightCells
	artifact.Warnings = append(artifact.Warnings, result.Warnings...)

	if artifact.Upload != "" {
		key := artifact.CacheKey
		if key == "" {
			key = fmt.Sprintf("%s|%s|%dx%d", path, result.Protocol, result.WidthCells, result.HeightCells)
		}
		m.queueImageUpload(key, artifact.Upload)
	}

	return artifact
}

func (m *Model) imageMaxCols() int {
	if m == nil || m.width <= 0 {
		return termimage.DefaultMaxCols
	}
	cols := m.width - 2
	if cols < 1 {
		cols = 1
	}
	return cols
}

func (m *Model) imageMaxRows() int {
	rows := chatImageMaxRows
	if rows < 1 {
		rows = 1
	}
	return rows
}

func (m *Model) imageBackground() color.Color {
	if m == nil || m.styles == nil || m.styles.Theme() == nil {
		return nil
	}
	return m.styles.Theme().Background
}

func (m *Model) queueImageUpload(key, upload string) {
	if m == nil || key == "" || upload == "" {
		return
	}
	if m.uploadedImageKeys == nil {
		m.uploadedImageKeys = make(map[string]struct{})
	}
	if _, ok := m.uploadedImageKeys[key]; ok {
		return
	}
	m.uploadedImageKeys[key] = struct{}{}
	m.pendingImageUploads = append(m.pendingImageUploads, upload)
	m.scheduleImageUploadFlush()
}

func (m *Model) scheduleImageUploadFlush() {
	if m == nil || m.imageUploadFlushScheduled {
		return
	}
	m.imageUploadFlushScheduled = true
	// Uploads discovered while rendering View() cannot be returned as commands
	// from View(). If the real Bubble Tea program is available, poke the update
	// loop so it can emit the pending bytes with tea.Raw before/alongside the next
	// frame. Use a goroutine because Program.Send is blocking and View() runs on
	// Bubble Tea's event-loop goroutine.
	if m.program != nil {
		p := m.program
		go p.Send(imageUploadFlushMsg{})
	}
}

func (m *Model) drainPendingImageUploads() string {
	if m == nil || len(m.pendingImageUploads) == 0 {
		if m != nil {
			m.imageUploadFlushScheduled = false
		}
		return ""
	}
	uploads := strings.Join(m.pendingImageUploads, "")
	m.pendingImageUploads = nil
	m.imageUploadFlushScheduled = false
	return uploads
}

func (m *Model) drainPendingImageUploadCmd() tea.Cmd {
	uploads := m.drainPendingImageUploads()
	if uploads == "" {
		return nil
	}
	return tea.Raw(uploads)
}

func (m *Model) resetImageUploadState() {
	if m == nil {
		return
	}
	m.pendingImageUploads = nil
	m.uploadedImageKeys = make(map[string]struct{})
	m.imageUploadFlushScheduled = false
}

func (m *Model) terminalImageCleanupCmd() tea.Cmd {
	if m == nil || !m.altScreen {
		return nil
	}
	seq := termimage.CleanupSequence(termimage.DefaultEnvironment())
	if seq == "" {
		return nil
	}
	return tea.Raw(seq)
}

func (m *Model) quitCmd(cmds ...tea.Cmd) tea.Cmd {
	seq := make([]tea.Cmd, 0, len(cmds)+2)
	for _, cmd := range cmds {
		if cmd != nil {
			seq = append(seq, cmd)
		}
	}
	if cleanup := m.terminalImageCleanupCmd(); cleanup != nil {
		seq = append(seq, cleanup)
	}
	seq = append(seq, tea.Quit)
	return tea.Sequence(seq...)
}

type imageUploadFlushMsg struct{}
