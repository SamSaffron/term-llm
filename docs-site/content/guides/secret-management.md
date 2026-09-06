---
title: "Secret management"
description: "Keep credentials out of public config with 1Password, macOS Keychain, Linux keyrings, or a private secrets.yml file."
kicker: "Credentials"
---
Keep `config.yaml` and `mcp.json` in your dotfiles without committing API keys or tokens. term-llm can resolve credentials from a vault, an operating-system keyring, environment variables, or a private file when the consuming feature needs them.

This guide covers credentials you configure explicitly. Provider-managed OAuth token stores and other application state have their own storage rules; see [Providers and models](/reference/providers-and-models/#credentials).

## Choose where secrets live

| Approach | Good fit | Considerations |
| --- | --- | --- |
| [1Password or another vault](#keep-secrets-in-1password-or-another-vault) | Existing vault users; machines with appropriate CLI authentication | Requires the vault's CLI and access to the secret |
| [macOS Keychain](#keep-secrets-in-macos-keychain) | Local Mac workflows using built-in encrypted storage | Keychain access may require interactive approval |
| [Linux desktop keyring](#keep-secrets-in-a-linux-desktop-keyring) | Open-source desktop workflows using Secret Service | Needs a compatible backend, a user D-Bus session, and an unlocked collection |
| [Private `secrets.yml`](#keep-all-your-configured-secrets-in-secretsyml) | Simple, dependency-free separation from public config | The file is plaintext; protect it and its backups |
| Environment variables | CI or a service manager that injects credentials | Keep literal secrets out of committed shell profiles and service definitions |

Vaults and keyrings avoid separate plaintext secret files. They do not prevent resolved credentials from entering process memory when used. Choose a backend whose access and unlock behavior fits how you run term-llm.

## Reference syntax

term-llm supports dynamic resolution for config values, so `config.yaml` can be tracked in dotfiles without storing a single plaintext secret:

- `op://...` for 1Password secret references
- `srv://...` for DNS SRV-based endpoint discovery
- `file://path` and `file://path#nested.field` for file contents or a JSON/YAML field
- `$(...)` for command output (any secret manager with a CLI)
- `${VAR}` / `$VAR` for environment variables

Example:

```yaml
providers:
  production-llm:
    type: vllm
    model: Qwen/Qwen3-30B-A3B
    url: "srv://_vllm._tcp.ml.company.com/v1/chat/completions"
    api_key: "op://Infrastructure/vLLM Cluster/credential?account=company.1password.com"
```

Use 1Password, another CLI-backed vault, macOS Keychain, or a Linux Secret Service keyring if you do not want plaintext credentials in separate files either. The `secrets.yml` recipe below is an optional alternative for keeping secrets separate from public config—not a requirement for deferred credentials.

## Where it applies

The following credential and endpoint settings in `config.yaml` accept these forms:

| Setting | Notes |
| --- | --- |
| `providers.<name>.api_key`, `url`, `base_url`, `env.*` | Also `access_key_id`, `secret_access_key`, `session_token` for Bedrock |
| `image.<provider>.api_key` | gemini, openai, xai, venice, flux, openrouter |
| `audio.<provider>.api_key` | venice, gemini, elevenlabs |
| `music.<provider>.api_key` | venice, elevenlabs |
| `transcription.<provider>.api_key` | venice, elevenlabs |
| `embed.<provider>.api_key`, `embed.ollama.base_url` | openai, gemini, jina, voyage |
| `search.<provider>.api_key`, `search.google.cx`, `search.exa_mcp.url` | exa, exa_mcp, perplexity, parallel, tavily, brave, google |
| `serve.telegram.token` | |
| `serve.web_push.vapid_public_key`, `vapid_private_key` | |

MCP servers in `mcp.json` accept the same syntax in `headers`, `env`, and `oauth.client_secret`:

```json
{
  "servers": {
    "example": {
      "url": "https://mcp.example.com/mcp",
      "headers": { "Authorization": "$(op read \"op://Private/Example MCP/token\")" }
    }
  }
}
```

In `mcp.json` maps, only explicitly deferred forms (`op://`, `srv://`, `file://`, `$(...)`, `${VAR}`) are resolved. A bare `$VAR` stays literal; `${VAR}` is an explicit environment reference. References replace whole values, not substrings such as `Bearer ${TOKEN}`.

## Keep secrets in 1Password or another vault

You do not need a plaintext secrets file. Keep the secret in your vault and store only its reference in `config.yaml`:

```yaml
providers:
  openai:
    api_key: "op://Private/OpenAI/api_key"

search:
  provider: brave
  brave:
    api_key: "op://Private/Brave Search/api_key"

serve:
  telegram:
    token: "op://Private/Telegram/token"
```

Replace the vault, item, and field names with yours, and install and authenticate the 1Password CLI (`op`). Other secret managers work through `$(...)` when their CLI can return just the requested secret on standard output. This is command substitution, not a separate built-in integration for each vault. Configure authentication with the secret manager rather than putting its unlock password in the command.

## Keep secrets in macOS Keychain

On a Mac, the built-in **Keychain** can hold the credentials instead of a plaintext `secrets.yml`. term-llm reads them through macOS's `/usr/bin/security` CLI; no additional secret-manager CLI is needed.

Open **Keychain Access**, select your **login** keychain, and choose **File → New Password Item**. Create one item for each credential you use:

| Keychain Item Name | Account Name | Password |
| --- | --- | --- |
| `term-llm.openai` | `term-llm` | Your OpenAI API key |
| `term-llm.brave` | `term-llm` | Your Brave Search API key |
| `term-llm.telegram` | `term-llm` | Your Telegram bot token |
| `term-llm.github` | `term-llm` | Your GitHub token for MCP |

Enter the actual secret in the Password field, not in a terminal command. This avoids putting it in shell history or command-line arguments. These are generic password items in **Keychain Access**, not website logins in the Passwords app.

Then keep only the lookup commands in `config.yaml`:

```yaml
providers:
  openai:
    api_key: '$(/usr/bin/security find-generic-password -a term-llm -s term-llm.openai -w)'

search:
  provider: brave
  brave:
    api_key: '$(/usr/bin/security find-generic-password -a term-llm -s term-llm.brave -w)'

serve:
  telegram:
    token: '$(/usr/bin/security find-generic-password -a term-llm -s term-llm.telegram -w)'
```

`-a` selects the account, `-s` selects the service (the Keychain Item Name), and `-w` returns only the password. The same lookup works in `mcp.json`:

```json
{
  "servers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "GITHUB_PERSONAL_ACCESS_TOKEN": "$(/usr/bin/security find-generic-password -a term-llm -s term-llm.github -w)"
      }
    }
  }
}
```

macOS may ask you to unlock the keychain or approve access when the credential is first needed. Access is governed by the keychain item's permissions; this does not promise a Touch ID prompt on every lookup. Avoid broadly allowing every application access just to suppress prompts. Running the lookup by itself in a terminal prints the secret, so do not paste its output into logs or issue reports.

For unattended `serve` processes, SSH sessions, or launch services, verify that the process's macOS user can access the keychain without interactive approval. A locked or inaccessible keychain causes a resolution error; command lookups have a 15-second timeout. Retry after restoring access. Restart long-running processes after rotating cached credentials.

This keeps plaintext credentials out of your dotfiles and separate secret files. Keychain manages encrypted storage; the resolved credential still enters term-llm's process memory when used.

## Keep secrets in a Linux desktop keyring

Linux has an open-standard option: the **freedesktop.org Secret Service API**. Use the open-source `secret-tool` CLI from libsecret with a compatible backend, such as **GNOME Keyring** or **KeePassXC with its Secret Service integration enabled**. This keeps credentials in the backend's encrypted keyring/database rather than a plaintext secrets file.

Install the client for your distribution:

```bash
# Debian / Ubuntu
sudo apt install libsecret-tools

# Arch Linux
sudo pacman -S libsecret
```

Installing the client alone does not provide secret storage: a Secret Service backend must be running and available on your user's D-Bus session bus. GNOME desktops commonly already have one. For KeePassXC, enable its Secret Service integration and configure the database/group it exposes before continuing.

In an interactive terminal, store each credential using public lookup attributes:

```bash
secret-tool store --label='term-llm OpenAI API key' application term-llm credential openai
secret-tool store --label='term-llm Brave Search API key' application term-llm credential brave
secret-tool store --label='term-llm Telegram bot token' application term-llm credential telegram
secret-tool store --label='term-llm GitHub MCP token' application term-llm credential github
```

Each command prompts for the secret. Enter it at that prompt, not as a command-line argument or an `echo` command, so it does not enter shell history. The attribute pairs identify the item; use the same pairs for lookups. Keep secrets out of labels and attributes, which are metadata rather than the protected secret value.

Reference the keyring from `config.yaml`:

```yaml
providers:
  openai:
    api_key: '$(secret-tool lookup application term-llm credential openai)'

search:
  provider: brave
  brave:
    api_key: '$(secret-tool lookup application term-llm credential brave)'

serve:
  telegram:
    token: '$(secret-tool lookup application term-llm credential telegram)'
```

And from `mcp.json`:

```json
{
  "servers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "GITHUB_PERSONAL_ACCESS_TOKEN": "$(secret-tool lookup application term-llm credential github)"
      }
    }
  }
}
```

`secret-tool lookup` writes the secret to standard output, which term-llm captures when the credential is needed. Running it by itself in a terminal displays the secret; do not copy that output into logs or issue reports. Successful deferred lookups are cached, so restart long-running processes after rotating credentials.

**Desktop session versus unattended server:** a user service, SSH session, container, or system daemon may not have access to your desktop's D-Bus session or an unlocked collection. Run term-llm as the intended user and arrange access through your chosen backend; installing `secret-tool` or running it with `sudo` does not solve this. Unlock the collection before unattended use—interactive unlock dialogs may be unavailable, and command lookups have a 15-second timeout. For a headless machine, a CLI-backed vault with an appropriate noninteractive authentication setup may fit better.

The backend controls encryption, unlocking, and access policy. An unlocked desktop keyring is not necessarily isolated from other processes running as your user, and the resolved credential enters term-llm's memory. Do not use an empty keyring password or disable encryption just to avoid an unlock prompt.

## Keep all your configured secrets in `secrets.yml`

Keep the credentials used by the settings above in one private YAML file and track only the references in your dotfiles. Native `file://` field lookup needs no helper command or additional dependency. Files ending in `.yml` or `.yaml` are parsed as YAML; other extensions use JSON when a fragment is present.

Create `/home/alice/.config/term-llm/secrets.yml` outside your public dotfiles repository, with permissions `0600` (and keep any backups private). Replace `/home/alice` in every example with your actual home directory: `file://` references use absolute paths, not shell `~` or embedded `$HOME` expansion. These are placeholders, not real credentials:

```yaml
openai: "sk-replace-me"
venice: "replace-me"
brave: "replace-me"
telegram: "123456:replace-me"
github: "github_pat_replace-me"
mcp_authorization: "Bearer replace-me"
```

Then reference the file from `config.yaml`:

```yaml
providers:
  openai:
    api_key: "file:///home/alice/.config/term-llm/secrets.yml#openai"

image:
  venice:
    api_key: "file:///home/alice/.config/term-llm/secrets.yml#venice"

search:
  provider: brave
  brave:
    api_key: "file:///home/alice/.config/term-llm/secrets.yml#brave"

serve:
  telegram:
    token: "file:///home/alice/.config/term-llm/secrets.yml#telegram"
```

The same file can supply MCP credentials in `mcp.json`:

```json
{
  "servers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "GITHUB_PERSONAL_ACCESS_TOKEN": "file:///home/alice/.config/term-llm/secrets.yml#github"
      }
    },
    "authenticated-api": {
      "type": "http",
      "url": "https://api.example.com/mcp",
      "headers": {
        "Authorization": "file:///home/alice/.config/term-llm/secrets.yml#mcp_authorization"
      }
    }
  }
}
```

Use a dot-separated fragment for nested fields, for example `#providers.openai.api_key`. Missing or null fields and malformed files produce an error; YAML files must contain one document. Quote secret values so YAML does not reinterpret numeric- or date-looking tokens, and so leading zeroes are preserved. Store the **complete header value**, including `Bearer ` when the server requires it: references replace whole values, not substrings. Without a `#fragment`, `file://` returns the entire file as trimmed text, not an individual secret. Add fields for any other credentials you use, including embedding, audio, music, OAuth client secrets, and both VAPID keys, using the settings table above. Video uses the shared `image.venice.api_key` setting.

This file is plaintext secret storage, not encryption. Do not commit it; `.gitignore` alone does not protect a file already tracked by Git. This recipe centralizes configured credentials, not automatically managed OAuth token stores or other application state. Restart long-running processes after changing secrets because successful deferred resolutions are cached. `secrets.yml` is not discovered or loaded automatically: each setting explicitly references its field.

Only use trusted config files: `$(...)` executes shell commands with your user permissions. The same warning applies to deferred values in `mcp.json`.

## Resolution is lazy

Deferred vault, file, command, and DNS references are not resolved when the config loads. They resolve when their consumer needs them. For example, `image.venice.api_key` is resolved for image generation or another Venice feature that falls back to that shared key; a vault-backed `search.brave.api_key` is not touched until a web search happens. The web-push public key is also needed to prepare the browser UI, while the private key is needed when sending notifications.

Availability checks ("is web search configured?", "does Telegram need setup?") deliberately inspect configuration without resolving, so they never trigger a vault unlock. Non-empty successful deferred feature-credential resolutions are memoized for the life of the process, and concurrent requests for the same reference share a lookup. Failed or empty lookups can be retried on subsequent use. Restart a long-running `term-llm serve` after rotating secrets.

Credential errors are reported by the consuming feature, for example `image.venice.api_key: 1password: failed to read ...`; a configured search provider is not silently replaced with another provider when its secret fails. The CLI retains its DuckDuckGo fallback when search configuration is absent or invalid, determined without resolving any deferred references. An unavailable web-push public key temporarily disables push setup without preventing the rest of the UI from loading.

An empty configured value or an unset environment reference can use that setting's environment fallback. A failed explicit vault/file/command reference is an error, not permission to silently switch accounts through a lower-priority credential. Existing cascade order (such as audio Venice → image Venice) is preserved for absent or empty credentials.

## Related pages

- [Configuration reference](/reference/configuration/#dynamic-secrets-and-endpoints)
- [Providers and models](/reference/providers-and-models/#credentials)
- [MCP servers](/guides/mcp-servers/#keeping-secrets-out-of-mcpjson)
