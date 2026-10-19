package hub

// Options captures the Hub invocation's flag values.
type Options struct {
	Host                  string
	Port                  int
	Config                string
	Contain               bool
	NodesFile             string
	AuthMode              string
	Token                 string
	RegistrationToken     string
	BasePath              string
	PublicURL             string
	PasskeyAuthFile       string
	BootstrapTokenFile    string
	RecoveryTokenFile     string
	PrintBootstrapToken   bool
	PasskeyTrustedProxies []string
}
