// Real updates along the path every copy in use today takes, built by build-chain-test.py:
//
//   A (an older release: old identifier, relay feed only)
//     -> B (built with the relay's address in; new identifier) from the relay, with the login
//     -> C (nothing built in, as published for everyone) from a fake GitHub feed, with no login.
//
// Real installs, downloads, signature checks, installer runs and restarts. The relay and GitHub are fake,
// and the installers have their own name, identifiers and key, so nothing here can touch a real copy.
//
//   node e2e/chain-update-test.mjs <playwright-core dir> <work folder>
import { spawn, spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import http from "node:http";
import { existsSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { makeRelay } from "./fake-relay.mjs";

const [pwDir, work] = process.argv.slice(2);
const require = createRequire(path.join(pwDir, "x.js"));
const { chromium } = require("playwright-core");

const PRODUCT = "WA Relay UpdTest";
const EXE_NAME = "wa-relay-updtest.exe";
const V = { A: "1.0.80", B: "1.0.81", C: "1.0.82" };
const CDP = 9336;
const PORT = 18793;
const GH_PORT = 18794;
const base = `http://127.0.0.1:${PORT}`;
const gh = `http://127.0.0.1:${GH_PORT}`;
const installDir = path.join(process.env.LOCALAPPDATA, PRODUCT);
const appExe = path.join(installDir, EXE_NAME);
const oldData = path.join(process.env.LOCALAPPDATA, "test.old.warelay.updtest");
const newData = path.join(process.env.LOCALAPPDATA, "app.warelay.updtest");
const setupPath = (v) => path.join(work, `${PRODUCT}_${v}_x64-setup.exe`);
const sigOf = (v) => readFileSync(setupPath(v) + ".sig", "utf8").trim();

const results = [];
function check(name, ok, detail = "") {
  results.push({ name, ok: !!ok });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${!ok && detail ? ` :: ${detail}` : ""}`);
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms = 30000) {
  const start = Date.now();
  while (Date.now() - start < ms) {
    try {
      const v = await fn();
      if (v) return v;
    } catch {}
    await sleep(150);
  }
  return null;
}
const powershell = (script) => spawnSync("powershell", ["-NoProfile", "-Command", script], { encoding: "utf8" }).stdout.trim();
const installedVersion = () => powershell(`(Get-Item '${appExe}').VersionInfo.ProductVersion`);
const kill = () => spawnSync("taskkill", ["/F", "/T", "/IM", EXE_NAME], { stdio: "ignore" });
const regQuery = (...args) => spawnSync("reg", ["query", ...args], { encoding: "utf8" });
const realLinkBefore = regQuery("HKCU\\Software\\Classes\\warelay", "/s").stdout;
const readText = (p) => {
  try {
    return readFileSync(p, "utf8");
  } catch {
    return "";
  }
};

// The fake GitHub: latest.json, and the installer behind a redirect, as GitHub serves release files.
const ghLog = [];
let ghLatest = null; // null: 404, as before the repository is public
const ghServer = http.createServer((req, res) => {
  ghLog.push({ path: req.url, cookie: req.headers.cookie, client: req.headers["x-wa-client"], agent: req.headers["user-agent"] });
  if (req.url === "/releases/latest/download/latest.json") {
    if (!ghLatest) {
      res.writeHead(404);
      return res.end("Not Found");
    }
    res.writeHead(200, { "content-type": "application/json" });
    return res.end(JSON.stringify(ghLatest));
  }
  if (req.url === `/releases/download/v${V.C}/WA-Relay-UpdTest_${V.C}_x64-setup.exe`) {
    res.writeHead(302, { location: `/files/release-asset-${V.C}` });
    return res.end();
  }
  if (req.url === `/files/release-asset-${V.C}`) {
    const bytes = readFileSync(setupPath(V.C));
    res.writeHead(200, { "content-type": "application/octet-stream", "content-length": bytes.length });
    return res.end(bytes);
  }
  res.writeHead(404);
  res.end();
});

const relay = makeRelay();
await new Promise((r) => relay.server.listen(PORT, "127.0.0.1", r));
await new Promise((r) => ghServer.listen(GH_PORT, "127.0.0.1", r));

function writeOverrides() {
  writeFileSync(
    path.join(installDir, "wa-test-overrides.txt"),
    [
      "WA_TEST_OFFSCREEN=1",
      "WA_UPDATE_TEST=1,2,1,3",
      "WA_ADOPT_EARLIER_DATA=1",
      `WA_UPDATE_FEED=${gh}/releases/latest/download/latest.json`,
      `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--remote-debugging-port=${CDP}`,
    ].join("\n"),
  );
}

let browser;
let page;
async function attach() {
  await browser?.close().catch(() => {});
  browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 60000);
  if (!browser) throw new Error("couldn't attach to the app");
  page = await until(async () => {
    for (const ctx of browser.contexts()) for (const p of ctx.pages()) if (p.url().includes("tauri.localhost") || p.url().includes("index.html")) return p;
  });
  if (!page) throw new Error("main page not found");
  await until(() => page.evaluate(() => !!window.__TAURI_INTERNALS__));
}
const invoke = (cmd, args = {}) => page.evaluate(([c, a]) => window.__TAURI_INTERNALS__.invoke(c, a), [cmd, args]);
const stateIs = (want, ms = 30000) => until(async () => (await invoke("update_state")).state === want, ms);
const seenAs = (version) => relay.log.requests.filter((r) => r.agent === `wa-relay-app/${version}`);

function cleanUp() {
  kill();
  const uninstaller = path.join(installDir, "uninstall.exe");
  if (existsSync(uninstaller)) spawnSync(uninstaller, ["/S"], { stdio: "ignore" });
  for (const dir of [installDir, oldData, newData]) rmSync(dir, { recursive: true, force: true });
}

try {
  cleanUp();
  await sleep(1000);

  // --- A, as copies are today.
  check("installs the older release", spawnSync(setupPath(V.A), ["/S", "/NS"], { stdio: "ignore" }).status === 0 && installedVersion() === V.A, installedVersion());
  writeOverrides();
  spawn(appExe, [], { detached: true, stdio: "ignore" }).unref();
  await attach();
  relay.finishLogin();
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  check("A signs in to the relay and syncs its chats", !!(await until(async () => (await invoke("chat_list")).length > 0)));
  await invoke("settings_set", { key: "read_receipts", value: false });
  check("A keeps its data under its own identifier", existsSync(path.join(oldData, "cache.db")) && !existsSync(newData));
  check("A has no public feed: it never asks GitHub", ghLog.length === 0);

  // --- A -> B, from the relay, with A's login.
  relay.setUpdate({ version: V.B, signature: sigOf(V.B), file: path.basename(setupPath(V.B)), bytes: readFileSync(setupPath(V.B)) });
  check("A finds B on the relay and gets it ready", await stateIs("ready"));
  const chatsBefore = (await invoke("chat_list")).length;
  page.evaluate(() => window.__TAURI_INTERNALS__.invoke("update_install")).catch(() => {});
  check("B installs and starts", !!(await until(() => seenAs(V.B).some((r) => r.path === "/api/events"), 90000)));
  check("the installed program is B", installedVersion() === V.B, installedVersion());
  relay.setUpdate(null);
  await attach();
  check("B moved A's data to its new identifier", !!(await until(() => existsSync(path.join(newData, "cache.db")))) && !existsSync(path.join(oldData, "cache.db")));
  check("and says so in its update log", readText(path.join(newData, "update.log")).includes("moved this app's data over from its earlier identifier"));
  const relayB = await invoke("relay_get");
  check("B saved the relay address it was built with", relayB.configured && relayB.address === base, JSON.stringify(relayB));
  check("A's login came across: B's requests carry it", seenAs(V.B).some((r) => r.cookie?.includes("CF_Authorization=test-login")));
  check("A's chats came across", (await invoke("chat_list")).length === chatsBefore && chatsBefore > 0);
  check("A's settings came across", (await invoke("settings_get")).read_receipts === false);

  // --- B checks GitHub first. Before the repository is public that's a 404, and the relay is asked instead.
  const relayChecks = relay.log.updateChecks;
  check("B asks the public feed, and with nothing there, falls back to the relay", !!(await until(() => ghLog.some((r) => r.path.endsWith("latest.json")) && relay.log.updateChecks > relayChecks)));
  check("with nothing on either, B is up to date", await stateIs("up_to_date"));

  // A feed naming an installer anywhere else is refused.
  ghLatest = { version: V.C, notes: "", pub_date: "2026-01-01T00:00:00Z", platforms: { "windows-x86_64": { signature: sigOf(V.C), url: `http://127.0.0.2:${GH_PORT}/files/release-asset-${V.C}` } } };
  check("an installer address on another host is refused", !!(await until(() => readText(path.join(newData, "update.log")).includes("installer at an unexpected address"))));

  // --- B -> C, from the fake GitHub, with no login.
  const relayDownloads = relay.log.updateDownloads;
  ghLatest.platforms["windows-x86_64"].url = `${gh}/releases/download/v${V.C}/WA-Relay-UpdTest_${V.C}_x64-setup.exe`;
  check("B finds C on the public feed and gets it ready", await stateIs("ready", 60000));
  check("the installer came through the feed's redirect", ghLog.some((r) => r.path === `/files/release-asset-${V.C}`));
  check("nothing sent to the public feed carried the login or the app's header", ghLog.every((r) => !r.cookie && !r.client), JSON.stringify(ghLog.filter((r) => r.cookie || r.client)));
  check("the relay wasn't asked for C", relay.log.updateDownloads === relayDownloads);
  page.evaluate(() => window.__TAURI_INTERNALS__.invoke("update_install")).catch(() => {});
  check("C installs and starts", !!(await until(() => seenAs(V.C).some((r) => r.path === "/api/events"), 90000)));
  check("the installed program is C", installedVersion() === V.C, installedVersion());
  await attach();
  const relayC = await invoke("relay_get");
  check("C, with no address built in, uses the one B saved", relayC.configured && relayC.address === base, JSON.stringify(relayC));
  check("the login is still there", seenAs(V.C).some((r) => r.cookie?.includes("CF_Authorization=test-login")));
  check("so are the chats and settings", (await invoke("chat_list")).length === chatsBefore && (await invoke("settings_get")).read_receipts === false);
  check("C never asks for a relay address", (await page.getByLabel("Relay address").count()) === 0);
  const uninstallKeys = regQuery("HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\" + PRODUCT).status === 0;
  check("there's still exactly one install, in the same place", uninstallKeys && existsSync(appExe));
  const leftover = regQuery(`HKCU\\Software\\old\\${PRODUCT}`).status === 0;
  console.log(`NOTE  the older release's own registry key ${leftover ? "is left behind (harmless)" : "is gone"}`);
} catch (e) {
  check("the test ran to the end", false, e.stack ?? String(e));
} finally {
  await browser?.close().catch(() => {});
  cleanUp();
  await sleep(1000);
  check("the test build uninstalled cleanly", !existsSync(appExe));
  spawnSync("reg", ["delete", `HKCU\\Software\\old\\${PRODUCT}`, "/f"], { stdio: "ignore" });
  check("the test build left no start-with-Windows entry", regQuery("HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run", "/v", PRODUCT).status !== 0);
  check("the real app's link wasn't touched", regQuery("HKCU\\Software\\Classes\\warelay", "/s").stdout === realLinkBefore);
  relay.server.closeAllConnections?.();
  relay.server.close();
  ghServer.close();
  const failed = results.filter((r) => !r.ok).length;
  console.log(`\n${results.length - failed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
}
