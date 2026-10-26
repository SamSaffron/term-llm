package linkeddevices

import (
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

// ServerOptionsFromConfig resolves the administrator-owned settings only when
// linked devices has a broker and converts them to runtime options.
func ServerOptionsFromConfig(cfg *config.Config) ServerOptions {
	if cfg == nil {
		return ServerOptions{}
	}
	if cfg.LinkedDevicesEnabled() {
		if err := cfg.ResolveLinkedDevices(); err != nil {
			return ServerOptions{UnavailableReason: "linked devices configuration credentials could not be resolved"}
		}
	}
	servers := make([]peerbroker.ConfiguredICEServer, len(cfg.LinkedDevices.ICEServers))
	for i, server := range cfg.LinkedDevices.ICEServers {
		servers[i] = peerbroker.ConfiguredICEServer{
			URLs:             append([]string(nil), server.URLs...),
			CredentialSource: peerbroker.CredentialSource(server.CredentialSource),
			Username:         server.Username,
			Credential:       server.Credential,
		}
	}
	policy := peerbroker.NewHostICEConfig(servers)
	return ServerOptions{
		BrokerURL:           cfg.LinkedDevices.BrokerURL,
		AdmissionCredential: cfg.LinkedDevices.AdmissionCredential,
		ICEPolicy:           &policy,
	}
}
