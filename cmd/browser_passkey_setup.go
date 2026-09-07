package cmd

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/samsaffron/term-llm/internal/passkeyauth"
	"github.com/spf13/cobra"
)

type browserPasskeyOptions struct {
	userName, displayName string
	trustedProxies        []string
	bootstrap             func(*cobra.Command, bool) ([]byte, string, error)
	recovery              func(bool) ([]byte, error)
}

// openBrowserPasskeys centralizes private storage, locking and secret lifetimes.
func openBrowserPasskeys(cmd *cobra.Command, endpoint passkeyauth.Endpoint, authFile string, opts browserPasskeyOptions) (_ *hubPasskeyRuntime, _ string, cleanup func() error, err error) {
	unlockPasskeyState, err := lockHubPasskeyState(authFile)
	if err != nil {
		return nil, "", nil, err
	}
	closeState := unlockPasskeyState
	defer func() {
		if err != nil {
			_ = closeState()
		}
	}()
	authStore, err := passkeyauth.OpenStore(passkeyauth.StoreOptions{Path: authFile, RPID: endpoint.RPID, UserName: opts.userName, Warnf: func(format string, args ...any) { fmt.Fprintf(cmd.ErrOrStderr(), "SECURITY: "+format+"\n", args...) }})
	if err != nil {
		return nil, "", nil, err
	}
	sessionFile := filepath.Join(filepath.Dir(authFile), "sessions.json")
	sessions, err := passkeyauth.OpenSessions(passkeyauth.SessionsOptions{
		Path:            sessionFile,
		RPID:            endpoint.RPID,
		UserID:          authStore.User().ID,
		ValidCredential: authStore.HasCredential,
		Warnf:           func(format string, args ...any) { fmt.Fprintf(cmd.ErrOrStderr(), "SECURITY: "+format+"\n", args...) },
	})
	if err != nil {
		return nil, "", nil, err
	}
	closeState = func() error { return errors.Join(sessions.Close(), unlockPasskeyState()) }
	bootstrapSecret, display, err := opts.bootstrap(cmd, authStore.CredentialCount() == 0)
	if err != nil {
		return nil, "", nil, err
	}
	bootstrapDisplay := display
	bootstrapGrants, err := passkeyauth.NewGrants(passkeyauth.GrantBootstrap, bootstrapSecret, nil, nil)
	for i := range bootstrapSecret {
		bootstrapSecret[i] = 0
	}
	if err != nil {
		return nil, "", nil, err
	}
	recoverySecret, err := opts.recovery(authStore.CredentialCount() > 0)
	if err != nil {
		return nil, "", nil, err
	}
	recoveryGrants, err := passkeyauth.NewGrants(passkeyauth.GrantRecovery, recoverySecret, nil, nil)
	for i := range recoverySecret {
		recoverySecret[i] = 0
	}
	if err != nil {
		return nil, "", nil, err
	}
	peerResolver, err := newHubClientPeerResolver(opts.trustedProxies)
	if err != nil {
		return nil, "", nil, err
	}
	displayName := opts.displayName
	if displayName == "" {
		displayName = hubPasskeyRPDisplayName
	}
	runtime, err := newHubPasskeyRuntime(endpoint, authStore, sessions, bootstrapGrants, recoveryGrants, peerResolver, displayName)
	if err != nil {
		return nil, "", nil, err
	}

	return runtime, bootstrapDisplay, closeState, nil
}
