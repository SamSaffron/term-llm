# Serve application code

- `options.go`: typed serve options passed from Cobra flag binding into startup and runtime helpers.
- `protocol/`: shared-shell marker parsing, payload construction, and text sanitizing; no Cobra or HTTP dependencies.
- `hub/`: Hub base-path mounting, trusted-proxy auth/rate limiting, cache identity, health, and delegation helpers.
- `shell/`: per-session PTY shell management, collaborative controller, and activity tracking.
- `jobs/`: jobs-v2 SQLite schema/migrations, run indexes, and cron math.
- `server/`: the reverse-connected Hub node's HTTP request relay, wire frames, and path guards. `cmd/serve_hub_reverse_connector.go` still owns websocket lifecycle and delegates each request to this package.
- `cmd/serve.go`, `cmd/serve_handlers*.go`: serve startup, `serveServer`, and HTTP route/handler composition. The original-code route and immediate auth-wrapper snapshot is `cmd/testdata/routes.golden`, checked by `TestServeRoutesGolden`.
- `cmd/serve_runtime*.go`, `cmd/serve_response*.go`, `cmd/serve_session*.go`: runtime, session, and response-run management (still in cmd; shared cmd runner dependencies prevent an isolated move).
- `cmd/serve_jobs_v2*.go`: jobs scheduler, runners, and HTTP handlers.
- `cmd/serve_live*.go`: live voice and realtime transport.
- `cmd/serve_hub*.go`: Hub server, routes, and reverse connection lifecycle; Web and Hub share the passkey implementation here.
- `cmd/serve.go`, `cmd/serve_media.go`: Telegram/platform startup and media integration still in cmd.
- `internal/serve/`: existing server primitives and Telegram platform wiring.

Focused tests (with an isolated HOME/XDG tree): `go test ./internal/app/serve/... -count=1`, `go test ./cmd -run 'ServeRoutesGolden|HubReverse|JobsV2|ServeShell' -count=1`.
Do not import `cmd` from application packages; keep Cobra flags and reference generation in `cmd`.
