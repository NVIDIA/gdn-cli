# gdn

GDN CLI for publishing app folders to NGC and managing apps in the GDN portal.

The CLI is named `gdn`. It uses Cobra for grouped commands, help, and shell completion. Dependencies are managed with Go modules.

## Users

Use this section if you want to upload, publish, update, launch, or delete GDN apps.

### Install

Download the archive for your platform from the
[Releases page](https://github.com/NVIDIA/gdn-cli/releases), extract it, and put
`gdn` (`gdn.exe` on Windows) on your `PATH`.

#### Verifying downloads

Every release includes `checksums.txt`. Check the archive you downloaded against
it before extracting:

```bash
sha256sum --ignore-missing -c checksums.txt     # macOS: shasum -a 256 --ignore-missing -c checksums.txt
```

The Windows and macOS binaries are also code-signed by NVIDIA Corporation, and
the macOS binaries are notarized by Apple, so they run without security
warnings.

Windows (PowerShell): `Status` should be `Valid` and the signer NVIDIA Corporation.

```powershell
Get-AuthenticodeSignature .\gdn.exe | Format-List Status, SignerCertificate
```

macOS: the signing authority should be `Developer ID Application: NVIDIA
Corporation (6KR3T733EC)`, and `spctl` should report `Notarized Developer ID`.

```bash
codesign -dv --verbose=4 ./gdn 2>&1 | grep -E 'Authority|TeamIdentifier'
spctl --assess --type install -vv ./gdn
```

Linux binaries are not signed; rely on `checksums.txt`.

### Setup

**First, generate a personal API key.** Sign in to the [GDN Portal API Keys page](https://gdn.nvidia.com/settings/api-keys/), create a key for the org you want to work with, and copy it (it looks like `nvapi-xxxx`). This key is the CLI API key.

**Find your org name.** The `--org` value the CLI uses is your org's **Org Name** from the GDN portal (Settings → Organization), not the Display Name. The Org Name is a numeric string (e.g. `0123456789012345`).

Then initialize the config for that org (written to `~/.config/gdn/config.toml`) and make it the default:

```bash
# Save the org's API key, then set it as the default org
gdn config set --org <org-name> --api-key nvapi-xxxx
gdn config use <org-name>

# Verify the key and connectivity
gdn config check --org <org-name>
```

Alternatively, export the credentials in your shell. Environment variables take precedence over the stored config:

```bash
export NGC_API_KEY=nvapi-xxxx
export NGC_ORG=my-org
```

The CLI does not read `.env` files. Credentials come from the org config file, environment variables, and flags only.

Common settings:

| Setting | Flag | Env var |
| ------- | ---- | ------- |
| NGC API key | _(stored config or env)_ | `NGC_API_KEY` |
| NGC org name (see Settings → Organization in the GDN portal) | `--org` | `NGC_ORG` |
| GDN launch gateway URL | `--gdn-base-url` | `GDN_BASE_URL` |

The NGC base URL serves both the NGC registry and app management (the portal). It is set per org via `gdn config set --org <org> --ngc-base-url <url>` (or the `NGC_BASE_URL` env var); it is no longer a command flag. `GDN_BASE_URL` is only the launch gateway (nonce + session server).

There is no `--api-key` command flag on operational commands; the API key comes from the org's stored config or the `NGC_API_KEY` env var.

### Multiple orgs

The CLI keeps one config per org in a single file (default `~/.config/gdn/config.toml`, override with `GDN_CONFIG_FILE`). Manage them with the `gdn config` group:

| Goal | Command |
| ---- | ------- |
| Save or update an org's settings | `gdn config set --org <org-name> --api-key <key>` |
| List configured orgs | `gdn config list` |
| Show an org's resolved settings | `gdn config show <org-name>` |
| Set the default org | `gdn config use <org-name>` |

Org names are numeric, so `gdn config` shows each org's Display Name beside it:

```
$ gdn config list
2 org(s) in ~/.config/gdn/config.toml:
* 0123456789012345 (my-org)
  9876543210987654 (my-other-org)

* = default org
```

The display name comes from NGC and is cached in the config file, so listing orgs needs no credentials. `gdn config set` stores it when you save an API key, and `gdn config check` refreshes it.

Scope any command to an org with `--org <org>` (or `NGC_ORG`); the matching stored config loads automatically. Omit it to use the default org. Endpoints default to production: the NGC base (`NGC_BASE_URL`, `https://api.ngc.nvidia.com`) serves both the NGC registry and app management (the portal), while the GDN API Gateway (`GDN_BASE_URL`, `https://api.gdn.nvidia.com`) serves only the launch nonce and the session server. The browser launch client (`GDN_CLIENT_URL`, `https://client.gdn.nvidia.com`) is separate.

`gdn app launch` only requires `--app-id`. The org comes from your config/`--org`, and the gateway and client URLs come from the org config (defaulting to production), so you normally don't set them. To target another environment, override the two base URLs for an org with `gdn config set`:

```bash
gdn config set --org my-org \
  --gdn-base-url https://api.stg.gdn.nvidia.com \
  --client-base-url https://stg.client.gdn.nvidia.com
```

You can also override streaming features for a single session, pass partner secure data, and tag the session with a stream lookup id or user id; all of these are bound to the session nonce at launch:

```bash
# Override applicationFeatures (JSON object; inline, @file, or - for stdin)
# and set partnerSecureData for this session
gdn app launch --app-id <app-id> \
  --app-features '{"TOUCH_SUPPORT": true, "MAX_BIT_RATE": 100}' \
  --partner-secure-data <secure-data> \
  --stream-lookup-id <stream-lookup-id> \
  --user-id <user-id>
```

To run a single command against a different account, select its org with `--org` (the stored config for that org supplies the key and endpoints):

```bash
gdn --org <other-org> app launch --app-id <app-id>
```

### Common Commands

Run `gdn --help` for command groups, and `gdn <group> <command> --help` for exact flags.

| Goal | Command |
| ---- | ------- |
| Check credentials and the instance names GDN offers | `gdn config check` |
| Create a new app from a folder | `gdn app create --dir ./my-app --app-name my-app-1 --version 1.0.0` |
| Add a new version to an app | `gdn app version add --app-id <app-id> --dir ./my-app --version 1.1.0` |
| Update an app without uploading | `gdn app update --app-id <app-id> [flags]` |
| Clone an existing app | `gdn app clone --app-id <app-id>` |
| Launch an app | `gdn app launch --app-id <app-id>` |
| List apps | `gdn app list` |
| Upload only to NGC | `gdn resource upload --dir ./my-app --name my-app-1 --version 1.0.0` |
| Update NGC resource (base version + new files) | `gdn resource update --name my-app-1 --base-version 1.0.0 --version 1.0.1 --file ./install.cmd` |
| List NGC resources | `gdn resource list` |
| Delete an app | `gdn app delete --app-id <app-id>` |
| Delete a resource version | `gdn resource delete --name my-app-1 --version 1.0.0` |

Use `--dry-run` on upload/create/delete commands to preview work without making API calls.

Use `--json` when scripting. Human output may include warnings and fix-up hints on errors; JSON output keeps stdout machine-readable, with warnings on stderr.

### Example Workflows

These cover the most common end-to-end tasks. Set `--org` (or `NGC_ORG`) once via `gdn config use <org>` to omit it below.

**1. Upload a new resource**

Upload an app folder to NGC as a versioned resource:

```bash
gdn resource upload --org my-org --dir ./my-app --name my-app-1 --version 1.0.0
```

**2. Create a new application with an initial version**

Create the app and point its first version at the uploaded resource, naming the executable to launch:

```bash
gdn app create --org my-org \
  --app-name my-app \
  --version 1.0.0 \
  --resource my-app-1:1.0.0 \
  --exec-path app.exe \
  --exec-args "--fullscreen,--no-splash"
```

Application names are 1-32 characters: letters, digits, `-` and `_`, starting with a letter or digit. The CLI checks this before uploading anything.

Streaming settings left out of `--app-features` get GDN's defaults (`gdn config check` shows them). To restrict where the app runs or how it connects, pass the whole policy object:

```bash
gdn app create ... --app-features '{
  "COVERAGE_AREA_POLICY": {"POLICY": "CUSTOM", "COVERAGE_AREAS": ["us-north-east"]},
  "TRANSPORT_POLICY": {"POLICY": "RELAY", "RELAY_PROTOCOL": "TCP", "RELAY_LOCATION": "CSP"}
}'
```

`TRANSPORT_POLICY` needs all three members, and `COVERAGE_AREA_POLICY` with `CUSTOM` needs `COVERAGE_AREAS`. `gdn app clone` copies both policies from the source app; an override there may name only the members that change.

**Display modes**

An app allows one or more display modes, each a resolution and a maximum frame rate written `RES@FPS`. `--display-mode` sets the whole list, on `app create` or `app update`:

```bash
gdn app update --app-id <app-id> --display-mode 4k@60 --display-mode 1080p@120
```

To change one mode and keep the rest, use `--add-display-mode` and `--remove-display-mode` on `app update`. The CLI reads the app's current list, edits it and sends the result, and `--dry-run` shows the list before and after:

```bash
gdn app update --app-id <app-id> --add-display-mode 1440p@90 --dry-run
```

Adding a mode that is already listed, or removing one that is not, changes nothing. Removing the last mode is refused; use `--display-mode` to replace the list instead.

The CLI checks each new mode against the ones GDN offers before sending anything. `--resolution` and `--max-fps` are deprecated; they still work when given together, as one mode.

`--exec-path` is the executable path relative to the startup resource. A resource that belongs to an NGC team takes the team as a prefix: `--resource my-team/my-app-1:1.0.0`. To upload the folder and create the app in one step, pass `--dir ./my-app` instead of `--resource` (the resource is uploaded first, then referenced), and use `--executable` for a path relative to the folder.

**3. Add a new version that changes the launch command and sets an environment variable**

For simple cases, set the launch command with `--exec-path`/`--exec-args` and pass environment variables with repeatable `--env-var KEY:VALUE` flags:

```bash
gdn app version add --org my-org \
  --app-id 019e3c18-f740-78a6-a515-37fcbf3f7047 \
  --version 1.1.0 \
  --resource my-app-1:1.1.0 \
  --exec-path app.exe \
  --exec-args "--windowed,--verbose" \
  --env-var LOG_LEVEL:DEBUG \
  --env-var REGION:us-west-2
```

For richer launch configuration, supply the whole per-version `launchConfig` as a JSON document with `--launch-config` (inline, `@file`, or `-` for stdin). Any `--env-var` flags are merged into its `environment`:

```bash
cat > launch.json <<'JSON'
{
  "executablePath": "app.exe",
  "executableArgs": ["--windowed", "--verbose"],
  "environment": [
    { "key": "LOG_LEVEL", "value": "DEBUG" }
  ]
}
JSON

gdn app version add --org my-org \
  --app-id 019e3c18-f740-78a6-a515-37fcbf3f7047 \
  --version 1.1.0 \
  --resource my-app-1:1.1.0 \
  --launch-config @launch.json
```

`executablePath` is relative to the startup resource; the CLI expands it to the seat's `%GDN_ROOT%` path. `environment` is an array of `{ "key": ..., "value": ... }` objects (total size under 4096 bytes); `--env-var KEY:VALUE` splits on the first colon, so values may contain colons. The JSON document also accepts `appLogPaths` and `appCrashdumpPaths`. Adding a version does not change which version is served to users; promote it with `gdn app set-production --app-id <id> --version-id <ver>` when ready.

Non-production launch example (point the org at another environment's gateway and client, then launch):

```bash
gdn config set --org my-org \
  --gdn-base-url https://api.stg.gdn.nvidia.com \
  --client-base-url https://stg.client.gdn.nvidia.com
gdn app launch --app-id 019e3c18-f740-78a6-a515-37fcbf3f7047 --open=false
```

### Shell Completion

Completion is generated by Cobra. After setup, tab completion suggests command groups, subcommands, and flags:

```bash
gdn app <TAB>
gdn resource <TAB>
gdn app version <TAB>
```

Current shell session only:

```bash
# zsh
source <(gdn completion zsh --script)

# bash
source <(gdn completion bash --script)

# fish
gdn completion fish --script | source

# PowerShell
gdn completion powershell --script | Out-String | Invoke-Expression
```

Persistent setup:

```bash
# zsh
mkdir -p ~/.zsh/completions
gdn completion zsh --script > ~/.zsh/completions/_gdn
```

Then add this to `~/.zshrc` if it is not already present:

```zsh
fpath=(~/.zsh/completions $fpath)
autoload -Uz compinit
compinit
```

For bash on Linux:

```bash
mkdir -p ~/.local/share/bash-completion/completions
gdn completion bash --script > ~/.local/share/bash-completion/completions/gdn
```

For fish:

```fish
mkdir -p ~/.config/fish/completions
gdn completion fish --script > ~/.config/fish/completions/gdn.fish
```

### Compatibility

Older flat command names such as `create-app`, `upload`, and `add-version` are kept as hidden compatibility aliases. New docs and help recommend the grouped commands.

## Developers/Maintainers

Use this section if you are changing, testing, or releasing the CLI.

### Build

Go 1.26+ is expected. Runtime code is pure Go with no CGO.

```bash
make help
make bin
make install
make test
```

The product version comes from the nearest git tag (`git describe`). Developer
builds look like `0.1.1-3-gabcd123` or `0.1.1-dirty`. Releases embed the tag
exactly (GoReleaser strips the leading `v`).

For an exact version string:

```bash
make bin VERSION=0.1.1
```

Direct build:

```bash
go build -o bin/gdn ./cmd/gdn
```

Install on your `PATH`:

```bash
make install
```

`make install` writes `gdn` to `GOBIN` when set, otherwise `GOPATH/bin`.
Make sure that directory is on your `PATH`, then verify:

```bash
which gdn
gdn version
```

Build all release binaries:

```bash
./scripts/build-all.sh
```

### Test

Go unit tests:

```bash
make test
```

Before going offline, you can prefetch module dependencies:

```bash
go mod download
```

### Project Layout

```text
cmd/gdn/main.go      installable gdn entrypoint
src/cmd/            Cobra command tree and CLI orchestration
src/cmd/ngc.go      NGC artifact registry client (upload/download)
src/cmd/portal.go   GDN portal client via the NGC gateway
src/cmd/publish.go  scan folder -> upload resource -> create/update app
scripts/            build and release helper scripts
vendor/             local Go dependencies for offline builds; ignored by git
```

### License And Contributions

This project is licensed under Apache 2.0. See `LICENSE`.

Third-party dependency licenses are listed in `THIRD_PARTY_DEPENDENCIES.md`.

Contributions must be signed off under the Developer Certificate of Origin.
See `CONTRIBUTING.md`.

### Maintainer Notes

- Keep upload latency-sensitive code in `src/cmd/ngc.go` and `src/cmd/publish.go` focused; CLI grouping should not add work to the upload path.
- NGC API traffic and S3 part PUTs use separate HTTP clients; 429/503 responses throttle the shared upload pool. Prefer fixing the throttle over raising `--file-workers` when stage returns rate limits.
- Keep human next-step hints out of JSON stdout. Warnings should go to stderr.
- Keep compatibility aliases hidden unless we intentionally want to advertise old names again.
