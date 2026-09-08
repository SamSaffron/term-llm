package cmd

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/passkeyauth"
	"github.com/samsaffron/term-llm/internal/userservice"
	"github.com/spf13/pflag"
)

// parseServiceLaunch uses the actual serve command flags and validators. It
// restores every flag value so install/status never change the running CLI's
// global serve configuration. There is no second flag schema to keep in sync.
func parseServiceLaunch(kind string, args []string) (userservice.Spec, error) {
	s := userservice.Spec{Version: userservice.Version, Kind: kind}
	command := serveCmd
	if kind == "hub" {
		command = serveHubCmd
	} else if kind != "web" {
		return s, fmt.Errorf("service kind must be web or hub")
	}
	fs := pflag.NewFlagSet("service launch", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	restores := []func(){}
	add := func(f *pflag.Flag) {
		if fs.Lookup(f.Name) != nil {
			return
		}
		value := f.Value.String()
		if slice, ok := f.Value.(pflag.SliceValue); ok {
			old := append([]string(nil), slice.GetSlice()...)
			restores = append(restores, func() { _ = slice.Replace(old) })
		} else {
			restores = append(restores, func() { _ = f.Value.Set(value) })
		}
		clone := *f
		clone.Changed = false
		fs.AddFlag(&clone)
		// Parse from command defaults, never values changed by a previous command.
		if slice, ok := f.Value.(pflag.SliceValue); ok {
			_ = slice.Replace(nil)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
	}
	command.Flags().VisitAll(add)
	command.InheritedFlags().VisitAll(add)
	rootCmd.PersistentFlags().VisitAll(add)
	defer func() {
		for i := len(restores) - 1; i >= 0; i-- {
			restores[i]()
		}
	}()
	if err := fs.Parse(args); err != nil {
		return s, fmt.Errorf("invalid %s launch arguments (use 'serve %s --help' for flags): %w", kind, kind, err)
	}
	if fs.NArg() != 0 {
		return s, fmt.Errorf("only additional serve %s flags may follow --", kind)
	}
	for _, name := range []string{"token", "webrtc-token", "hub-registration-token", "registration-token", "passkey-bootstrap-token-file", "passkey-recovery-token-file", "print-passkey-bootstrap-token", "setup", "help", "version", "debug-raw", "cpuprofile", "memprofile", "pprof"} {
		if fs.Changed(name) {
			return s, fmt.Errorf("--%s is not persisted in service arguments; use service credential/setup options instead", name)
		}
	}
	launch := append([]string(nil), args...)
	setDefault := func(name, value string) {
		if !fs.Changed(name) {
			_ = fs.Set(name, value)
			launch = append(launch, "--"+name, value)
		}
	}
	if !fs.Changed("auth") && !fs.Changed("no-auth") && !fs.Changed("allow-no-auth") {
		setDefault("auth", "passkey")
	}
	mode, _ := fs.GetString("auth")
	var err error
	if kind == "web" {
		none, _ := fs.GetBool("no-auth")
		mode, err = resolveServeAuthMode(fs.Changed("auth"), mode, fs.Changed("no-auth") || fs.Changed("allow-no-auth"), none)
	} else {
		mode, err = resolveHubAuthMode(mode)
	}
	if err != nil {
		return s, err
	}
	host, _ := fs.GetString("host")
	port, _ := fs.GetInt("port")
	if err = validateHubBind(host, port, mode != "none"); err != nil {
		return s, err
	}
	if !isLoopbackHost(host) {
		return s, fmt.Errorf("managed user services bind to loopback; use a reverse proxy for remote access")
	}
	base, _ := fs.GetString("base-path")
	if !fs.Changed("base-path") {
		base = "/ui"
		if kind == "hub" {
			base = "/hub"
		}
	}
	public, _ := fs.GetString("public-url")
	if public == "" {
		public = "http://" + net.JoinHostPort("localhost", strconv.Itoa(port)) + strings.TrimRight(base, "/") + "/"
	}
	if mode == "passkey" {
		endpoint, e := passkeyauth.ParseEndpoint(passkeyauth.EndpointOptions{PublicURL: public, BasePath: base, BasePathExplicit: fs.Changed("base-path")})
		if e != nil {
			return s, e
		}
		base = endpoint.BasePath
		public = endpoint.URL.String()
		if kind == "web" {
			if base == "" {
				return s, fmt.Errorf("Web passkeys require a non-root URL path such as /ui/")
			}
			rtc, _ := fs.GetBool("webrtc")
			hubURL, _ := fs.GetString("hub-url")
			register, _ := fs.GetBool("hub-register")
			connect, _ := fs.GetString("hub-connect")
			cors, _ := fs.GetStringArray("cors-origin")
			if err := validateWebPasskeyTransport(rtc, hubURL, register, connect, cors); err != nil {
				return s, err
			}
		}
	} else {
		if kind == "web" {
			base, err = normalizeBasePath(base)
		} else {
			base, err = normalizeHubBasePath(base)
		}
		if err != nil {
			return s, err
		}
		u, e := url.Parse(public)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return s, fmt.Errorf("invalid public URL")
		}
	}
	setDefault("base-path", base)
	setDefault("public-url", public)
	if kind == "web" {
		connect, _ := fs.GetString("hub-connect")
		register, _ := fs.GetBool("hub-register")
		hubURL, _ := fs.GetString("hub-url")
		nodeID, _ := fs.GetString("hub-node-id")
		s.Register = register
		connect = strings.ToLower(strings.TrimSpace(connect))
		if connect != "direct" && connect != "reverse" {
			return s, fmt.Errorf("invalid --hub-connect")
		}
		if register && connect != "reverse" {
			return s, fmt.Errorf("--hub-register requires --hub-connect reverse")
		}
		if connect == "reverse" && (hubURL == "" || nodeID == "" || mode != "bearer") {
			return s, fmt.Errorf("reverse Hub connection requires bearer auth, --hub-url and --hub-node-id")
		}
	}
	authFile, _ := fs.GetString("passkey-auth-file")
	if authFile != "" && !filepath.IsAbs(authFile) {
		return s, fmt.Errorf("--passkey-auth-file must be absolute for a service")
	}
	s.Args = launch
	s.Auth = mode
	s.URL = public
	s.Host = host
	s.Port = port
	s.BasePath = base
	s.AuthFile = authFile
	return s, nil
}
