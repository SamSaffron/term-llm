package cmd

func init() {
	f := serveCmd.Flags()
	f.StringVar(&serveFlags.PasskeyAuthFile, "passkey-auth-file", "", "Private Web passkey store (default: <data-dir>/web-auth/auth.json)")
	f.StringVar(&serveFlags.PasskeyBootstrapFile, "passkey-bootstrap-token-file", "", "Private file containing the first-passkey setup secret")
	f.StringVar(&serveFlags.PasskeyRecoveryFile, "passkey-recovery-token-file", "", "Private file containing a short-lived passkey recovery secret")
	f.BoolVar(&serveFlags.PrintPasskeyBootstrap, "print-passkey-bootstrap-token", false, "Explicitly allow printing the one-time setup code to non-interactive output")
	f.StringSliceVar(&serveFlags.PasskeyTrustedProxies, "passkey-trusted-proxy", nil, "Trusted proxy IP/CIDR for passkey rate limits (repeatable; never determines the public origin)")
}
