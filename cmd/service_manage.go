package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/samsaffron/term-llm/internal/userservice"
	"github.com/spf13/cobra"
)

func manageUserService(cmd *cobra.Command, action string, args []string) error {
	e, err := newServiceEnvironment()
	if err != nil {
		return err
	}
	if action == "status" && len(args) == 0 {
		for _, kind := range []string{"web", "hub"} {
			if err = showUserService(cmd, e, kind); err != nil {
				return err
			}
		}
		return nil
	}
	kind, err := e.selectKind(args)
	if err != nil {
		return err
	}
	if action == "status" {
		return showUserService(cmd, e, kind)
	}
	path := e.specPath(kind)
	spec, err := userservice.Load(path)
	if err != nil {
		return fmt.Errorf("service %s is not installed: %w", kind, err)
	}
	if err = e.native.CheckOwned(kind, path); err != nil {
		return err
	}
	if action == "open" {
		if spec.Auth == "passkey" {
			count, err := serviceCredentialCount(spec)
			if err != nil {
				return err
			}
			if count == 0 {
				return fmt.Errorf("enrollment is pending; run term-llm service setup %s", kind)
			}
		}
		return openBrowser(spec.URL)
	}
	if action == "token" {
		if spec.Auth != "bearer" {
			return fmt.Errorf("%s uses %s authentication; no managed browser bearer token", kind, spec.Auth)
		}
		token, err := e.credentials(kind).Get(cmd.Context(), serviceTokenName(kind))
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), token)
		return nil
	}
	if _, err := os.Stat(e.native.Path(kind)); err != nil {
		return fmt.Errorf("%s is not registered; run service install %s", kind, kind)
	}
	unlock, err := e.lock(kind)
	if err != nil {
		return err
	}
	defer unlock()
	if err = e.native.Check(cmd.Context()); err != nil {
		return err
	}
	if err = e.native.CheckLoaded(cmd.Context(), kind, path); err != nil {
		return err
	}
	switch action {
	case "stop":
		err = e.native.Stop(cmd.Context(), kind)
	case "uninstall":
		err = e.native.Remove(cmd.Context(), kind, path)
		if err == nil {
			if removeErr := os.Remove(filepath.Join(e.dir(kind), "enrollment.json")); removeErr != nil && !os.IsNotExist(removeErr) {
				return removeErr
			}
		}
		if err == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s registration. Preserved configuration, conversations, passkeys and service credentials at %s.\n", kind, e.dir(kind))
		}
	case "start", "restart":
		if action == "restart" {
			enabled, err := e.native.Enabled(cmd.Context(), kind)
			if err != nil {
				return err
			}
			if !enabled {
				return fmt.Errorf("%s autostart is disabled; use service start %s", kind, kind)
			}
		}
		if _, err = e.credentials(kind).Load(cmd.Context(), spec.Secrets); err != nil {
			return err
		}
		if spec.Auth == "passkey" {
			count, err := serviceCredentialCount(spec)
			if err != nil {
				return err
			}
			if count == 0 {
				if _, err = os.Stat(filepath.Join(e.dir(kind), "enrollment.json")); err != nil {
					return fmt.Errorf("first-passkey enrollment is pending; run service setup %s", kind)
				}
			}
		}
		if action == "restart" {
			fmt.Fprintln(cmd.OutOrStdout(), "Restarting interrupts active work.")
		}
		if !e.native.Running(cmd.Context(), kind) {
			if err := checkUserServicePort(spec); err != nil {
				return err
			}
		}
		err = e.native.Start(cmd.Context(), kind, action == "restart")
		if err == nil {
			err = waitUserService(cmd.Context(), spec, e.native)
		}
	case "setup", "recover":
		if spec.Auth != "passkey" {
			return fmt.Errorf("%s uses %s authentication", kind, spec.Auth)
		}
		count, err := serviceCredentialCount(spec)
		if err != nil {
			return err
		}
		if action == "setup" && count > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "Passkeys are already enrolled. Sign in at %s\n", spec.URL)
			return nil
		}
		if action == "recover" && count == 0 {
			return fmt.Errorf("no passkeys are enrolled; use service setup %s", kind)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Restarting to load a fresh enrollment capability; active work will be interrupted.")
		if !e.native.Running(cmd.Context(), kind) {
			if err := checkUserServicePort(spec); err != nil {
				return err
			}
		}
		code, err := e.newEnrollment(spec, action == "recover", count)
		if err != nil {
			return err
		}
		if err = e.native.Start(cmd.Context(), kind, true); err != nil {
			return err
		}
		if err = waitUserService(cmd.Context(), spec, e.native); err != nil {
			return err
		}
		printServiceEnrollment(cmd, spec, code, action == "recover")
	default:
		return fmt.Errorf("unknown service operation")
	}
	return err
}

func showUserService(cmd *cobra.Command, e serviceEnvironment, kind string) error {
	spec, err := userservice.Load(e.specPath(kind))
	if os.IsNotExist(err) {
		if _, nativeErr := os.Lstat(e.native.Path(kind)); nativeErr == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%s: existing unmanaged native service (left untouched)\n", kind)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "%s: not installed\n", kind)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = os.Stat(e.native.Path(kind)); os.IsNotExist(err) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: not registered (saved setup retained)\n", kind)
		return nil
	}
	if err = e.native.CheckOwned(kind, e.specPath(kind)); err != nil {
		return err
	}
	status, err := e.native.Status(cmd.Context(), kind)
	fmt.Fprintf(cmd.OutOrStdout(), "%s — %s\n  auth: %s\n  specification: %s\n", kind, spec.URL, spec.Auth, e.specPath(kind))
	if err != nil {
		fmt.Fprintln(cmd.OutOrStdout(), "  process: not loaded (or user service manager unavailable)")
	} else if e.native.OS == "linux" {
		fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(status))
	} else {
		for _, line := range strings.Split(status, "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "state =") || strings.HasPrefix(trim, "pid =") || strings.HasPrefix(trim, "last exit code =") {
				fmt.Fprintln(cmd.OutOrStdout(), "  "+trim)
			}
		}
		enabled, err := e.native.Enabled(cmd.Context(), kind)
		if err == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "  autostart: %v\n", enabled)
		}
	}
	if spec.Auth == "passkey" {
		count, err := serviceCredentialCount(spec)
		if err != nil {
			return err
		}
		if count == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "  enrollment: pending — term-llm service setup %s\n", kind)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "  enrolled passkeys: %d\n", count)
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "  logs: term-llm service logs %s\n", kind)
	return nil
}

func runUserService(cmd *cobra.Command, kind, path string) error {
	if os.Getuid() == 0 {
		return fmt.Errorf("managed services must run as a normal user, not root")
	}
	if !userservice.ValidKind(kind) || !filepath.IsAbs(path) {
		return fmt.Errorf("service runner requires web/hub and an absolute specification path")
	}
	spec, err := userservice.Load(path)
	if err != nil {
		return err
	}
	if spec.Kind != kind {
		return fmt.Errorf("service kind mismatch")
	}
	parsed, err := parseServiceLaunch(kind, spec.Args)
	if err != nil {
		return err
	}
	if parsed.Auth != spec.Auth || parsed.Host != spec.Host || parsed.Port != spec.Port || parsed.BasePath != spec.BasePath || parsed.URL != spec.URL || parsed.AuthFile != spec.AuthFile {
		return fmt.Errorf("service launch metadata does not match arguments; rerun service install")
	}
	// Keep the same PID: launchd/systemd supervise serve itself, not a new watchdog.
	credentials := userservice.Credentials{OS: runtime.GOOS, Dir: filepath.Dir(path), Kind: kind}
	secrets, err := credentials.Load(cmd.Context(), spec.Secrets)
	if err != nil {
		return err
	}
	if kind == "web" && spec.Auth == "passkey" && secrets[serviceTokenName(kind)] != "" {
		return fmt.Errorf("Web passkeys must not have a bearer credential")
	}
	if spec.Auth == "bearer" && secrets[serviceTokenName(kind)] == "" {
		return fmt.Errorf("managed bearer service requires a stable credential; reinstall")
	}
	if spec.Auth == "passkey" {
		count, err := serviceCredentialCount(spec)
		if err != nil {
			return err
		}
		grantPath := filepath.Join(filepath.Dir(path), "enrollment.json")
		data, readErr := userservice.ReadPrivate(grantPath)
		if readErr == nil {
			var enrollment serviceEnrollment
			if err = json.Unmarshal(data, &enrollment); err != nil {
				return fmt.Errorf("invalid service enrollment file")
			}
			// A consumed setup/recovery capability must not be renewed by a restart.
			consumed := (!enrollment.Recovery && count > 0) || (enrollment.Recovery && count > enrollment.CredentialCount)
			if consumed {
				if err = os.Remove(grantPath); err != nil {
					return err
				}
			} else {
				prefix := "TERM_LLM_SERVE_"
				if kind == "hub" {
					prefix = "TERM_LLM_HUB_"
				}
				suffix := "BOOTSTRAP_TOKEN"
				if enrollment.Recovery {
					suffix = "RECOVERY_TOKEN"
				}
				secrets[prefix+suffix] = enrollment.Secret
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		} else if count == 0 {
			return fmt.Errorf("first-passkey setup is pending; run term-llm service setup %s", kind)
		}
	}
	if err = os.Chdir(spec.Directory); err != nil {
		return err
	}
	args := append([]string{spec.Binary, "serve", kind}, spec.Args...)
	return replaceProcess(spec.Binary, args, userservice.RunnerEnvironment(spec, os.Environ(), secrets))
}
