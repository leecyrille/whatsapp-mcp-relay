# WA Relay

A WhatsApp desktop app for every Windows PC you use, through one WhatsApp link on a home PC. WhatsApp sees a
single linked device, the bridge on the home PC. Every PC reaches that bridge through one hostname behind one
login, so WhatsApp's linked-device limit stops mattering, and history, groups and settings are shared.

```
Tauri app on any PC
  -> Cloudflare Access (email PIN, or any login Access supports)
  -> Cloudflare Tunnel
  -> wa-relay   127.0.0.1:8704   checks the Access token; the only way in
       -> whatsapp-bridge  127.0.0.1:18700   the one WhatsApp link, and the message store
       -> wa-runner        127.0.0.1:18705   optional: runs Claude jobs on the home PC, with approvals
```

| Piece | Where | Config |
|---|---|---|
| App | `app/` (Tauri 2, Rust, Svelte) | First run asks for the relay's address |
| Relay | `relay/cmd/wa-relay` | `relay/bin/relay.json`, from `config/relay.example.json` |
| Runner (optional) | `relay/cmd/wa-runner` | `relay/bin/runner.json`, from `config/runner.example.json` |
| Your groups | kept by the relay in `data/groups.json` | starts with Personal and Work; `config/groups.example.json` |
| Site details | `site.json` next to this folder, private | from `site.example.json` |
| Bridge | the WhatsApp bridge's `app_*.go` files, in the bridge's own folder | see the bridge's README |

None of the real config files belong in a repository: they hold your domain, login emails and paths.

## What it does

- Chats, groups, search across all saved messages, photos, video, voice notes, documents with previews, contact
  cards, mentions, replies, reactions, edits, deletes and forwarding, from any of your PCs.
- Each chat has one read marker on the home PC. Reading on the phone clears every PC, and reading on a PC clears the
  phone.
- Your own groups (tabs) on top of People and Groups, kept on the home PC and shared by every PC. An optional rule can
  put group chats whose name matches a pattern into one of them (`name_rule` in `relay.json`).
- The list is newest first. An unread chat out of view lights that edge of the list; a click scrolls to it.
- A group with no name shows its members' names, as WhatsApp does.
- Notifications, a tray icon, start with Windows, and quiet automatic updates.
- Optional: a Claude panel that runs jobs on the home PC, where every action that can change anything waits for your
  approval in the app, and Claude sorting chats into your groups. Both need `wa-runner` running on the home PC with
  Claude Code installed there. Without it, the panel says Claude can't be reached and everything else works.

**What's missing compared with WhatsApp:** calls, phones and tablets, Status, Channels and Communities, sending polls,
locations and stickers, typing and online indicators, and archive, pin and mute from the phone. Older history is only
what the phone sends at pairing, plus what you ask for chat by chat.

## Setting it up

You need a Windows home PC that stays on, running the WhatsApp bridge; a domain on Cloudflare; Go, Rust and Node.js.

1. **Site details.** Copy `site.example.json` to `site.json` next to this folder and fill in your domain, Access
   team, tunnel name, the host name for the app (for example `wa`, giving `wa.example.com`), and the emails allowed
   to sign in. `site.json` stays private.
2. **Cloudflare.** `python tools/cf_setup_app.py access` creates the Access application and writes its audience tag
   and allowed emails into `relay/bin/relay.json`. Add a tunnel route from the app's host name to
   `http://127.0.0.1:8704`, then `python tools/cf_setup_app.py dns`. Access goes first and the public route last, so
   the host name is never reachable without a login.
3. **Relay.** Copy `config/relay.example.json` to `relay/bin/relay.json`, fill in the paths, and leave out
   `name_rule` unless you want it. For the Claude panel, also copy `config/runner.example.json` to
   `relay/bin/runner.json`. Build both (below) and run them as services that start with Windows.
4. **Update signing key.** `npx tauri signer generate -w <path to your secrets folder>/wa-relay-update.key`. Put the
   public half in `app/src-tauri/tauri.conf.json` under `plugins.updater.pubkey`. Keep the private key out of every
   repository and every synced folder.
5. **Update feed.** Copies check `PUBLIC_FEED` in `app/src-tauri/src/update.rs` first. For your own fork, change it,
   or build with `WA_UPDATE_FEED` set to your own `.../releases/latest/download/latest.json`.
6. **Build and publish** the app with `tools/release.py` (below), and install it on each PC. The first run asks for
   your relay's address, then you sign in through Access once per PC.

### Build

```powershell
# Relay and runner. Stop the service first: Windows locks a running exe.
cd relay
go build -ldflags="-s -w" -o bin\wa-relay.exe .\cmd\wa-relay
go build -ldflags="-s -w" -o bin\wa-runner.exe .\cmd\wa-runner

# App, without publishing it
cd ..\app
npm install
npx tauri build
```

## Updates

**How a copy updates:** about a minute after starting, and then every 6 hours, it reads the public feed, the
`latest.json` of the newest GitHub release, with no login and no cookie. Only if that can't be reached does it ask
the relay, with its normal login. A new version downloads in the background and is checked, then waits until you
press Restart to update in Settings, or the window has been closed to the tray for 5 minutes with nothing in
progress. An open window, a running Claude job, or something you wrote that hasn't gone (a draft, a message that
didn't send, a file still uploading) holds it back. Settings lists everything unsent, in any chat, with Open chat,
Send now and Discard. Drafts and messages that didn't send are kept through a restart, so the button can update
past them. Something unsent stops holding the automatic update back once it's a day old or its chat has left the
list; it stays listed. A file the home PC already has never holds it back. Turning off "Install updates
automatically" in Settings only stops it installing by itself: it still checks, and Settings offers Install
update.

**What proves an update is genuine:** the installer isn't code-signed, so the update signature is the proof. The app
has the public key built in and refuses anything not signed by the private key for that exact file and version. An
installer from the public feed may only come from the feed's own host or GitHub's file store, and nothing from that
feed is ever sent the login cookie. From the relay, the app builds the download address itself.

**Publishing:** bump the version in `tauri.conf.json`, `Cargo.toml` and `package.json`, then:

```powershell
python tools/release.py                  # dist/, and a GitHub release once the repository is public
python tools/release.py --github dry-run # dist/, plus the GitHub files and the gh command it would run
```

It builds, signs, checks the signature against the app's public key, makes sure a tampered copy fails, publishes to
`dist/` for the relay, and then, with `gh`, creates the GitHub release: the installer, its `.sig`, and `latest.json`.
It builds with `--remap-path-prefix`, so source paths recorded in the program (which on Windows start with your user
folder) come out as `/cargo/...`, `/src/...` and so on. Before anything goes to `dist/` or GitHub it scans the built
program itself and its web files, not the compressed installer, as plain bytes and as UTF-16, for the names in
`site.json`, your Windows user name, your login emails' domains, and any patterns in an optional private
`secrets/public_denylist.txt` next to this folder; anything found stops the release. A harmless match inside
third-party data can be let through by putting the text around it in `scan-allow.private.txt`. For GitHub it also
checks the repository is public. The signing key stays on this PC, so there are no CI secrets.

**Step releases:** `python tools/release.py --bake-relay --step` builds with your relay's address built in and
publishes it to `dist/` only, never GitHub, marked as a step. The relay offers a step to every older copy before
anything newer, so each copy runs it once and saves the address. Use it when copies installed before the first-run
question need to keep working without anyone typing the address.

**Changing the app's identifier** moves its data folder. The first start under a new identifier adopts the data of
an earlier one ending in `.warelay`, logins and settings included (`app/src-tauri/src/setup.rs`).

## Security

- **Login at the origin too.** `wa-relay` checks the Access token on every request before any route runs: RS256
  from the team's keys, issuer, audience, expiry and an allowed email. It won't start on bad config.
- **Only listed routes.** The relay forwards a fixed list, so the bridge's other routes and the runner's approval
  endpoint can't be reached from outside.
- **Web pages can't use the login.** `/api/*` needs a custom header and refuses any `Origin`.
- **Claude's limits are in code.** `wa-runner` has no setting for permission mode and refuses to start if
  `allowed_tools` lists anything. Jobs load no settings files, get no MCP servers except the approval one, and
  `denied_tools` blocks reading secrets whatever is approved.
- **Forged messages.** Only a message's author can edit or delete it. A message ID can't reach into another chat.
- **Files from strangers.** Filenames are cleaned, including right-to-left spoofing. Saved and opened files carry the
  internet mark, so Protected View and SmartScreen still apply. Message text never renders as HTML, and document
  previews run in a sandboxed frame.
- **Your data on each PC** is an unencrypted SQLite cache under `%LOCALAPPDATA%\app.warelay.desktop`, plus media. Use
  BitLocker on laptops. Settings > Sign out and remove saved chats clears it. A lost PC's sign-in can be revoked in
  Cloudflare Access.

This is an unofficial linked device. WhatsApp can restrict a number that uses one. Sending by hand at normal rates
is the lowest-risk way to use it, but the risk isn't zero.

## Tests

```powershell
cd relay; go test ./...
cd app\src-tauri; cargo test --lib
```

End to end, with the real app against a fake relay, made-up data and an off-screen window:
`npx tauri build --debug --no-bundle`, then from `app/`:

- `node e2e/run.mjs <folder with playwright-core> <screenshot folder>`: the whole app.
- `node e2e/first-run.mjs <playwright-core> <screenshots>`: the first run's relay question.
- `node e2e/read-retry.mjs <playwright-core>`: reads that fail are kept and sent later.

Real updates, between real installers under a throwaway name and key, against a fake relay:

- `python e2e/build-update-test.py <work>`, then `node e2e/update-test.mjs <playwright-core> <work>`.
- `python e2e/build-chain-test.py <work> <an older release's app folder>`, then
  `node e2e/chain-update-test.mjs <playwright-core> <work>`: an older copy updating from the relay to a step
  release, which moves its data to the new identifier and saves the relay address, and then from a fake GitHub feed
  to a build with nothing built in.

Test builds use the `test-overrides` Cargo feature, which is what lets them reach a fake relay; `release.py` never
sets it. No test sends anything on WhatsApp.
