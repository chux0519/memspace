# Set up MemSpace on this machine — instructions for AI coding agents

You are an AI coding agent (Claude Code, Codex, …) and the user asked you to
set up MemSpace on the machine you are running on. MemSpace is a
self-hosted memory server: this machine becomes a *device* whose agent
sessions are captured and synced to the user's server, and whose agents
get recent work and related history as context.

Follow the steps in order. Run each command yourself, read its output and
continue; stop and ask the user only where a step says so. Talk to the user
in their language.

## How this device gets access

A device token gives full access to the user's memory, so the server's
administrator (the user) must let this device in. By default the device
asks to join and shows a short code, e.g. `K7QF-2M9P`; the user compares it
and clicks **Approve** in the web UI. If the user gave you an invite code
(`msi_…`) instead, use it: no approval step is needed.

## Rules

- **Tell the user before changing their configuration**, and use your
  normal approval flow for commands that write outside the current
  project. Setup writes these files:
  - `~/.local/bin/memspace` (the client program; Windows: `%USERPROFILE%\bin\memspace.exe`)
  - `~/.memspace/credentials.json` (this device's token, mode 0600)
  - Claude Code: hooks in `~/.claude/settings.json`, a user-scoped MCP server `memspace`
  - Codex: hooks in `~/.codex/hooks.json`, an MCP server `memspace` in `~/.codex/config.toml`
- **Never print or repeat secrets**: do not `cat` `~/.memspace/credentials.json`
  or files under `~/.memspace/state/`, and do not run `memspace mcp token`.
- Do not approve this device yourself unless the user explicitly says so.
- Do not edit agent configs by hand; `memspace` does it and validates it.
- Do not put the program in a temporary directory: hooks run it by absolute
  path, and `memspace onboard` refuses binaries in temp directories.

## 1. Find the server

You need `SERVER`, the server URL, e.g. `http://192.168.1.13:8770` or
`https://memory.example.com` (no trailing slash).

1. If the user gave it, use it.
2. Look for hints on this machine: `memspace client status` names the
   server if MemSpace was set up here before. If this machine runs the
   server itself (`pgrep -x memspace` and a `[server]` section in
   `~/.memspace/config.toml`), use its `public_url`, or
   `http://<this machine's LAN address>:<port from address>`.
3. Otherwise ask, e.g. "Where is your MemSpace server? (for example
   http://192.168.1.13:8770)". If the user does not have one yet, stop: the
   server is set up first (README, "Run the server").

Check that it is reachable:

```bash
curl -fsS --noproxy '*' "$SERVER/api/v1/health"
```

Expect JSON with `"status":"ok"`. Use `--noproxy '*'` when `SERVER` is on a
private network (192.168.x, 10.x, 172.16–31.x, 100.64–127.x / Tailscale,
`*.local`, `*.ts.net`): desktop HTTP proxies usually cannot reach those. For
a public hostname, drop `--noproxy` if a proxy is required. If it is
unreachable, tell the user (wrong URL, server down, or a firewall) and stop.

## 2. Detect the platform

```bash
uname -s; uname -m
```

`Darwin` → `darwin`, `Linux` → `linux`; `x86_64`/`amd64` → `amd64`,
`arm64`/`aarch64` → `arm64`. On Windows (PowerShell), the platform is
`windows-amd64`; Windows support is experimental, tell the user so.

## 3. Is MemSpace already installed?

```bash
command -v memspace && memspace client status
```

- Connected to the same `SERVER`: setup was done before. Install the client
  anyway (step 4) to update it, then continue; `onboard` keeps the existing
  connection and skips approval.
- Connected to a different server: stop and ask the user whether to switch
  (`memspace client connect --force "$SERVER"`, which asks for approval).

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

`onboard` sets up every agent it finds on `PATH` (it calls `claude` and
`codex` to register MCP):

```bash
command -v claude codex
```

If an agent the user has is not found, it is usually installed through nvm
in a shell where nvm is not loaded. Run the next steps in a login shell
(`zsh -lc '…'` on macOS, `bash -lc '…'` on Linux) or after
`source ~/.nvm/nvm.sh && nvm use`.

## 6. Ask to join, then connect and install

If Codex is installed, ask the user whether to pass `--trust`: Codex only
runs hooks the user has reviewed, and `--trust` records that review for
MemSpace's hooks (otherwise they approve them in Codex's `/hooks`). Add
`--name <name>` to choose the device name (default: host name), and
`--agent claude` or `--agent codex` to set up only some agents.

**6a. Ask to join.** `--wait 0` shows the code and exits at once, so you
can relay it:

```bash
memspace onboard "$SERVER" --wait 0 [--trust]
```

The output looks like this, and the command ends with
`error: not approved yet: …`. That is expected, not a failure:

```text
This device needs approval. Ask the MemSpace administrator to approve it:

  Code:    K7QF-2M9P   (macmini · darwin, expires 16:20)
  Web UI:  http://192.168.1.13:8770/#/admin  (管理 → 待审批设备 → 批准)
  Or on the server:  memspace server approve K7QF-2M9P
```

Tell the user the code and both ways to approve, and ask them to tell you
when they have approved it. Mention:

- In the web UI they must be signed in as administrator. If they are not,
  `memspace server login-link` on the server prints a sign-in link.
- They should check that the code in the UI matches the one you showed.
- The code is valid for 10 minutes.

**6b. Finish.** When the user says it is approved, run the same command
again (it resumes the same request; the short wait covers a slow click):

```bash
memspace onboard "$SERVER" --wait 2m [--trust]
```

It prints `Approved.`, connects, installs hooks and MCP for each agent and
runs the doctor. If it says the request is still not approved, ask the
user again. If it expired, run 6a again and relay the new code. If the
user denied it, stop.

**With an invite instead** (the user gave `msi_…`): skip 6a/6b and run
`memspace onboard "$SERVER" --invite "$INVITE" [--trust]`. If it fails
with `invalid, expired or already used invite`, ask for a new invite or
use the approval steps above.

## 7. Verify

```bash
memspace agent doctor
```

Every line should be `✓`. Common `!` / `✗` lines:

| Doctor says | Fix |
|---|---|
| `mcp: not registered` | the agent CLI was not on `PATH` during step 6: fix `PATH` (step 5), then `memspace agent install claude` (or `codex`) |
| `hook trust` (Codex) | `memspace agent install codex --trust` after asking the user, or approve in Codex's `/hooks` |
| `server` unreachable | check step 1; MemSpace itself never sends requests for private addresses through a proxy |
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
