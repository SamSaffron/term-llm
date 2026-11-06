package userservice

import "context"

// SetEnabled changes login autostart only; it never starts or stops a process.
// Callers must check the managed specification and native ownership first.
func (n Native) SetEnabled(ctx context.Context, kind string, enabled bool) error {
	action := "disable"
	if enabled {
		action = "enable"
	}
	if n.OS == "linux" {
		target := n.Label(kind)
		if enabled {
			target = n.Path(kind)
		}
		_, err := n.run(ctx, "systemctl", "--user", action, target)
		return err
	}
	_, err := n.run(ctx, "launchctl", action, n.domain()+"/"+n.Label(kind))
	return err
}

// StartWithoutEnable starts or restarts regardless of the autostart setting,
// preserving that setting. It does not replace the native definition.
func (n Native) StartWithoutEnable(ctx context.Context, kind string, restart bool) error {
	return n.start(ctx, kind, restart, false, false)
}

// StopWithoutDisable stops the current process without changing login autostart.
func (n Native) StopWithoutDisable(ctx context.Context, kind string) error {
	if n.OS == "linux" {
		_, err := n.run(ctx, "systemctl", "--user", "stop", n.Label(kind))
		return err
	}
	target := n.domain() + "/" + n.Label(kind)
	if _, err := n.run(ctx, "launchctl", "print", target); err != nil {
		return nil
	}
	_, err := n.run(ctx, "launchctl", "bootout", target)
	return err
}
