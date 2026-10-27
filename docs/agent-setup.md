# Set up MemSpace on this machine — instructions for AI coding agents

You are an AI coding agent (Claude Code, Codex, …) and the user asked you to
set up MemSpace on the machine you are running on. MemSpace is a
self-hosted memory server: this machine becomes a *device* whose agent
sessions are captured and synced to the user's server, and whose agents
get recent work and related history as context.

Follow the steps in order. Run each command yourself, read its output and
continue; stop and ask the user only where a step says so.

## Inputs

You need two values from the user:

- `SERVER`: the server URL, e.g. `http://192.168.1.13:8770` or
  `https://memory.example.com` (no trailing slash).
- `INVITE`: a one-time invite code starting with `msi_`.

If either is missing, ask for it. The user creates invites on the server
with `memspace server invite`, or in the web UI under Administration → Add
device.

## Rules

- **Tell the user before changing their configuration**, and use your
  normal approval flow for commands that write outside the current
  project. Setup writes these files:
  - `~/.local/bin/memspace` (the client program; Windows: `%USERPROFILE%\bin\memspace.exe`)
  - `~/.memspace/credentials.json` (this device's token, mode 0600)
  - Claude Code: hooks in `~/.claude/settings.json`, a user-scoped MCP server `memspace`
  - Codex: hooks in `~/.codex/hooks.json`, an MCP server `memspace` in `~/.codex/config.toml`
- **Never print or repeat secrets**: do not `cat` `~/.memspace/credentials.json`
  and do not run `memspace mcp token`. The invite may appear in commands; it
  is single-use and short-lived.
- Do not edit agent configs by hand; `memspace` does it and validates it.
- Do not put the program in a temporary directory: hooks run it by absolute
  path, and `memspace onboard` refuses binaries in temp directories.

## 1. Detect the platform

```bash
uname -s; uname -m
```

`Darwin` → `darwin`, `Linux` → `linux`; `x86_64`/`amd64` → `amd64`,
`arm64`/`aarch64` → `arm64`. On Windows (PowerShell), the platform is
`windows-amd64`; Windows support is experimental, tell the user so.

## 2. Check that the server is reachable

```bash
curl -fsS --noproxy '*' "$SERVER/api/v1/health"
```

Expect JSON with `"status":"ok"`. Use `--noproxy '*'` when `SERVER` is on a
private network (192.168.x, 10.x, 172.16–31.x, 100.64–127.x / Tailscale,
`*.local`, `*.ts.net`): desktop HTTP proxies usually cannot reach those. For
a public hostname, drop `--noproxy` if a proxy is required. If the server is
unreachable, stop and tell the user (wrong URL, server down, or a firewall).

## 3. Is MemSpace already installed?

```bash
command -v memspace && memspace client status
```

- Connected to the same `SERVER`: setup was done before. Install the client
  anyway (step 4) to update it, then skip to step 5; `onboard` keeps the
  existing connection and does not use the invite.
- Connected to a different server: stop and ask the user whether to switch.
  Switching is `memspace client connect --force "$SERVER" --invite "$INVITE"`.

## 4. Install the client

Download it from the server (the usual case):

```bash
mkdir -p ~/.local/bin
curl -fsSL --noproxy '*' "$SERVER/downloads/memspace-<os>-<arch>" -o ~/.local/bin/memspace
chmod +x ~/.local/bin/memspace
~/.local/bin/memspace --help | head -1
```

Windows (PowerShell):

```powershell
New-Item -ItemType Directory -Force "$HOME\bin" | Out-Null
Invoke-WebRequest "$SERVER/downloads/memspace-windows-amd64.exe" -OutFile "$HOME\bin\memspace.exe"
```

If the download returns 404, the server does not publish client builds.
Build the client from source instead (needs Go ≥ 1.26, pure Go, no C
toolchain):

```bash
CGO_ENABLED=0 go install github.com/chux0519/memspace/cmd/memspace@latest
# installs to "$(go env GOPATH)/bin/memspace"
```

If `~/.local/bin` (or the Go bin directory) is not on `PATH`, use the full
path in the next steps and tell the user how to add it to their shell
profile; hooks do not need it on `PATH`.

## 5. Make the agent CLIs visible

`onboard` sets up every agent it can find on `PATH` (it calls `claude` and
`codex` to register MCP):

```bash
command -v claude codex
```

If an agent the user has is not found, it is usually installed through nvm
in a shell where nvm is not loaded. Run the next steps in a login shell
(`zsh -lc '…'` on macOS, `bash -lc '…'` on Linux) or after
`source ~/.nvm/nvm.sh && nvm use`.

## 6. Connect and install

```bash
memspace onboard "$SERVER" --invite "$INVITE"
```

Add `--trust` if Codex is installed: Codex only runs hooks the user has
reviewed, and `--trust` records that review for MemSpace's hooks. Ask the
user first; without it they approve the hooks themselves in Codex's
`/hooks`. Add `--name <name>` to choose the device name (default: host
name). To set up only some agents: `--agent claude` or `--agent codex`.

If it fails with `invalid, expired or already used invite`, ask the user
for a new invite and run the command again.

## 7. Verify

```bash
memspace agent doctor
```

Every line should be `✓`. Common `!` / `✗` lines:

| Doctor says | Fix |
|---|---|
| `mcp: not registered` | the agent CLI was not on `PATH` during step 6: fix `PATH` (step 5), then `memspace agent install claude` (or `codex`) |
| `hook trust` (Codex) | `memspace agent install codex --trust` after asking the user, or approve in Codex's `/hooks` |
| `server` unreachable | check step 2; MemSpace itself never sends requests for private addresses through a proxy |
| `semantic search` disabled or failing | server-side and optional; keyword search works. Mention it to the user, nothing to do here |

## 8. Optional: import older Codex history

New sessions are captured from now on. Ask the user whether to import
earlier Codex sessions, and from when:

```bash
memspace import codex --since YYYY-MM-DD   # or --all
```

## 9. Report

Tell the user:

- what was installed and which files changed (from the rules above);
- the doctor summary;
- that hooks load when a session starts: **this current session is not
  captured**; new Claude Code / Codex sessions are;
- how to look around: `memspace ui` opens the web UI signed in (read-only),
  `memspace search <query>` searches history, `memspace client status`
  shows sync state.
