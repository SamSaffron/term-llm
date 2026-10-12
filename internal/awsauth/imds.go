package awsauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func imdsProvider(o Options) CredentialsProvider {
	return retrieveFunc(func(ctx context.Context) (Credentials, error) {
		if strings.EqualFold(o.Getenv("AWS_EC2_METADATA_DISABLED"), "true") {
			return Credentials{}, errors.New("no AWS credentials found: tried static config, environment and shared profile; EC2 IMDS disabled by AWS_EC2_METADATA_DISABLED")
		}
		creds, err := retrieveIMDS(ctx, o)
		if err != nil {
			return Credentials{}, fmt.Errorf("no AWS credentials found: tried static config, environment, shared profile and EC2 IMDSv2: %w", err)
		}
		return creds, nil
	})
}

// imdsDefaultEndpoint selects the IMDS address for
// AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE (IPv4 by default), as the AWS SDK does.
func imdsDefaultEndpoint(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "ipv4":
		return "http://169.254.169.254", nil
	case "ipv6":
		return "http://[fd00:ec2::254]", nil
	default:
		return "", fmt.Errorf("invalid AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE %q (want IPv4 or IPv6)", mode)
	}
}

func retrieveIMDS(ctx context.Context, o Options) (Credentials, error) {
	// An explicit endpoint wins; the mode only selects the default address.
	endpoint := firstNonempty(o.IMDSEndpoint, o.Getenv("AWS_EC2_METADATA_SERVICE_ENDPOINT"))
	if endpoint == "" {
		defaultEndpoint, err := imdsDefaultEndpoint(o.Getenv("AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE"))
		if err != nil {
			return Credentials{}, err
		}
		endpoint = defaultEndpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Credentials{}, errors.New("invalid EC2 metadata endpoint")
	}
	tokenHeaders := http.Header{"X-Aws-Ec2-Metadata-Token-Ttl-Seconds": {"21600"}}
	token, err := metadataRequest(ctx, o.HTTPClient, time.Second, "PUT", endpoint+"/latest/api/token", tokenHeaders)
	if err != nil {
		return Credentials{}, fmt.Errorf("fetch IMDSv2 token (IMDSv1 fallback is disabled): %w", err)
	}
	if len(token) == 0 {
		return Credentials{}, errors.New("empty IMDSv2 token")
	}
	headers := http.Header{"X-Aws-Ec2-Metadata-Token": {strings.TrimSpace(string(token))}}
	roles, err := metadataRequest(ctx, o.HTTPClient, time.Second, "GET", endpoint+"/latest/meta-data/iam/security-credentials/", headers)
	if err != nil {
		return Credentials{}, fmt.Errorf("fetch EC2 role: %w", err)
	}
	firstRole, _, _ := strings.Cut(string(roles), "\n")
	role := strings.TrimSpace(firstRole)
	if role == "" {
		return Credentials{}, errors.New("EC2 metadata returned no IAM role")
	}
	body, err := metadataRequest(ctx, o.HTTPClient, time.Second, "GET", endpoint+"/latest/meta-data/iam/security-credentials/"+url.PathEscape(role), headers)
	if err != nil {
		return Credentials{}, fmt.Errorf("fetch EC2 role credentials: %w", err)
	}
	var result credentialJSON
	if json.Unmarshal(body, &result) != nil {
		return Credentials{}, errors.New("ec2 imds: invalid credential JSON")
	}
	if result.Code != "Success" {
		return Credentials{}, errors.New("ec2 imds: credential response Code is not Success")
	}
	return result.credentials("ec2 imds", true)
}
