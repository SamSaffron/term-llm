package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ManagedMarkerSuffix names the sidecar a desktop installer writes next to an
// executable it owns. Presence of the file is authoritative: a distributable
// that verifies its own payload, records a manifest and can roll back must not
// be replaced behind its back by `term-llm upgrade`.
const ManagedMarkerSuffix = ".managed.json"

// ManagedInstall is the public, secret-free marker format. Unknown fields are
// ignored so installers can add provenance without breaking older CLIs.
type ManagedInstall struct {
	SchemaVersion int    `json:"schema_version"`
	Owner         string `json:"owner"`
	UpdateHint    string `json:"update_hint,omitempty"`
}

// ManagedInstallFor reports whether executable is owned by an external
// installer. The executable is resolved first so invoking a managed binary
// through a user symlink cannot bypass the guard. An unreadable marker still
// blocks self-upgrade; ownership is not inferred to be absent on read errors.
func ManagedInstallFor(executable string) (ManagedInstall, bool, error) {
	resolved := executable
	if value, err := filepath.EvalSymlinks(executable); err == nil && value != "" {
		resolved = value
	}
	marker := resolved + ManagedMarkerSuffix
	data, err := os.ReadFile(marker)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ManagedInstall{}, false, nil
		}
		return ManagedInstall{}, true, err
	}
	info := ManagedInstall{}
	if err := json.Unmarshal(data, &info); err != nil {
		return ManagedInstall{}, true, err
	}
	return info, true, nil
}

// managedUpgradeRefusal converts the marker check into the user-facing upgrade
// error. It returns nil when the executable is not managed.
func managedUpgradeRefusal(executable string) error {
	info, managed, err := ManagedInstallFor(executable)
	if !managed {
		return nil
	}
	owner := strings.TrimSpace(info.Owner)
	if owner == "" {
		owner = "an external installer"
	}
	hint := strings.TrimSpace(info.UpdateHint)
	if hint != "" {
		hint = " " + hint
	}
	if err != nil {
		hint = " Update it through the installer that placed it."
	}
	return fmt.Errorf("%s is managed by %s and cannot self-upgrade.%s", executable, owner, hint)
}
