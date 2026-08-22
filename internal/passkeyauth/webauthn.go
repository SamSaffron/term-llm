package passkeyauth

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// RelyingPartyOptions contains application policy needed to construct a
// WebAuthn relying party. Endpoint validation remains independent so callers
// can share it with cookie and request-origin handling.
type RelyingPartyOptions struct {
	Endpoint    Endpoint
	DisplayName string
}

// RelyingParty pins WebAuthn validation to the configured public origin and RP ID.
type RelyingParty struct{ webauthn *webauthn.WebAuthn }

func NewRelyingParty(opts RelyingPartyOptions) (*RelyingParty, error) {
	if opts.Endpoint.RPID == "" || opts.Endpoint.Origin == "" || strings.TrimSpace(opts.DisplayName) == "" {
		return nil, fmt.Errorf("relying-party endpoint and display name are required")
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                   opts.Endpoint.RPID,
		RPDisplayName:          strings.TrimSpace(opts.DisplayName),
		RPOrigins:              []string{opts.Endpoint.Origin},
		AttestationPreference:  protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementPreferred, RequireResidentKey: protocol.ResidentKeyNotRequired(), UserVerification: protocol.VerificationRequired},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize WebAuthn relying party: %w", err)
	}
	return &RelyingParty{webauthn: wa}, nil
}

func (r *RelyingParty) BeginRegistration(user User) (*protocol.CredentialCreation, webauthn.SessionData, error) {
	exclusions := make([]protocol.CredentialDescriptor, 0, len(user.Credentials))
	for _, credential := range user.Credentials {
		exclusions = append(exclusions, credential.WebAuthn.Descriptor())
	}
	options, session, err := r.webauthn.BeginRegistration(user, webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred), webauthn.WithExclusions(exclusions))
	if err != nil {
		return nil, webauthn.SessionData{}, err
	}
	return options, *session, nil
}

func (r *RelyingParty) FinishRegistration(user User, session webauthn.SessionData, request *http.Request) (webauthn.Credential, error) {
	credential, err := r.webauthn.FinishRegistration(user, session, request)
	if err != nil {
		return webauthn.Credential{}, err
	}
	return *credential, nil
}

func (r *RelyingParty) BeginLogin(user User) (*protocol.CredentialAssertion, webauthn.SessionData, error) {
	options, session, err := r.webauthn.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, webauthn.SessionData{}, err
	}
	return options, *session, nil
}

func (r *RelyingParty) FinishLogin(user User, session webauthn.SessionData, request *http.Request) (webauthn.Credential, error) {
	credential, err := r.webauthn.FinishLogin(user, session, request)
	if err != nil {
		return webauthn.Credential{}, err
	}
	return *credential, nil
}
