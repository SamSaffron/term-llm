package service

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/term-llm/term-llm/internal/userservice"
)

// ValidateAutostartArgs validates the optional service kind and required action.
func ValidateAutostartArgs(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("use autostart [web|hub] on|off|status")
	}
	if len(args) == 2 && !userservice.ValidKind(args[0]) {
		return fmt.Errorf("choose web or hub")
	}
	switch args[len(args)-1] {
	case "on", "off", "status":
		return nil
	default:
		return fmt.Errorf("choose autostart on, off or status")
	}
}

// Autostart changes only login autostart, never the running process.
func (a App) Autostart(cmd *cobra.Command, args []string) error {
	if err := ValidateAutostartArgs(args); err != nil {
		return err
	}
	e, err := newServiceEnvironment()
	if err != nil {
		return err
	}
	return autostartUserService(cmd, e, args)
}

func autostartUserService(cmd *cobra.Command, e serviceEnvironment, args []string) error {
	kind, err := e.selectKind(args[:len(args)-1])
	if err != nil {
		return err
	}
	unlock, err := e.lock(kind)
	if err != nil {
		return err
	}
	defer unlock()
	path := e.specPath(kind)
	spec, err := userservice.Load(path)
	if err != nil {
		return fmt.Errorf("service %s is not installed: %w", kind, err)
	}
	if spec.Kind != kind {
		return fmt.Errorf("service kind mismatch")
	}
	if _, err := os.Lstat(e.native.Path(kind)); err != nil {
		return fmt.Errorf("%s is not registered; run service install %s", kind, kind)
	}
	if err := e.native.CheckOwned(kind, path); err != nil {
		return err
	}
	if err := e.native.Check(cmd.Context()); err != nil {
		return err
	}
	if err := e.native.CheckLoaded(cmd.Context(), kind, path); err != nil {
		return err
	}
	action := args[len(args)-1]
	if action != "status" {
		return e.native.SetEnabled(cmd.Context(), kind, action == "on")
	}
	enabled, err := e.native.Enabled(cmd.Context(), kind)
	if err != nil {
		return err
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	fmt.Fprintln(cmd.OutOrStdout(), state)
	return nil
}

func startNativeUserService(cmd *cobra.Command, e serviceEnvironment, kind, action string) error {
	noEnable, _ := cmd.Flags().GetBool("no-enable")
	// Ordinary restart requires enabled autostart (checked before credentials),
	// but must not modify it. Only an ordinary start enables autostart.
	if noEnable || action == "restart" {
		return e.native.StartWithoutEnable(cmd.Context(), kind, action == "restart")
	}
	return e.native.Start(cmd.Context(), kind, false)
}
