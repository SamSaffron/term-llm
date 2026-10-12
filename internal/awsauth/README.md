# AWS authentication subset

This package is standard-library-only, including its tests. It supplies the AWS
pieces needed by the Bedrock HTTP transport:

```go
func SignV4(req *http.Request, payloadSHA256Hex string, creds Credentials, service, region string, now time.Time) error
func Resolve(ctx context.Context, opts Options) (*Resolved, error)
func NewEventStreamDecoder(r io.Reader) *EventStreamDecoder
func (d *EventStreamDecoder) Next() (EventStreamMessage, error)
func (m EventStreamMessage) Header(name string) (string, bool)
```

`Resolve` reads shared configuration but does not retrieve credentials, run
commands, resolve DNS, or make HTTP requests. Call
`resolved.Credentials.Retrieve(ctx)` when credentials are needed. An empty region
is intentional; the caller supplies its own default. `Options` includes optional
OS/process/clock/HTTP seams for deterministic tests.

Credentials follow explicit static configuration, environment (unless a profile
was specified programmatically), selected profile, ECS container, then EC2 IMDSv2.
Profiles requiring assume-role, SSO, web identity, or login-session resolution use
`aws configure export-credentials --profile NAME --format process`; install AWS
CLI v2 for those flows. Environment web identity (`AWS_WEB_IDENTITY_TOKEN_FILE`)
is delegated without `--profile`. `credential_process` uses `sh -c` / `cmd.exe /C`.
Both commands inherit the process environment, get no stdin (they must be
non-interactive), and have a 60-second timeout. Failures report the exit status;
only AWS CLI stderr is included (redacted), never `credential_process` stderr. Credential
JSON must include nonempty signing keys; process output must have `Version: 1`.

Successful expiring credentials refresh five minutes before expiration.
Concurrent callers share in-flight refresh results, including failures; failures
are not retained for subsequent retrievals. Any caller can cancel its own wait.
The refresh runs detached from the caller that started it (bounded to two
minutes), so one request's cancellation never fails others.

## Oracle fixtures

The fixtures were generated with aws-sdk-go-v2 as an oracle (core v1.47.1,
config v1.33.6, eventstream v1.7.20) before term-llm dropped that dependency; the
throwaway generator was not committed.

`testdata/sigv4.json` covers 26 default SDK signer results, including the published
`get-vanilla` example, Bedrock escaped model IDs/ARNs, query normalization, headers,
host ports, and opaque URLs. `testdata/eventstream.json` describes SDK-encoded
binary frames containing all ten wire header types and corrupted/truncated frames.
`testdata/credentials.json` records 13 SDK static credential/region decisions.
Committed tests load those fixtures without importing an SDK or executing AWS CLI.

## Intentional limits and differences

- Only default non-S3 SigV4 HTTP signing is implemented, not presigning or S3's
  single-escaped URI variant. As with the SDK, signing does **not** automatically
  add `X-Amz-Content-Sha256`.
- STS/SSO/login flows are delegated to AWS CLI rather than implemented directly.
  Delegation takes priority over profile static keys when a delegated feature is
  present, as required by this package's credential-chain contract.
- IMDSv2 only: no IMDSv1 fallback or SDK retry machinery. Each IMDS request has a
  one-second deadline; container HTTP requests have a five-second deadline.
- Metadata redirects, credential-URI userinfo, and non-HTTP(S) URIs are rejected.
  Plain HTTP container hosts must resolve only to loopback or allowed ECS/EKS IPs,
  and the peer address is re-checked at connect time (no DNS-rebinding window).
  Metadata requests never use an HTTP proxy.
  Metadata/command responses are bounded to 1 MiB; command stderr to 2 KiB. Errors
  exclude stdout, response bodies, and arbitrary runner/network error strings.
- Frames enforce the historical SDK bounds of 16 MiB payload and 128 KiB headers,
  including a 16-byte framing overhead. Eventstream v1.7.20 removed these bounds;
  retaining them prevents attacker-controlled allocations.
- Unrecognized shared-file lines are ignored, as in the SDK. Keys are
  case-insensitive, matching outer quotes are removed, nested service settings
  and multiline continuations are ignored, and credentials-file properties
  override config-file properties. Region is read from the config file only.
