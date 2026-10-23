package hubnode

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"syscall"
)

var localNetworkHintOnce sync.Once

// LogLocalNetworkHint explains the likely macOS cause of an unreachable
// local-network Hub once per process; the raw error ("no route to host") gives
// operators no indication that a privacy setting is involved.
func LogLocalNetworkHint(logf func(string, ...any), err error) {
	hint := LocalNetworkHint(runtime.GOOS, err)
	if hint == "" || logf == nil {
		return
	}
	localNetworkHintOnce.Do(func() { logf("%s", hint) })
}

// LocalNetworkHint returns recovery guidance when a macOS connection to a
// private or link-local address failed with EHOSTUNREACH, which is how Local
// Network privacy reports a blocked connection from a background service.
func LocalNetworkHint(goos string, err error) string {
	if goos != "darwin" || !errors.Is(err, syscall.EHOSTUNREACH) {
		return ""
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Addr == nil {
		return ""
	}
	var ip net.IP
	switch addr := opErr.Addr.(type) {
	case *net.TCPAddr:
		ip = addr.IP
	case *net.UDPAddr:
		ip = addr.IP
	}
	if ip == nil || !(ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return ""
	}
	prefix := "/32"
	if ip.To4() == nil {
		prefix = "/128"
	}
	return fmt.Sprintf("hub: %s is a local-network address. If the Hub is reachable from a terminal, macOS Local Network privacy is blocking this background service. "+
		"macOS authorizes each new build separately, usually within seconds while the process keeps running, and Hub connections keep retrying. "+
		"If this persists, enable Term LLM in System Settings > Privacy & Security > Local Network, or exempt the Hub with "+
		"'sudo defaults write com.apple.network.local-network AllowedWiFiLocalNetworkAddresses -array %s%s' "+
		"(AllowedEthernetLocalNetworkAddresses for wired networks) and restart the Mac.",
		ip, ip, prefix)
}
