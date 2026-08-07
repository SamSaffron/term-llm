package clipboard

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"
)

// ReadText reads text content from the system clipboard
func ReadText() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return readTextMacOS()
	case "linux":
		return readTextLinux()
	default:
		return "", fmt.Errorf("clipboard read not supported on %s", runtime.GOOS)
	}
}

func readTextMacOS() (string, error) {
	cmd := exec.Command("pbpaste")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to read clipboard: %w", err)
	}
	return out.String(), nil
}

func readTextLinux() (string, error) {
	// Try wl-paste first (Wayland)
	if _, err := exec.LookPath("wl-paste"); err == nil {
		cmd := exec.Command("wl-paste", "--no-newline")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			return out.String(), nil
		}
	}

	// Fall back to xclip (X11)
	if _, err := exec.LookPath("xclip"); err == nil {
		cmd := exec.Command("xclip", "-selection", "clipboard", "-o")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			return out.String(), nil
		}
	}

	return "", fmt.Errorf("no clipboard utility found (install wl-paste or xclip)")
}

const primarySelectionProxyEnv = "TERM_LLM_PRIMARY_SELECTION_URL"

// ReadPrimarySelection reads from the PRIMARY selection (middle-click buffer on Linux).
// On macOS, falls back to the regular clipboard since there's no primary selection.
func ReadPrimarySelection() (string, error) {
	if proxyURL := strings.TrimSpace(os.Getenv(primarySelectionProxyEnv)); proxyURL != "" {
		if text, err := readPrimarySelectionProxy(proxyURL); err == nil {
			return text, nil
		}
	}
	switch runtime.GOOS {
	case "darwin":
		return readTextMacOS() // macOS has no primary selection concept
	case "linux":
		return readPrimaryLinux()
	default:
		return "", fmt.Errorf("primary selection not supported on %s", runtime.GOOS)
	}
}

func readPrimarySelectionProxy(proxyURL string) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(proxyURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("primary selection proxy returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func readPrimaryLinux() (string, error) {
	// Try wl-paste --primary first (Wayland)
	if _, err := exec.LookPath("wl-paste"); err == nil {
		cmd := exec.Command("wl-paste", "--primary", "--no-newline")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			return out.String(), nil
		}
	}

	// Fall back to xclip with primary selection (X11)
	if _, err := exec.LookPath("xclip"); err == nil {
		cmd := exec.Command("xclip", "-selection", "primary", "-o")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			return out.String(), nil
		}
	}

	return "", fmt.Errorf("no clipboard utility found (install wl-paste or xclip)")
}

// ReadImage reads image data from the system clipboard
// Returns the image data and an error if clipboard doesn't contain an image
func ReadImage() ([]byte, error) {
	switch runtime.GOOS {
	case "darwin":
		return readImageMacOS()
	case "linux":
		return readImageLinux()
	default:
		return nil, fmt.Errorf("clipboard read not supported on %s", runtime.GOOS)
	}
}

func readImageMacOS() ([]byte, error) {
	// Try pngpaste first (clean PNG output)
	if pngpastePath, err := exec.LookPath("pngpaste"); err == nil {
		// pngpaste outputs to file, use temp file
		tmpFile, err := os.CreateTemp("", "clipboard-*.png")
		if err != nil {
			return nil, fmt.Errorf("failed to create temp file: %w", err)
		}
		tmpPath := tmpFile.Name()
		tmpFile.Close()
		defer os.Remove(tmpPath)

		cmd := exec.Command(pngpastePath, tmpPath)
		if err := cmd.Run(); err == nil {
			data, err := os.ReadFile(tmpPath)
			if err == nil && len(data) > 0 {
				return data, nil
			}
		}
	}

	// Fallback: use osascript to write clipboard to temp file, then convert with sips
	tmpTiff, err := os.CreateTemp("", "clipboard-*.tiff")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpTiffPath := tmpTiff.Name()
	tmpTiff.Close()
	defer os.Remove(tmpTiffPath)

	// Write clipboard as TIFF (most reliable format on macOS)
	script := fmt.Sprintf(`set tiffData to (the clipboard as «class TIFF»)
set fp to open for access POSIX file "%s" with write permission
write tiffData to fp
close access fp`, tmpTiffPath)

	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("clipboard does not contain an image")
	}

	// Check if file was written
	info, err := os.Stat(tmpTiffPath)
	if err != nil || info.Size() == 0 {
		return nil, fmt.Errorf("clipboard is empty or not an image")
	}

	// Convert TIFF to PNG using sips
	tmpPngPath := tmpTiffPath + ".png"
	defer os.Remove(tmpPngPath)

	cmd = exec.Command("sips", "-s", "format", "png", tmpTiffPath, "--out", tmpPngPath)
	cmd.Stderr = nil // suppress sips output
	if err := cmd.Run(); err != nil {
		// If sips fails, try returning the TIFF directly
		return os.ReadFile(tmpTiffPath)
	}

	data, err := os.ReadFile(tmpPngPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read converted image: %w", err)
	}

	return data, nil
}

func readImageLinux() ([]byte, error) {
	// Try wl-paste first (Wayland)
	if _, err := exec.LookPath("wl-paste"); err == nil {
		// Discover available clipboard MIME types and request the best image type.
		// Many applications provide JPEG/WebP only, so hardcoding PNG misses valid images.
		listCmd := exec.Command("wl-paste", "--list-types")
		listOut, _ := listCmd.Output()
		if mime := detectPreferredImageMIME(string(listOut)); mime != "" {
			cmd := exec.Command("wl-paste", "--type", mime)
			var out bytes.Buffer
			cmd.Stdout = &out
			if err := cmd.Run(); err == nil && out.Len() > 0 {
				return out.Bytes(), nil
			}
		}
	}

	// Fall back to xclip (X11)
	if _, err := exec.LookPath("xclip"); err == nil {
		// Query supported targets and pick the best available image MIME type.
		targetsCmd := exec.Command("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
		targetsOut, _ := targetsCmd.Output()
		mime := detectPreferredImageMIME(string(targetsOut))
		if mime == "" {
			mime = "image/png"
		}
		cmd := exec.Command("xclip", "-selection", "clipboard", "-t", mime, "-o")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil && out.Len() > 0 {
			return out.Bytes(), nil
		}
	}

	return nil, fmt.Errorf("clipboard does not contain an image (or no clipboard utility found)")
}

func detectPreferredImageMIME(rawTypes string) string {
	lines := strings.Split(rawTypes, "\n")
	available := map[string]bool{}
	for _, line := range lines {
		mime := normalizeImageMIME(line)
		if mime != "" {
			available[mime] = true
		}
	}

	for _, preferred := range []string{"image/png", "image/jpeg", "image/webp", "image/gif"} {
		if available[preferred] {
			return preferred
		}
	}

	// Fallback to the first available image type in original order.
	for _, line := range lines {
		mime := normalizeImageMIME(line)
		if mime != "" {
			return mime
		}
	}
	return ""
}

func normalizeImageMIME(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || !strings.HasPrefix(s, "image/") {
		return ""
	}
	// Common alias in X11 targets lists.
	if s == "image/jpg" {
		return "image/jpeg"
	}
	// Keep MIME token only; drop any optional parameters.
	if idx := strings.IndexByte(s, ';'); idx > 0 {
		s = strings.TrimSpace(s[:idx])
	}
	// Reject malformed entries that are not simple MIME tokens.
	if strings.ContainsAny(s, " \t") || !strings.Contains(s, "/") {
		return ""
	}
	return s
}

// OSC52Sequence returns the OSC 52 escape sequence that instructs a terminal
// emulator to set the system clipboard to text. Uses the ST (ESC \) terminator.
// If running inside tmux or screen, wraps the sequence in a DCS passthrough
// so the outer terminal receives it. This framing helper does not enforce the
// 100 KiB raw UTF-8 payload cap; CopyTextOSC52 and CopyTextBestEffort do.
func OSC52Sequence(text string) string {
	payload := base64.StdEncoding.EncodeToString([]byte(text))
	osc := "\033]52;c;" + payload + "\033\\"

	if os.Getenv("TMUX") != "" {
		// tmux DCS passthrough: ESC Ptmux; <escaped-sequence> ESC backslash
		// Inner ESC chars are doubled for tmux.
		return "\033Ptmux;" + strings.ReplaceAll(osc, "\033", "\033\033") + "\033\\"
	}
	if os.Getenv("STY") != "" {
		// GNU screen DCS passthrough
		return "\033P" + osc + "\033\\"
	}
	return osc
}

// CopyTextOSC52 writes an OSC 52 escape sequence directly to /dev/tty,
// instructing the terminal emulator to set the system clipboard. This works
// over SSH because the local terminal interprets the sequence. Writing to
// /dev/tty bypasses any stdout buffering or filtering by bubbletea. Payloads
// are capped at 100 KiB of raw UTF-8 text and are rejected rather than truncated.
func CopyTextOSC52(text string) error {
	if len(text) > osc52MaxPayloadBytes {
		return errors.New(osc52PayloadTooLargeMessage)
	}
	seq := OSC52Sequence(text)

	f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open /dev/tty: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(seq); err != nil {
		return fmt.Errorf("write OSC 52: %w", err)
	}
	return nil
}

// CopyMethod identifies the clipboard backend that accepted copied text.
type CopyMethod string

const (
	CopyMethodNative CopyMethod = "native"
	CopyMethodOSC52  CopyMethod = "osc52"

	osc52MaxPayloadBytes        = 100 * 1024
	osc52PayloadTooLargeMessage = "content is too large for terminal clipboard transfer (maximum 100 KiB)"
)

type copyTextFunc func(string) error

// CopyTextBestEffort copies text through exactly one successful backend. An
// interactive SSH session with a usable controlling TTY prefers OSC 52 so the
// user's local terminal receives the text; other sessions prefer the platform
// clipboard. The alternate backend is attempted only when the preferred backend
// fails. OSC 52 is capped at 100 KiB of raw UTF-8; native clipboard delivery is
// uncapped.
func CopyTextBestEffort(text string) (CopyMethod, error) {
	return copyTextBestEffort(text, isInteractiveSSHSession(), CopyText, CopyTextOSC52)
}

func copyTextBestEffort(text string, preferOSC52 bool, native, osc52 copyTextFunc) (CopyMethod, error) {
	type backend struct {
		method CopyMethod
		copy   copyTextFunc
	}

	backends := []backend{
		{method: CopyMethodNative, copy: native},
		{method: CopyMethodOSC52, copy: osc52},
	}
	if preferOSC52 {
		backends[0], backends[1] = backends[1], backends[0]
	}

	failures := make([]error, 0, len(backends))
	for _, backend := range backends {
		label := "native clipboard"
		if backend.method == CopyMethodOSC52 {
			label = "OSC 52"
			if len(text) > osc52MaxPayloadBytes {
				failures = append(failures, fmt.Errorf("%s: %s", label, osc52PayloadTooLargeMessage))
				continue
			}
		}
		if err := backend.copy(text); err == nil {
			return backend.method, nil
		} else {
			failures = append(failures, fmt.Errorf("%s: %w", label, err))
		}
	}

	return "", errors.Join(failures...)
}

func isInteractiveSSHSession() bool {
	return isInteractiveSSHSessionWith(os.Getenv, hasUsableTTY)
}

func isInteractiveSSHSessionWith(getenv func(string) string, usableTTY func() bool) bool {
	ssh := getenv("SSH_CONNECTION") != "" || getenv("SSH_CLIENT") != "" || getenv("SSH_TTY") != ""
	termName := strings.TrimSpace(getenv("TERM"))
	return ssh && termName != "" && !strings.EqualFold(termName, "dumb") && usableTTY()
}

func hasUsableTTY() bool {
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	defer tty.Close()
	return term.IsTerminal(int(tty.Fd()))
}

// CopyText copies text to the system clipboard
func CopyText(text string) error {
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("pbcopy")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	case "linux":
		return copyTextLinux(text)
	default:
		return fmt.Errorf("clipboard not supported on %s", runtime.GOOS)
	}
}

func copyTextLinux(text string) error {
	// Try wl-copy first (Wayland)
	if _, err := exec.LookPath("wl-copy"); err == nil {
		cmd := exec.Command("wl-copy")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}

	// Fall back to xclip (X11)
	if _, err := exec.LookPath("xclip"); err == nil {
		cmd := exec.Command("xclip", "-selection", "clipboard")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}

	return fmt.Errorf("no clipboard utility found (install wl-copy or xclip)")
}

// CopyImage copies image data to the system clipboard
func CopyImage(imagePath string, imageData []byte) error {
	switch runtime.GOOS {
	case "darwin":
		return copyImageMacOS(imagePath)
	case "linux":
		return copyImageLinux(imageData)
	default:
		return fmt.Errorf("clipboard not supported on %s", runtime.GOOS)
	}
}

func copyImageMacOS(imagePath string) error {
	// Use osascript to copy image data to clipboard
	// This allows pasting the actual image in apps like Slack, etc.
	script := fmt.Sprintf(`set the clipboard to (read (POSIX file "%s") as TIFF picture)`, imagePath)
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		// Fallback: copy path as text using pbcopy
		cmd := exec.Command("pbcopy")
		cmd.Stdin = strings.NewReader(imagePath)
		return cmd.Run()
	}
	return nil
}

func copyImageLinux(imageData []byte) error {
	// Try wl-copy first (Wayland)
	if _, err := exec.LookPath("wl-copy"); err == nil {
		cmd := exec.Command("wl-copy", "--type", "image/png")
		cmd.Stdin = bytes.NewReader(imageData)
		return cmd.Run()
	}

	// Fall back to xclip (X11)
	if _, err := exec.LookPath("xclip"); err == nil {
		cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png")
		cmd.Stdin = bytes.NewReader(imageData)
		return cmd.Run()
	}

	return fmt.Errorf("no clipboard utility found (install wl-copy or xclip)")
}
