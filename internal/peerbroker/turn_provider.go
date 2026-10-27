package peerbroker

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

type coturnRESTProvider struct {
	config CoturnConfig
	now    func() time.Time
}

func (p coturnRESTProvider) URLs() []string {
	return append([]string(nil), p.config.URLs...)
}

func (p coturnRESTProvider) Mint(_ context.Context, subject string, urls []string) (ICECredentials, error) {
	expires := p.now().Add(p.config.TTL).UTC().Truncate(time.Second)
	parts := []string{strconv.FormatInt(expires.Unix(), 10)}
	if p.config.Username != "" {
		parts = append(parts, p.config.Username)
	}
	parts = append(parts, subject)
	username := strings.Join(parts, ":")
	mac := hmac.New(sha1.New, []byte(p.config.SharedSecret))
	_, _ = mac.Write([]byte(username))
	credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return ICECredentials{
		ICEServers: []ICEServer{{URLs: append([]string(nil), urls...), Username: username, Credential: credential}},
		ExpiresAt:  expires,
	}, nil
}
