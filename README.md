# WhatsApp MCP Relay

Your own WhatsApp connector for Claude (claude.ai, Claude Desktop, Claude Code) and ChatGPT, running on a Windows PC
at home. It also includes **WA Relay**, a desktop WhatsApp app for your other PCs.

- **Connector:** read, search and send WhatsApp messages from Claude or ChatGPT, with send limits and confirmations.
- **No open ports:** everything goes through a Cloudflare Tunnel, behind Cloudflare Access sign-in. Access issues the
  connector's OAuth login, and each request's Access token is checked again on the PC.
- **WA Relay app:** a fast desktop client with offline search. It talks to your home PC, so it doesn't use up another
  WhatsApp linked device. It **updates itself** from this repo's GitHub Releases.

## Set it up: let your agent do it

This repo is written so an AI coding agent (Claude Code, Codex and similar) can build the whole thing for you. Open
your agent on the Windows PC that will host it, and say:

> Set up the WhatsApp MCP from https://github.com/leecyrille/whatsapp-mcp-relay. Follow its AGENTS.md.

The agent does the work and stops to ask you only for the steps that need a person:
- connecting the **Cloudflare MCP**, so the agent can configure Cloudflare for you
- scanning the WhatsApp QR code
- signing in
- running one command as administrator

### What you need

- A Windows 10 or 11 PC that stays on.
- A domain on Cloudflare (the free plan is fine), for example `example.com`.
- WhatsApp on your phone.
- About an hour, mostly waiting for builds.

## Install just the app

If the home PC is already running, download the latest `WA Relay_x.y.z_x64-setup.exe` from
[Releases](https://github.com/leecyrille/whatsapp-mcp-relay/releases/latest). Run it, and when it asks, enter your
relay address, for example `https://d.example.com`. Updates install themselves from then on.

## How it fits together

```
Claude / ChatGPT  ->  https://a.<domain>/mcp  ->  Cloudflare Access (OAuth + email PIN)  ->  Tunnel
                  ->  gateway 127.0.0.1:8701 (re-checks the Access token, per-tool rules, call log)
                  ->  MCP server 127.0.0.1:18701  ->  whatsapp-bridge 127.0.0.1:18700  ->  WhatsApp

WA Relay app      ->  https://d.<domain>  ->  Cloudflare Access  ->  Tunnel  ->  wa-relay 127.0.0.1:8704  ->  bridge
```

| Folder | What it is |
|---|---|
| `whatsapp/src/whatsapp-bridge` | Go WhatsApp client (whatsmeow), linked to your phone as a device. Forked from [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp), MIT |
| `whatsapp/server` | The MCP server (Python, FastMCP) and its safety guards |
| `gateway` | Checks the Cloudflare Access token on every request before anything reaches the server |
| `whatsapp-app` | The WA Relay desktop app (Tauri) and its relay server (Go) |
| `services` | Windows scheduled tasks that keep everything running, and `mcpctl.ps1` to start, stop and check them |
| `tools` | Cloudflare setup scripts for people who'd rather use an API token than the Cloudflare MCP |

## Before you use it

- **This is unofficial.** It links to WhatsApp as a companion device through the open-source
  [whatsmeow](https://github.com/tulir/whatsmeow) library, not WhatsApp's official API. WhatsApp may restrict accounts
  that use unofficial clients, especially for bulk or automated messaging. Use it for your own messages, gently.
- **Message text is untrusted.** Anyone who messages you can try to steer an AI that reads your chats. The connector
  tells the AI so, confirms before sending, and rate-limits sends. Read [SECURITY.md](SECURITY.md) anyway.
- Your messages stay on your PC. Nothing goes through a third party except Cloudflare's tunnel and, when you use it,
  the AI you connect.

## License

MIT. See [LICENSE](LICENSE). The WhatsApp bridge keeps its original MIT license in `whatsapp/src/LICENSE`.
