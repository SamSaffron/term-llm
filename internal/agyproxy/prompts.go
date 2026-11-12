package agyproxy

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

const (
	promptDirectory = ".term-llm-prompts"
	maxPromptSize   = 128 << 20
)

// StagePrompt persists a UTF-8 user prompt in the private agy home and returns
// its transport envelope. The proxy removes the envelope and restores even
// truncated copies from this content-addressed store on every generation,
// including native resumes. Files must persist for the lifetime of the home.
// Prompts above 128 MiB are rejected; the forwarded JSON request has its own
// 128 MiB limit, so JSON escaping and history can impose a lower effective limit.
func StagePrompt(home, prompt string) (string, error) {
	if len(prompt) > maxPromptSize {
		return "", errors.New("stage agy prompt: prompt exceeds 128 MiB limit")
	}
	if !utf8.ValidString(prompt) {
		return "", errors.New("stage agy prompt: prompt is not valid UTF-8")
	}
	dir, err := openPromptDirectory(home, true)
	if err != nil {
		return "", fmt.Errorf("stage agy prompt: %w", err)
	}
	defer dir.Close()
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(prompt)))
	name := hash + ".txt"
	if _, err := dir.Lstat(name); err == nil {
		if _, err := readStagedPrompt(dir, hash); err != nil {
			return "", fmt.Errorf("stage agy prompt: %w", err)
		}
		return promptEnvelope(hash, prompt), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect staged agy prompt: %w", err)
	}
	if err := writeStagedPrompt(dir, name, prompt); err != nil {
		return "", fmt.Errorf("stage agy prompt: %w", err)
	}
	// Verify the published file too, including concurrent publication of the
	// same hash. Never overwrite or repair a corrupt/symlinked existing file.
	if _, err := readStagedPrompt(dir, hash); err != nil {
		return "", fmt.Errorf("verify staged agy prompt: %w", err)
	}
	return promptEnvelope(hash, prompt), nil
}

func writeStagedPrompt(dir *os.Root, name, prompt string) error {
	tmp := ".stage-" + rand.Text()
	file, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create prompt file: %w", err)
	}
	defer dir.Remove(tmp)
	_, writeErr := io.WriteString(file, prompt)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write prompt file: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close prompt file: %w", closeErr)
	}
	// Link publishes the completed file atomically without following or
	// replacing an existing destination (unlike Rename).
	if err := dir.Link(tmp, name); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("publish prompt file: %w", err)
	}
	return nil
}

func openPromptDirectory(home string, create bool) (*os.Root, error) {
	if home == "" {
		return nil, errors.New("missing private agy home for staged prompt")
	}
	info, err := os.Lstat(filepath.Clean(home))
	if err != nil {
		return nil, fmt.Errorf("inspect private agy home: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("private agy home must be a non-symlink directory")
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, fmt.Errorf("open private agy home: %w", err)
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("private agy home changed while opening")
	}
	if create {
		if err := root.Mkdir(promptDirectory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create prompt directory: %w", err)
		}
	}
	info, err = root.Lstat(promptDirectory)
	if err != nil {
		return nil, fmt.Errorf("inspect prompt directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return nil, errors.New("prompt directory must be a private 0700 non-symlink directory")
	}
	dir, err := root.OpenRoot(promptDirectory)
	if err != nil {
		return nil, fmt.Errorf("open prompt directory: %w", err)
	}
	opened, err = dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		dir.Close()
		return nil, errors.New("prompt directory changed while opening")
	}
	return dir, nil
}

func readStagedPrompt(dir *os.Root, hash string) (string, error) {
	name := hash + ".txt" // hash is validated by the envelope parser, never a path
	info, err := dir.Lstat(name)
	if err != nil {
		return "", fmt.Errorf("inspect staged prompt: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > maxPromptSize {
		return "", errors.New("staged prompt must be a private 0600 regular file at most 128 MiB")
	}
	file, err := dir.Open(name)
	if err != nil {
		return "", fmt.Errorf("open staged prompt: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("staged prompt changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxPromptSize+1))
	if err != nil {
		return "", fmt.Errorf("read staged prompt: %w", err)
	}
	if len(content) > maxPromptSize || fmt.Sprintf("%x", sha256.Sum256(content)) != hash || !utf8.Valid(content) {
		return "", errors.New("staged prompt exceeds limit or fails SHA-256/UTF-8 integrity check")
	}
	return string(content), nil
}
