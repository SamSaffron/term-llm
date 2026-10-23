module github.com/samsaffron/term-llm

go 1.27.3

// Patch-pinned: CI and release builds read this directive via
// go-version-file: go.mod, so the pinned patch is the Go version that shipped
// binaries are built with.

// npm installs can ship Go packages (frontend/node_modules/flatted/golang);
// keep them out of ./... so go build/vet/test never compile npm content.
ignore node_modules

replace github.com/muesli/reflow v0.3.0 => ./internal/reflow

// term-llm owns this terminal subsystem; see internal/terminal/README.md.
replace charm.land/bubbletea/v2 => ./internal/terminal/runtime

replace github.com/charmbracelet/ultraviolet => ./internal/terminal/renderer

require (
	charm.land/bubbles/v2 v2.2.1
	charm.land/bubbletea/v2 v2.0.8
	charm.land/huh/v2 v2.0.3
	charm.land/lipgloss/v2 v2.0.6
	github.com/BourgeoisBear/rasterm v1.1.2
	github.com/SherClockHolmes/webpush-go v1.4.0
	github.com/alecthomas/chroma/v2 v2.27.0
	github.com/bmatcuk/doublestar/v4 v4.10.3
	github.com/charmbracelet/x/ansi v0.11.9
	github.com/creack/pty v1.1.24
	github.com/dustin/go-humanize v1.1.0
	github.com/fxamacker/cbor/v2 v2.9.6
	github.com/go-telegram-bot-api/telegram-bot-api/v5 v5.5.1
	github.com/go-viper/mapstructure/v2 v2.5.0
	github.com/go-webauthn/webauthn v0.18.2
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/mattn/go-runewidth v0.0.31
	// The 1.8 line carries the SEP-2575 discover/listen work this client depends on.
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/muesli/cancelreader v0.2.2
	github.com/muesli/reflow v0.3.0
	github.com/pion/turn/v5 v5.1.2
	github.com/pion/webrtc/v4 v4.2.25
	github.com/sahilm/fuzzy v0.1.3
	github.com/shogoki/gotextdiff v1.22.0
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/yuin/goldmark v1.8.6
	golang.org/x/crypto v0.58.0
	golang.org/x/image v0.47.0
	golang.org/x/mod v0.42.0
	golang.org/x/net v0.61.0
	golang.org/x/oauth2 v0.37.0
	golang.org/x/sync v0.24.0
	golang.org/x/sys v0.49.0
	golang.org/x/term v0.47.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.60.1
)

require (
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/catppuccin/go v0.3.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886 // indirect
	github.com/charmbracelet/x/exp/ordered v0.1.0 // indirect
	github.com/charmbracelet/x/exp/strings v0.1.0 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/dlclark/regexp2/v2 v2.9.1 // indirect
	github.com/go-webauthn/x v0.3.1 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mitchellh/hashstructure/v2 v2.0.2 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/philhofer/fwd v1.2.1 // indirect
	github.com/pion/datachannel v1.6.3 // indirect
	github.com/pion/dtls/v3 v3.1.15 // indirect
	github.com/pion/ice/v4 v4.4.8 // indirect
	github.com/pion/interceptor v0.1.49 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.2.2 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.20 // indirect
	github.com/pion/rtp v1.10.6 // indirect
	github.com/pion/sctp v1.12.0 // indirect
	github.com/pion/sdp/v3 v3.0.20 // indirect
	github.com/pion/srtp/v3 v3.1.3 // indirect
	github.com/pion/stun/v4 v4.0.1 // indirect
	github.com/pion/transport/v5 v5.1.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/tinylib/msgp v1.6.5 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/xo/terminfo v1.2.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
