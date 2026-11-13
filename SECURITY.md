# Security model

## What protects what

| Layer | What it stops |
|---|---|
| Cloudflare Tunnel, with the services bound to 127.0.0.1 | Any direct connection to the PC. No ports are open on the router or firewall |
| Cloudflare Access with an email PIN | Anyone who isn't on the allowed email list |
| Access Managed OAuth, with a fixed list of callbacks | Rogue OAuth clients. Only the listed callback URLs can register |
| `gateway/access_proxy.py` | A request that reaches the PC some other way. It verifies the Access JWT itself on every request: the signature, issuer, audience, expiry and an allowed email. It also applies per-tool email rules, and logs every call to `calls.log` |
| Server guards (`whatsapp/server/guards.py`) | Runaway sending. Sends are rate-limited (`whatsapp/limits.json`) and only go to existing chats or contacts. Each send is logged |
| The bridge's REST API on 127.0.0.1:18700 | The API is never routed through the tunnel |

## Prompt injection

Message text, names, media, contact cards and web pages are written by **other people**. Any of it can try to tell
the AI to forward messages, send something, or change a name. The server's instructions and every tool description
say this content is untrusted. Actions that matter need the user to have asked for them, and the AI is told to
confirm the recipient and wording first.

These are the best defenses available, but they don't guarantee anything. Keep the send limits low, don't allow
people you don't trust, and read `sends.log` now and then.

## Secrets: never in git

- `C:\mcp\.env`: Cloudflare API token, only if you used the scripts instead of the Cloudflare MCP.
- `C:\mcp\cloudflared\*.json`: tunnel credentials.
- `C:\mcp\secrets\`: the app update signing key, if you build your own releases.
- `whatsapp\src\whatsapp-bridge\store\`: the WhatsApp link (it's effectively a login) and all your messages.

`.gitignore` covers these. Use BitLocker on the PC. Saved chats in the WA Relay app aren't encrypted on disk.

## App updates

WA Relay installs updates from this repo's GitHub Releases only after checking their signature against the public key
built into the app. A tampered or unsigned download is refused.

## Reporting

Please report security issues through GitHub's private vulnerability reporting on this repo. Don't open a public
issue.
