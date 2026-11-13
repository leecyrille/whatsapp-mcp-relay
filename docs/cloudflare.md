# Cloudflare setup through the Cloudflare MCP

These are the Cloudflare API calls to make, in order, with the Cloudflare MCP (`https://mcp.cloudflare.com/mcp`). Its
`search` tool finds endpoints and `execute` runs them. Every step is safe to repeat: look for the thing first, and
only create it if it's missing.

`tools\cf_setup.py whatsapp` and `whatsapp-app\tools\cf_setup_app.py` do the same with an API token, if you'd rather.

Values come from `C:\mcp\site.json`: `<domain>`, `<team>`, `<tunnel>`, and the hosts `a` and `d`. `<emails>` are the
emails the user allowed.

**Order matters:** sign-in and policy first, the public DNS record last. That way a hostname never answers without a
login in front of it.

## 1. Find the zone and account

`GET /zones?name=<domain>` returns `zone_id = result[0].id` and `account_id = result[0].account.id`. The zone's status
must be `active`.

## 2. Access organization and sign-in

1. `GET /accounts/{account_id}/access/organizations`. If there isn't one, create it with
   `POST` and `{"name": "<team>", "auth_domain": "<team>.cloudflareaccess.com"}`. If one exists already, use its
   `auth_domain`'s first label as `<team>` and put that in `site.json`.
2. `GET /accounts/{account_id}/access/identity_providers`. If none has `type: "onetimepin"`, create one with
   `POST` and `{"name": "Email PIN", "type": "onetimepin", "config": {}}`. Remember its `id` as `<idp>`.

## 3. Access app for the connector (`a.<domain>`)

1. Look for it: `GET /accounts/{account_id}/access/apps`, and find the app whose `domain` is `a.<domain>`.
2. If it's missing, `POST /accounts/{account_id}/access/apps` with:

   ```json
   {"name": "a", "domain": "a.<domain>", "type": "self_hosted", "session_duration": "720h",
    "allowed_idps": ["<idp>"], "auto_redirect_to_identity": true, "app_launcher_visible": false,
    "policies": [{"name": "a-allow", "decision": "allow",
                  "include": [{"email": {"email": "<email 1>"}}, {"email": {"email": "<email 2>"}}]}]}
   ```

## 4. Turn on Managed OAuth for the connector

Claude's connector signs in through OAuth, and Cloudflare Access is the OAuth server. `GET` the whole app, set the
`oauth_configuration` below, and `PUT` the whole object back. Keep any entries already in `allowed_uris`.

```json
"oauth_configuration": {
  "enabled": true,
  "dynamic_client_registration": {
    "enabled": true,
    "allowed_uris": ["https://claude.ai/api/mcp/auth_callback", "https://claude.com/api/mcp/auth_callback"],
    "allow_any_on_localhost": false,
    "allow_any_on_loopback": false
  },
  "grant": {"session_duration": "720h", "access_token_lifetime": "1h"}
}
```

**4b. ChatGPT (optional).** Add `https://chatgpt.com/connector/oauth/*` and
`https://chatgpt.com/connector_platform_oauth_redirect` to `allowed_uris`. Only a trailing `/*` wildcard works, and
entries must be HTTPS. `tools\cf_allow_chatgpt.py` does this.

**Record:**
- Set `team_domain` in `gateway\gateway.json` to the bare team name.
- Set `servers.whatsapp.aud` in `gateway\gateway.json` to the app's `aud`.

The gateway rejects any request whose Access token doesn't have this audience and an allowed email.

## 5. Access app for WA Relay (`d.<domain>`), optional

Same as step 3, with these differences:
- `"name": "d"` and `"domain": "d.<domain>"`.
- Add `"http_only_cookie_attribute": true` and `"same_site_cookie_attribute": "lax"`.
- The policy lists only the emails allowed to use the app.
- No Managed OAuth.

The app signs in through the browser and keeps its own Access token.

**Record:** set `team_domain`, `aud` and `allowed_emails` in `whatsapp-app\relay\bin\relay.json`.

## 6. The tunnel

Create it on the PC, so its secret never passes through chat or the MCP. The user does step 1; you run the rest.

1. The user runs `C:\mcp\toolchain\cloudflared\cloudflared.exe tunnel login` and picks the zone in the browser. This
   writes `cert.pem` to `%USERPROFILE%\.cloudflared\`.
2. Run `cloudflared tunnel create <tunnel>`. It writes `<tunnel-id>.json` to the same folder. Move that file to
   `C:\mcp\cloudflared\`.
3. Copy `cloudflared\config.example.yml` to `C:\mcp\cloudflared\config.yml`, then fill in the tunnel ID, the
   credentials path and your hostnames.

Check it with `cloudflared tunnel ingress validate --config C:\mcp\cloudflared\config.yml`.

## 7. DNS, last

Do this only after steps 3 to 6 are done. For each hostname, first check
`GET /zones/{zone_id}/dns_records?name=a.<domain>`. If there's no record, create it with
`POST /zones/{zone_id}/dns_records` and:

```json
{"type": "CNAME", "name": "a.<domain>", "content": "<tunnel-id>.cfargotunnel.com", "proxied": true}
```

Do the same for `d.<domain>` if you set up the app.

## 8. Check from outside

Start the services (AGENTS.md, stage 6). Then:
- `https://a.<domain>/mcp` with no login returns 401, with a `www-authenticate` header that points to OAuth.
- `/.well-known/oauth-authorization-server` names `https://<team>.cloudflareaccess.com`.
- Registering a client with an unknown callback is refused with 400.
- `https://d.<domain>/` returns 302 to the Access login.

`tools\external_check.py a.<domain>` runs these checks.
