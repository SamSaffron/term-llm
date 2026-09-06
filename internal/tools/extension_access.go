package tools

import (
	"fmt"
	"os"

	"github.com/samsaffron/term-llm/internal/config"
)

// SetExtensionDirectory installs a replaceable, runtime-only file capability.
// Only the web host calls this for the verified built-in extension-builder.
// It is not a workspace confirmation, is not persisted/inherited, and never
// authorizes shell commands or access to the main configuration file.
func (r *LocalToolRegistry) SetExtensionDirectory(dir, mainConfig string) error {
	if r == nil || r.approval == nil {
		return fmt.Errorf("extension file authorization unavailable")
	}
	m := r.approval
	m.toolAllowMu.Lock()
	m.extensionDirectory = ""
	m.toolAllowMu.Unlock()
	if dir == "" {
		return nil
	}
	canonical, err := canonicalizePathForWrite(dir)
	if err != nil {
		return err
	}
	defaultConfig, err := config.GetConfigPath()
	if err != nil {
		return err
	}
	for _, protected := range []string{mainConfig, defaultConfig} {
		if protected == "" {
			continue
		}
		p, err := canonicalizePathForWrite(protected)
		if err != nil {
			return err
		}
		if pathWithinWorkspace(p, canonical) {
			return fmt.Errorf("extension directory is too broad: it contains main configuration; choose a dedicated subdirectory")
		}
	}
	if err := os.MkdirAll(canonical, 0755); err != nil {
		return err
	}
	m.toolAllowMu.Lock()
	m.extensionDirectory = canonical
	m.toolAllowMu.Unlock()
	return nil
}
func (m *ApprovalManager) extensionFileAllowed(toolName, path string) bool {
	if m == nil {
		return false
	}
	switch toolName {
	case ReadFileToolName, WriteFileToolName, EditFileToolName, UnifiedDiffToolName, GrepToolName, GlobToolName, UIGetSourceToolName:
	default:
		return false
	}
	m.toolAllowMu.RLock()
	dir := m.extensionDirectory
	m.toolAllowMu.RUnlock()
	return dir != "" && pathWithinWorkspace(path, dir)
}
