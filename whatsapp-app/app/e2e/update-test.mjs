// A real update between two real installers, built by build-update-test.py: real install, real download,
// real signature check, real installer run, real restart. Only the relay is fake. The installers have
// their own name and ID, so nothing here can touch a real copy of the app.
//
//   node e2e/update-test.mjs <playwright-core dir> <work folder>
import { spawn, spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { existsSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { makeRelay } from "./fake-relay.mjs";

const [pwDir, work] = process.argv.slice(2);
const require = createRequire(path.join(pwDir, "x.js"));
const { chromium } = require("playwright-core");

const PRODUCT = "WA Relay UpdTest";
const EXE_NAME = "wa-relay-updtest.exe";
const OLD = "1.0.90";
const NEW = "1.0.91";
const CDP = 9334;
const PORT = 18791;
const IDLE_SECS = 3;
const base = `http://127.0.0.1:${PORT}`;
const installDir = path.join(process.env.LOCALAPPDATA, PRODUCT);
const appExe = path.join(installDir, EXE_NAME);
const data = path.join(work, "data");
const setupName = (v) => `${PRODUCT}_${v}_x64-setup.exe`;
const setupPath = (v) => path.join(work, setupName(v));
const sigOf = (v) => readFileSync(setupPath(v) + ".sig", "utf8").trim();

const SCHEME = "warelaytest";
const linkKey = `HKCU\\Software\\Classes\\${SCHEME}`;
const regQuery = (...args) => spawnSync("reg", ["query", ...args], { encoding: "utf8" });
const realLinkBefore = regQuery("HKCU\\Software\\Classes\\warelay", "/s").stdout;

const results = [];
function check(name, ok, detail = "") {
  results.push({ name, ok: !!ok });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${!ok && detail ? ` :: ${detail}` : ""}`);
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms = 20000) {
  const start = Date.now();
  while (Date.now() - start < ms) {
    try {
      const v = await fn();
      if (v) return v;
    } catch {}
    await sleep(100);
  }
  return null;
}

const relay = makeRelay();
await new Promise((r) => relay.server.listen(PORT, "127.0.0.1", r));

function powershell(script) {
  return spawnSync("powershell", ["-NoProfile", "-Command", script], { encoding: "utf8" }).stdout.trim();
}
const pids = () => powershell(`(Get-Process wa-relay-updtest -ErrorAction SilentlyContinue | ForEach-Object Id) -join ','`);
const installedVersion = () => powershell(`(Get-Item '${appExe}').VersionInfo.ProductVersion`);
// How many of the app's main windows are visible. Windows' own "main window" property is no use here,
// since it also reports hidden helper windows.
const visibleWindows = () =>
  powershell(`
Add-Type -TypeDefinition 'using System;using System.Text;using System.Runtime.InteropServices;public class W{public delegate bool P(IntPtr h,IntPtr l);[DllImport("user32")]public static extern bool EnumWindows(P p,IntPtr l);[DllImport("user32")]public static extern bool IsWindowVisible(IntPtr h);[DllImport("user32")]public static extern int GetWindowThreadProcessId(IntPtr h,out int p);[DllImport("user32",CharSet=CharSet.Unicode)]public static extern int GetWindowText(IntPtr h,StringBuilder s,int n);public static int Count(int[] ids,string title){int n=0;EnumWindows((h,l)=>{int p;GetWindowThreadProcessId(h,out p);var s=new StringBuilder(64);GetWindowText(h,s,64);if(Array.IndexOf(ids,p)>=0&&IsWindowVisible(h)&&s.ToString()==title)n++;return true;},IntPtr.Zero);return n;}}';
[W]::Count([int[]]@(Get-Process wa-relay-updtest -ErrorAction SilentlyContinue | ForEach-Object Id),'WA Relay')`);
const updateLog = () => {
  try {
    return readFileSync(path.join(data, "update.log"), "utf8");
  } catch {
    return "";
  }
};
const agentSeen = (version) => relay.log.requests.some((r) => r.path === "/api/events" && r.agent === `wa-relay-app/${version}`);

function kill() {
  spawnSync("taskkill", ["/F", "/T", "/IM", EXE_NAME], { stdio: "ignore" });
}

// /NS skips shortcuts, so a test never adds anything to the desktop or Start menu.
function manualInstall(version) {
  return spawnSync(setupPath(version), ["/S", "/NS"], { stdio: "ignore" }).status;
}

function writeOverrides(hidden, extra = []) {
  const lines = [
    `WA_RELAY_URL=${base}`,
    `WA_DATA_DIR=${data}`,
    "WA_TEST_OFFSCREEN=1",
    `WA_UPDATE_TEST=1,2,1,${IDLE_SECS}`,
    `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--remote-debugging-port=${CDP}`,
  ];
  if (hidden) lines.push("WA_START_HIDDEN=1");
  lines.push(...extra);
  writeFileSync(path.join(installDir, "wa-test-overrides.txt"), lines.join("\n"));
}

let browser;
let page;
async function attach() {
  await browser?.close().catch(() => {});
  browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 40000);
  if (!browser) throw new Error("couldn't attach to the app");
  page = await until(async () => {
    for (const ctx of browser.contexts()) for (const p of ctx.pages()) if (p.url().includes("tauri.localhost") || p.url().includes("index.html")) return p;
  });
  if (!page) throw new Error("main page not found");
  await until(() => page.evaluate(() => !!window.__TAURI_INTERNALS__));
}
const invoke = (cmd, args = {}) => page.evaluate(([c, a]) => window.__TAURI_INTERNALS__.invoke(c, a), [cmd, args]);
const state = () => invoke("update_state");
const stateIs = (want, ms = 20000) => until(async () => (await state()).state === want, ms);
const pageCount = () => browser.contexts().reduce((n, c) => n + c.pages().length, 0);

async function launch(hidden) {
  writeOverrides(hidden);
  spawn(appExe, [], { detached: true, stdio: "ignore" }).unref();
  await attach();
}

const genuine = () => ({ version: NEW, signature: sigOf(NEW), file: setupName(NEW), bytes: readFileSync(setupPath(NEW)) });
const logLinesWith = (text) => updateLog().split("\n").filter((l) => l.includes(text)).length;

async function expectRefused(label, offer, why) {
  const before = logLinesWith("refused:");
  relay.setUpdate(offer);
  const refused = await until(() => logLinesWith("refused:") > before);
  const line = updateLog().split("\n").filter((l) => l.includes("refused:")).pop() ?? "";
  check(`${label}: refused`, refused && why.test(line), line);
  check(`${label}: nothing staged, same version, still running`, !updateLog().includes("downloaded and verified") && (await state()).current === OLD && pids() !== "");
}

try {
  rmSync(data, { recursive: true, force: true });

  // --- A login that's missing or lapsed must not break anything or nag.
  check("installs the older build", manualInstall(OLD) === 0 && existsSync(appExe));
  check("older build reports its version", installedVersion() === OLD, installedVersion());
  await launch(false);
  check("a normal start shows its window, so the hidden checks below mean something", visibleWindows() === "1", visibleWindows());
  check("with no login, the update check waits quietly", await stateIs("need_login"));
  check("with no login, nothing was requested", relay.log.updateChecks === 0 && relay.log.updateDownloads === 0);
  check("with no login, no sign-in window opens by itself", pageCount() === 1, String(pageCount()));

  relay.finishLogin();
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  check("signing in triggers a check, with nothing to install", await stateIs("up_to_date"));
  const authed = relay.log.requests.find((r) => r.path === "/api/update/latest");
  check("the check uses the app's login and client header", authed?.cookie?.includes("CF_Authorization=test-login") && authed?.client === "1" && !authed?.origin);

  relay.setRelogin(true);
  check("login lapsing later is quiet too", await stateIs("need_login"));
  check("login lapsing opens no window", pageCount() === 1, String(pageCount()));
  relay.setRelogin(false);
  check("checks resume when the login is back", await stateIs("up_to_date", 40000));

  relay.setUpdate({ ...genuine(), lapse: true });
  check("login lapsing during the download is quiet too", await stateIs("need_login"));
  check("a redirect during the download isn't followed", /download failed: .*302/.test(updateLog()), updateLog());
  relay.setUpdate(null);

  // --- Only a genuine update for that exact version gets through.
  const flipped = Buffer.from(readFileSync(setupPath(NEW)));
  flipped[flipped.length >> 1] ^= 1;
  await expectRefused("tampered installer", { ...genuine(), bytes: flipped }, /signature/i);
  await expectRefused("signed by another key", { ...genuine(), signature: readFileSync(path.join(work, "new-wrong-key.sig"), "utf8").trim() }, /key|signature/i);
  await expectRefused("old genuine installer announced as newer", { version: "1.0.95", signature: sigOf(OLD), file: setupName(OLD), bytes: readFileSync(setupPath(OLD)) }, /version/i);
  await expectRefused("signature without a version", { ...genuine(), signature: readFileSync(path.join(work, "new-no-version.sig"), "utf8").trim() }, /version/i);
  check("the refused state says so in Settings", (await state()).state === "refused" || (await stateIs("refused")));
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  check("Settings shows the refusal", await until(() => page.getByText(/failed its signature check/).isVisible()));

  const downloads = relay.log.updateDownloads;
  relay.setUpdate({ version: OLD, signature: sigOf(OLD), file: setupName(OLD), bytes: readFileSync(setupPath(OLD)) });
  await stateIs("up_to_date");
  await sleep(3000);
  check("the same or an older version isn't downloaded", relay.log.updateDownloads === downloads);

  // --- The switch. Off only stops installing by itself: it still checks, and offers the update.
  const toggle = page.getByLabel(/Install updates automatically/);
  check("automatic updates are on by default", await toggle.isChecked());
  await toggle.uncheck();
  relay.setUpdate(genuine());
  check("turned off, it still checks and downloads by itself", await stateIs("ready"));
  check("the off switch is remembered", (await invoke("settings_get")).auto_update === false);
  check("turned off, Settings says it's available, with an Install button", !!(await until(() => page.getByRole("button", { name: "Install update" }).isVisible())) && (await page.getByText(new RegExp(`Version ${NEW.replace(/\./g, "\\.")} is available`)).isVisible()));
  await page.screenshot({ path: path.join(work, "update-available.png") });
  await toggle.check();
  check("turned back on, the button says Restart to update", !!(await until(() => page.getByRole("button", { name: "Restart to update" }).isVisible())));

  // --- A ready update never interrupts an open window.
  const before = pids();
  await sleep((IDLE_SECS * 3 + 1) * 1000);
  check("with the window open, a ready update waits", pids() === before && (await state()).current === OLD && logLinesWith("installing 1") === 0);
  check("it shows as a quiet dot, and a button in Settings", (await page.locator(".rail-btn .dot").isVisible()) && (await page.getByRole("button", { name: "Restart to update" }).isVisible()));
  await page.screenshot({ path: path.join(work, "update-ready.png") });

  // --- Something unsent is listed, and the update says it waits for it. The button keeps it through the update.
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await until(async () => (await page.locator(".side .row").count()) > 0);
  await page.locator(".side .row").first().click();
  const draftChat = await page.locator(".convo .title .name").innerText();
  const box = page.locator("textarea").first();
  await box.fill("A made-up draft kept through the update");
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const listed = page.locator(".unsent li", { hasText: "A made-up draft kept through the update" });
  check("the draft is listed in Settings, with its chat", !!(await until(() => listed.isVisible())) && (await listed.innerText()).includes(draftChat));
  check("Settings says the update waits for it, and that updating keeps it", (await page.getByText(/so the update waits/).isVisible()) && (await page.getByText(/Updating now keeps them/).isVisible()));
  check("it holds back an automatic update", (await state()).held === true);
  const refused = await invoke("update_install", { keepUnsent: false }).then(() => "installed", (e) => String(e));
  check("without the user going ahead, the install refuses", /hasn't been sent/.test(refused), refused);
  relay.setClaudeRunning(true);
  await page.getByRole("button", { name: "Restart to update" }).click();
  check("Restart waits for a running Claude job", await until(() => page.getByRole("alert").getByText(/Claude is still working/).isVisible()));
  check("after refusing, the update is still ready and nothing restarted", (await state()).state === "ready" && pids() === before);
  relay.setClaudeRunning(false);

  // --- A real update, by the button, with a sidebar width and a silence to carry over.
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await page.locator(".side .resize").focus();
  await page.keyboard.press("ArrowRight");
  await page.keyboard.press("ArrowRight");
  await page.keyboard.press("ArrowRight");
  const sideWidth = () => page.locator(".side").evaluate((s) => Math.round(s.getBoundingClientRect().width));
  const widthBefore = await sideWidth();
  await invoke("silence_set", { minutes: 240 });
  const silencedUntil = (await invoke("silence_state")).until_ms;
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByRole("button", { name: "Restart to update" }).click();
  check("the button installs it and the new version comes back", await until(() => agentSeen(NEW), 60000));
  check("the new process is a different one", pids() !== before && pids() !== "");
  check("the installed program is the new version", installedVersion() === NEW, installedVersion());
  check("the log records the install, and a visible restart", updateLog().includes(`installing ${NEW}, show=true`) && updateLog().includes(`started ${NEW}, hidden=false`), updateLog());
  check("the button's restart shows the window", (await until(() => visibleWindows() === "1")) !== null, visibleWindows());
  check("the update was only downloaded once", logLinesWith(`${NEW} downloaded and verified`) === 1, updateLog());
  await attach();
  check("the app reports the new version", (await state()).current === NEW);
  check("saved chats survived the update", (await until(async () => (await invoke("chat_list")).length > 0)) !== null);
  check("the login survived the update", relay.log.requests.some((r) => r.agent === `wa-relay-app/${NEW}` && r.cookie?.includes("CF_Authorization=test-login")));
  check("the off/on setting survived the update", (await invoke("settings_get")).auto_update === true);
  check("a silence survived the update, with the same end time", (await invoke("silence_state")).until_ms === silencedUntil && silencedUntil > 0);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const keptDraft = page.locator(".unsent li", { hasText: "A made-up draft kept through the update" });
  check("the unsent draft survived the update", !!(await until(() => keptDraft.isVisible())));
  await keptDraft.getByRole("button", { name: "Open chat" }).click();
  check("and it's back in its chat's box", !!(await until(async () => (await page.locator("textarea").first().inputValue()) === "A made-up draft kept through the update")));
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await keptDraft.getByRole("button", { name: "Discard…" }).click();
  await keptDraft.getByRole("button", { name: "Yes, discard" }).click();
  check("once discarded, nothing holds the next update back", !!(await until(async () => (await keptDraft.count()) === 0 && (await state()).held === false)));
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  check("the sidebar width survived the update", widthBefore === 400 && (await until(async () => (await sideWidth()) === widthBefore)) !== null, String(await sideWidth()));
  await invoke("silence_clear");
  check("the same version isn't offered again", await stateIs("up_to_date"));
  check("only one copy is running after the update", pids().split(",").length === 1, pids());

  // --- A manual install still works, in either direction, keeping everything.
  kill();
  await sleep(1000);
  relay.setUpdate(null);
  check("a manual install over the new version works", manualInstall(OLD) === 0 && installedVersion() === OLD, installedVersion());

  // --- Hidden in the tray: installs by itself, but only when nothing's in progress, and stays hidden.
  await launch(true);
  check("started hidden", visibleWindows() === "0", visibleWindows());
  relay.setClaudeRunning(true);
  relay.setUpdate(genuine());
  check("hidden: downloads by itself", await stateIs("ready"));
  const hiddenPids = pids();
  await sleep((IDLE_SECS * 3 + 1) * 1000);
  check("hidden: waits while a Claude job runs", pids() === hiddenPids && logLinesWith(`installing ${NEW}, show=false`) === 0);
  await invoke("update_set_busy", { busy: true });
  relay.setClaudeRunning(false);
  await sleep((IDLE_SECS * 2 + 1) * 1000);
  check("hidden: waits while something's in progress", pids() === hiddenPids && logLinesWith(`installing ${NEW}, show=false`) === 0);
  const seenBefore = relay.log.requests.length;
  await invoke("update_set_busy", { busy: false });
  check("hidden: installs by itself once it's clear", await until(() => relay.log.requests.slice(seenBefore).some((r) => r.path === "/api/events" && r.agent === `wa-relay-app/${NEW}`), 60000));
  check("hidden: the installed program is the new version", installedVersion() === NEW, installedVersion());
  check("hidden: restarts hidden, without popping up", updateLog().includes(`started ${NEW}, hidden=true`) && visibleWindows() === "0", `${visibleWindows()}\n${updateLog()}`);

  // --- Turned off and hidden: Check now can stage it, but it won't install by itself.
  kill();
  await sleep(1000);
  relay.setUpdate(null);
  manualInstall(OLD);
  await launch(true);
  await invoke("settings_set", { key: "auto_update", value: false });
  relay.setUpdate(genuine());
  check("off and hidden: it still finds it by itself", await stateIs("ready"));
  const offPids = pids();
  await sleep((IDLE_SECS * 3 + 1) * 1000);
  check("off and hidden: it doesn't install by itself", pids() === offPids && (await state()).current === OLD);

  // --- Over an older copy by hand, which is what a PC on 1.0.0 has to do once.
  kill();
  await sleep(1000);
  relay.setUpdate(null);
  check("a manual install over an older copy works", manualInstall(NEW) === 0 && installedVersion() === NEW, installedVersion());
  await launch(false);
  check("after a manual install it runs the new version", (await state()).current === NEW);
  check("after a manual install, chats and login are still there", (await until(async () => (await invoke("chat_list")).length > 0)) !== null && relay.log.requests.at(-1)?.cookie?.includes("CF_Authorization"));

  // --- What a clicked notification does: Windows opens the app's link, through the registry. The toast
  // itself isn't shown or clicked here, since no test may put one on this PC's screen.
  const registered = regQuery(`${linkKey}\\shell\\open\\command`, "/ve");
  check("the installer registers the link to the installed program", registered.status === 0 && registered.stdout.toLowerCase().includes(appExe.toLowerCase()) && registered.stdout.includes('"%1"'), registered.stdout);
  const chatLink = (jid) => `${SCHEME}://open?chat=${encodeURIComponent(jid)}`;
  const openLink = (url) => powershell(`Start-Process '${url}'`);
  const chatShown = (name) => until(async () => (await page.locator(".convo header").innerText()).includes(name), 15000);
  kill();
  await sleep(1000);
  await launch(true);
  await until(async () => (await invoke("chat_list")).length > 0);
  const linkPids = pids();
  check("hidden in the tray before the click", visibleWindows() === "0", visibleWindows());
  openLink(chatLink(relay.DM2));
  check("hidden: a click shows the window", (await until(() => visibleWindows() === "1", 15000)) !== null, visibleWindows());
  check("hidden: a click opens that chat", await chatShown("Bob Plumber"));
  check("hidden: the running copy took it, and the second one left", (await until(() => pids() === linkPids, 10000)) !== null, pids());
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  openLink(chatLink(relay.GROUP));
  check("a click from another page opens that chat", await chatShown("Bingo Committee"));
  openLink(chatLink("19999999999@s.whatsapp.net"));
  check("a chat that isn't in the list opens nothing", await until(() => updateLog().includes("a chat not in the list"), 15000));
  await sleep(500);
  check("after that, the open chat hasn't changed", (await page.locator(".convo header").innerText()).includes("Bingo Committee"));
  for (const junk of [`${SCHEME}://open?chat=..%2F..%2Fx`, `${SCHEME}://nothing`, `${SCHEME}://open?chat=1%22%20--minimized`]) openLink(junk);
  await sleep(2500);
  check("bad links open nothing and break nothing", (await page.locator(".convo header").innerText()).includes("Bingo Committee") && pids() === linkPids && visibleWindows() === "1", pids());
  openLink(`${SCHEME}://claude`);
  check("a Claude approval's link opens the Claude page", await until(async () => (await page.getByRole("button", { name: "Claude", exact: true }).getAttribute("aria-current")) === "page", 15000));
  check("no address is written to the log", !updateLog().includes("15550000002") && !updateLog().includes("19999999999"));
  kill();
  await sleep(1000);
  writeOverrides(true);
  check("not running before the next click", pids() === "");
  openLink(chatLink(relay.GROUP));
  await attach();
  check("not running: a click starts it, and opens that chat", await chatShown("Bingo Committee"));
  check("not running: its window shows, whatever else says to start hidden", (await until(() => visibleWindows() === "1", 15000)) !== null, visibleWindows());
  check("not running: only one copy is left", pids().split(",").length === 1, pids());

  // --- With groups in their own column, a click shows the row in the right column, whatever tab it's on.
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await page.getByRole("button", { name: "Groups column" }).click();
  await until(() => page.locator(".column.people").isVisible());
  const peopleTabs = page.getByRole("tablist", { name: "Filter the people list by type" });
  await peopleTabs.getByRole("tab", { name: /^Unsorted/ }).click();
  await page.getByRole("tablist", { name: "Filter the groups list by type" }).getByRole("tab", { name: /^Unread/ }).click();
  openLink(chatLink(relay.GROUP));
  check("two columns: a click on a group's notification selects it in the Groups column", await until(async () => (await page.locator(".column.groups .row.on").count()) === 1 && (await page.locator(".column.groups .row.on .name").innerText()) === "Bingo Committee" && (await page.locator(".column.people .row.on").count()) === 0, 15000));
  openLink(chatLink(relay.DM2));
  check("two columns: a click on a person's notification selects them in the People column", await until(async () => (await page.locator(".column.people .row.on").count()) === 1 && (await page.locator(".column.people .row.on .name").innerText()) === "Bob Plumber" && (await page.locator(".column.groups .row.on").count()) === 0, 15000));
  await page.getByRole("button", { name: "Groups column" }).click();

  // --- The Silence button's link. It changes something, so it needs this app's own key.
  kill();
  await sleep(1000);
  await launch(true);
  await until(async () => (await invoke("chat_list")).length > 0);
  const silencePids = pids();
  const silenceLink = (await invoke("silence_state")).test_link;
  openLink(`${SCHEME}://silence?key=0123456789abcdef0123456789abcdef`);
  await until(() => updateLog().includes("had the wrong key"), 15000);
  check("a Silence link with the wrong key does nothing", updateLog().includes("had the wrong key") && (await invoke("silence_state")).until_ms === null && visibleWindows() === "0");
  const silenceAsked = Date.now();
  openLink(silenceLink);
  check("the Silence link silences for an hour", await until(async () => Math.abs(((await invoke("silence_state")).until_ms ?? 0) - silenceAsked - 3600000) < 20000, 15000));
  check("it doesn't bring up the window", visibleWindows() === "0" && pids() === silencePids, visibleWindows());
  await invoke("silence_clear");
  kill();
  await sleep(1000);
  openLink(silenceLink);
  await attach();
  check("with the app not running, it silences and stays in the tray", (await until(async () => (await invoke("silence_state")).until_ms !== null, 15000)) !== null && visibleWindows() === "0", visibleWindows());
  await invoke("silence_clear");

  // --- It survives a restart, then ends by itself. Set to run for 20 seconds in this build.
  kill();
  await sleep(1000);
  writeOverrides(true, ["WA_TEST_SILENCE_MS=20000"]);
  spawn(appExe, [], { detached: true, stdio: "ignore" }).unref();
  await attach();
  await invoke("silence_set", { minutes: 15 });
  const endsAt = (await invoke("silence_state")).until_ms;
  kill();
  await sleep(1000);
  spawn(appExe, [], { detached: true, stdio: "ignore" }).unref();
  await attach();
  const afterRestart = (await invoke("silence_state")).until_ms;
  check("a silence survives a restart", afterRestart === endsAt && Date.now() < endsAt, `${afterRestart} ${endsAt}`);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const silenceRow = page.getByRole("group", { name: "Silence notifications" });
  check("after the restart, Settings still says it's on", await until(() => silenceRow.getByText(/silenced until/).isVisible()));
  check("it ends by itself at the set time, and Settings catches up", await until(() => silenceRow.getByRole("button", { name: "1 hour" }).isVisible(), 45000) && Date.now() >= endsAt && (await invoke("silence_state")).until_ms === null);
} catch (e) {
  check("the test ran to the end", false, e.stack ?? String(e));
} finally {
  await browser?.close().catch(() => {});
  kill();
  await sleep(1000);
  const uninstaller = path.join(installDir, "uninstall.exe");
  if (existsSync(uninstaller)) spawnSync(uninstaller, ["/S"], { stdio: "ignore" });
  await until(() => !existsSync(appExe), 20000);
  check("the test build uninstalled cleanly", !existsSync(appExe));
  rmSync(installDir, { recursive: true, force: true });
  rmSync(path.join(process.env.LOCALAPPDATA, "app.warelay.updtest"), { recursive: true, force: true });
  const run = spawnSync("reg", ["query", "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run", "/v", PRODUCT], { encoding: "utf8" });
  check("the test build left no start-with-Windows entry", run.status !== 0);
  check("uninstalling removes the test build's link", regQuery(linkKey).status !== 0);
  check("the real app's link wasn't touched", regQuery("HKCU\\Software\\Classes\\warelay", "/s").stdout === realLinkBefore);
  relay.server.closeAllConnections?.();
  relay.server.close();
  const failed = results.filter((r) => !r.ok).length;
  console.log(`\n${results.length - failed} passed, ${failed} failed`);
  writeFileSync(path.join(work, "results.json"), JSON.stringify(results, null, 1));
  process.exit(failed ? 1 : 0);
}
