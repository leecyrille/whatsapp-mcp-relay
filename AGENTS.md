# Setup guide for AI agents

You are setting up a self-hosted WhatsApp MCP connector on the user's Windows PC, plus the optional WA Relay desktop
app. Read `README.md` and `SECURITY.md` first. Work through the stages in order. Verify each stage before moving on,
and keep a short log for the user in `C:\mcp\SETUP_LOG.md`. When what you see differs from this guide, trust what you
see, work it out, and tell the user.

## Rules

- **The user does these, and you stop and ask:**
  - QR scans, sign-ins, email PINs and passwords
  - buying or moving a domain
  - authorizing the Cloudflare MCP
  - anything that needs an elevated (administrator) shell

  Give exact numbered steps, and only for what you need from them. Never type a credential for them.
- **Never put a secret in chat, a log or a file in this repo.** Secrets are API tokens, tunnel credentials and
  signing keys. They live in `C:\mcp\.env`, `C:\mcp\cloudflared\` and `C:\mcp\secrets\`, none of which are in git.
- **Never send a real WhatsApp message to test.** A real send, group rename or exit needs the user to name the
  recipient and the wording.
- **Don't run `whatsapp-bridge.exe` to "check" it.** It takes no flags and starts for real. Use
  `mcpctl.ps1 status` and the health endpoint.
- **Access before DNS.** Create the Access app and its policy before the hostname's DNS record exists, so the
  hostname is never public without a login in front of it.
- **Bind to 127.0.0.1 only.** Open no router or firewall ports. The tunnel is the only way in.
- **Claude Desktop users:** don't launch GUI apps from a shell inside Claude Desktop. It's a packaged (MSIX) app, so
  child processes get their AppData redirected and break. Ask the user to start GUI apps from the Start menu.

## 0. Connect the Cloudflare MCP first

You'll configure Cloudflare through Cloudflare's official MCP server, which covers its whole API. Ask the user to
connect it, then check it works by listing their zones. They need a Cloudflare account with their domain on it,
showing as Active. If the domain isn't on Cloudflare yet, have them add it at dash.cloudflare.com and change the
nameservers at their registrar. That can take a few hours.

Give the user the steps for their client:
- **Claude Code:** run
  `claude mcp add --transport http cloudflare-api https://mcp.cloudflare.com/mcp`.
  Then run `/mcp`, choose `cloudflare-api`, and authorize in the browser.
- **Claude Desktop or claude.ai:** go to Settings > Connectors > Add custom connector. Use the URL
  `https://mcp.cloudflare.com/mcp`, then press Connect and authorize.
- **Other agents:** add `https://mcp.cloudflare.com/mcp` as a remote MCP server with OAuth.

Permissions to grant:
- Zone: Read and DNS: Edit on their zone
- Account: Access (Apps and Policies, Organizations, Identity Providers): Edit
- Cloudflare Tunnel: Edit

If they'd rather not connect it, the scripts in `tools\` do the same with an API token saved in `C:\mcp\.env`.
`tools\save_token_from_clipboard.ps1` saves the token without it passing through chat.

## 1. Lay out the machine

Paths are fixed to `C:\mcp`. Clone it there, and limit the folder to the user, SYSTEM and Administrators:

```powershell
git clone https://github.com/leecyrille/whatsapp-mcp-relay C:\mcp
icacls C:\mcp /inheritance:r /grant:r "${env:USERNAME}:(OI)(CI)F" "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F"
foreach ($s in 'tunnel','whatsapp') { New-Item -ItemType File -Force "C:\mcp\services\OFF-$s.flag" }
```

The OFF flags keep services from restart-looping before they're configured. `mcpctl.ps1 start <name>` clears one.

Make the private config files from the examples. They are git-ignored, so keep them that way.

| Copy | To | Fill in |
|---|---|---|
| `site.example.json` | `site.json` | The user's domain, an Access team name, and a tunnel name. Hosts default to `a` (connector) and `d` (app) |
| `gateway\gateway.example.json` | `gateway\gateway.json` | `allowed_emails` holds the emails allowed to use the connector. `tool_access` limits contact-editing tools to some of those emails |
| `whatsapp-app\config\relay.example.json` | `whatsapp-app\relay\bin\relay.json` | The emails allowed to use the app. Keep this list narrow. `name_rule` is optional and off when absent |
| `whatsapp-app\config\groups.example.json`, `runner.example.json` | `whatsapp-app\data\groups.json`, `relay\bin\runner.json` | Optional: starting chat groups, and the runner for the app's Claude features. See `whatsapp-app\README.md` |
| `whatsapp\instructions.example.md` | `instructions.md` | Optional notes the connector gives the AI, read fresh on every connect |

Ask the user which email addresses to allow. Sign-in is by a PIN emailed to that address.

## 2. Install the toolchain

Install under `C:\mcp\toolchain` where noted. Scheduled tasks run outside Claude Desktop's redirected AppData, so
tools installed only into a redirected profile can be invisible to them. Verify checksums from the official sites.

| Tool | Where | For |
|---|---|---|
| Go 1.25 or newer | `toolchain\go` | Bridge and relay |
| WinLibs GCC (x86_64, UCRT) | `toolchain\mingw64` | The bridge's SQLite (cgo) |
| cloudflared | `toolchain\cloudflared\cloudflared.exe` | The tunnel |
| Python 3.12 or newer | Any; venv at `C:\mcp\venv` | Server and gateway |
| Rust (stable) and Node 20 or newer | Normal install | Only to build the app yourself. Most people install the release instead |

```powershell
python -m venv C:\mcp\venv
C:\mcp\venv\Scripts\python.exe -m pip install -r C:\mcp\requirements.lock.txt
```

Where a pinned version won't install, drop the pin for that package. `fastmcp` must stay on 3.x and `mcp` on 1.x.

## 3. Build and test

Set these for every Go command:

```powershell
$env:GOPATH='C:\mcp\toolchain\gopath'; $env:GOCACHE='C:\mcp\toolchain\gocache'
$env:Path="C:\mcp\toolchain\go\bin;C:\mcp\toolchain\mingw64\bin;$env:Path"
```

```powershell
cd C:\mcp\whatsapp\src\whatsapp-bridge; $env:CGO_ENABLED='1'; go test ./...; go build -o whatsapp-bridge.exe .
cd C:\mcp\whatsapp-app\relay; $env:CGO_ENABLED='0'; go test ./...
go build -ldflags="-s -w" -o bin\wa-relay.exe .\cmd\wa-relay
cd C:\mcp\gateway; C:\mcp\venv\Scripts\python.exe -m pytest -q
cd C:\mcp\whatsapp\server; C:\mcp\venv\Scripts\python.exe -m pytest -q
```

Some server tests read the live store, so a failure before WhatsApp is linked may not be real. Check before "fixing"
anything.

**Phone numbers:** the bridge treats numbers with no country code as North American (`defaultCountry` in
`whatsapp/src/whatsapp-bridge/app_cards.go`). Change it if the user is elsewhere.

## 4. Link WhatsApp

Ask the user to:
1. Run `C:\mcp\whatsapp\Pair-WhatsApp.cmd` from the Start menu or File Explorer. Don't run it from your shell.
2. On the phone, open WhatsApp > Settings > Linked devices > Link a device, and scan the QR code.
3. Wait about a minute after it says it's connected, then press Ctrl+C.

The link and the message store live in `whatsapp\src\whatsapp-bridge\store\`. That folder is private, and is never
committed or shared.

## 5. Cloudflare

Follow `docs\cloudflare.md`. It lists every API call to make through the Cloudflare MCP, in a safe order:
1. The Access organization and email PIN sign-in.
2. The Access apps for the connector (with Managed OAuth) and for the app.
3. The tunnel, created locally with `cloudflared` so its credentials never pass through chat.
4. DNS, last.

Each Access app's `aud` tag goes into `gateway.json` and `relay.json`.

## 6. Run it as services

The user runs this in an **elevated** PowerShell. Give them the exact line:

```powershell
powershell -ExecutionPolicy Bypass -File C:\mcp\services\install-tasks.ps1 -Only tunnel,whatsapp
```

`services.ps1` also defines `pc`, `claude` and `sms` services. Leave those out: they belong to other connectors
that aren't in this repo. The exception is the app's optional Claude features, which use the `wa-runner` from the
`claude` service; `whatsapp-app\README.md` covers them. Then start and check:

```powershell
C:\mcp\services\mcpctl.ps1 start tunnel; C:\mcp\services\mcpctl.ps1 start whatsapp; C:\mcp\services\mcpctl.ps1 status
```

The whatsapp service runs four processes: the bridge, the server, the gateway and the relay.

## 7. Verify, then connect

- `curl.exe -s -o NUL -w "%{http_code}" https://a.<domain>/mcp` returns **401**, because no login was given.
- `https://d.<domain>/` returns **302** to the Access login.
- `C:\mcp\venv\Scripts\python.exe C:\mcp\tools\external_check.py a.<domain>` passes.

Then the user adds the connector:
- **claude.ai:** Settings > Connectors > Add custom connector, with URL `https://a.<domain>/mcp`. Leave the OAuth
  fields empty, sign in with an allowed email and its PIN, then call the `health` tool.
- **ChatGPT:** first add ChatGPT's callbacks to the connector's Access app (`docs\cloudflare.md`, step 4b). Then in
  Settings > Apps & Connectors > Advanced, turn on Developer mode. Create a connector with the same URL and OAuth,
  leaving the client ID and secret blank.

## 8. The app (optional)

Users install WA Relay from this repo's
[latest release](https://github.com/leecyrille/whatsapp-mcp-relay/releases/latest), then enter
`https://d.<domain>` on first run. It updates itself from GitHub Releases, and the update signature is checked before
anything installs. Building your own signed releases instead is covered in `whatsapp-app\README.md`.

## Keeping it working

- **WhatsApp error 405** means WhatsApp rejected the client's version. Update the library with
  `go get go.mau.fi/whatsmeow@latest`, then `go mod tidy`, test, rebuild and restart the service.
- **Logs:**
  - `C:\mcp\logs\`
  - each server's `calls.log`: every tool call, with who made it
  - `whatsapp\sends.log`: every message sent
- **Re-linking:** run `Pair-WhatsApp.cmd` again, then `mcpctl.ps1 start whatsapp`.
