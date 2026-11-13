// Breaks the real services and watches what the real app says. The app talks to the real relay code, which
// reads the real bridge, through the test front in relay/internal/relay/live_outage_test.go. That front only
// passes GET, so nothing can be sent or changed through it. Prints states and counts, never names or text,
// and takes no screenshots.
//
//   node e2e/real-outage.mjs <playwright-core dir> <mcpctl path>
import { spawn, spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { mkdtempSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";

const [pwDir, mcpctl] = process.argv.slice(2);
const require = createRequire(path.join(pwDir, "x.js"));
const { chromium } = require("playwright-core");

const FRONT = 18794;
const CDP = 9335;
const relayDir = path.resolve("../relay");
const results = [];
function check(name, ok, detail = "") {
  results.push({ name, ok: !!ok });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? ` :: ${detail}` : ""}`);
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms = 60000) {
  const start = Date.now();
  while (Date.now() - start < ms) {
    try {
      const v = await fn();
      if (v) return v;
    } catch {}
    await sleep(500);
  }
  return null;
}
const ctl = (...args) => spawnSync("powershell", ["-NoProfile", "-ExecutionPolicy", "Bypass", "-File", mcpctl, ...args], { encoding: "utf8" }).stdout;
const bridge = () => fetch("http://127.0.0.1:18700/api/app/status").then((r) => r.json());

const killFront = () => spawnSync("taskkill", ["/F", "/T", "/IM", "relay.test.exe"], { stdio: "ignore" });
killFront();
await until(() => fetch(`http://127.0.0.1:${FRONT}/login-ok`).then(() => false, () => true), 10000);
spawn(process.env.GO_EXE ?? "go", ["test", "-run", "TestLiveOutageFront", "-timeout", "30m", "./internal/relay"], { cwd: relayDir, env: { ...process.env, LIVE_OUTAGE_PORT: String(FRONT) }, stdio: "ignore" });
if (!(await until(() => fetch(`http://127.0.0.1:${FRONT}/login-ok`).then((r) => r.ok), 120000))) {
  console.log("FAIL  the test front didn't start");
  process.exit(1);
}

const data = mkdtempSync(path.join(os.tmpdir(), "wa-outage-"));
const app = spawn(path.resolve("src-tauri/target/debug/wa-relay-app.exe"), [], {
  env: { ...process.env, WA_RELAY_URL: `http://127.0.0.1:${FRONT}`, WA_DATA_DIR: data, WA_TEST_OFFSCREEN: "1", WA_SECOND_INSTANCE: "1", WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: `--remote-debugging-port=${CDP}` },
  stdio: "ignore",
});
let browser;
let stopped = false;
try {
  browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 30000);
  const page = await until(async () => browser.contexts().flatMap((c) => c.pages()).find((p) => p.url().includes("tauri.localhost")));
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  const invoke = (cmd, args = {}) => page.evaluate(([c, a]) => window.__TAURI_INTERNALS__.invoke(c, a), [cmd, args]);
  const link = async () => (await invoke("conn_state")).link;

  const reached = await until(async () => (await link()) === "ok", 120000);
  if (!reached) console.log("      windows:", JSON.stringify(await Promise.all(browser.contexts().flatMap((c) => c.pages()).map(async (p) => [new URL(p.url()).pathname, await p.title()]))), JSON.stringify(await invoke("conn_state")));
  check("real data: reaches ok through the real relay code", reached, await link());
  const tab = async (name) => Number(await page.getByRole("tab", { name }).locator(".chat-count").innerText());
  await until(async () => (await tab(/^All/)) > 0);
  const counts = { all: await tab(/^All/), people: await tab(/^People/), groups: await tab(/^Groups/) };
  check("real data: People and Groups add up to every chat", counts.all === counts.people + counts.groups && counts.people > 0 && counts.groups > 0, JSON.stringify(counts));
  console.log("      tabs:", JSON.stringify((await page.getByRole("tab").allInnerTexts()).map((s) => s.replace(/\s+/g, " "))));
  const beforeHead = (await bridge()).head;

  // The bridge really stops. The relay code stays up, so this is the real "relay up, bridge down".
  let t = Date.now();
  stopped = true;
  ctl("stop", "whatsapp");
  check("real bridge stopped: app says the WhatsApp service isn't running", await until(() => page.locator(".banner.down", { hasText: /WhatsApp service isn't running/ }).isVisible(), 40000));
  console.log(`      appeared ${Date.now() - t} ms after the stop command returned or began`);
  check("the state is bridge_down", (await link()) === "bridge_down", await link());
  const text = await page.locator(".banner").innerText();
  check("it says when things were last current", /last updated today at/.test(text));
  check("the dot stops saying up to date", (await page.locator(".status-dot.good").count()) === 0);
  const hits = await invoke("search", { query: "the", limit: 5 });
  check("offline search works with the bridge stopped", hits.length > 0);
  const stats = await invoke("cache_stats");
  check("saved chats are still there", stats.messages > 0, `${stats.messages > 0}`);
  await page.locator(".side .row").first().click();
  check("a saved chat still opens", await until(async () => (await page.locator(".bubble").count()) > 0));
  check("the composer warns", await until(() => page.locator(".offline-note").isVisible()));
  // Refused by the front whatever happens, so this can't reach WhatsApp.
  await page.locator("textarea").first().fill("x");
  await page.locator("textarea").first().press("Enter");
  check("a message sent now is marked Not sent", await until(() => page.locator(".pending.failed").getByText("Not sent.").isVisible()));
  await page.locator(".pending.failed").getByRole("button", { name: "Discard" }).click();

  t = Date.now();
  ctl("start", "whatsapp");
  stopped = false;
  check("real bridge back: warning clears by itself", await until(async () => (await link()) === "ok", 90000), await link());
  console.log(`      cleared ${Date.now() - t} ms after starting the service`);
  check("no warning left on screen", (await page.locator(".banner").count()) === 0);
  const status = await bridge();
  check("WhatsApp is linked and connected again", status.logged_in === true && status.connected === true);
  check("the app's cursor reached the bridge's head", await until(async () => (await invoke("conn_state")).link === "ok" && status.head >= beforeHead));
} catch (e) {
  check("the test ran to the end", false, e.stack ?? String(e));
} finally {
  if (stopped) ctl("start", "whatsapp");
  await browser?.close().catch(() => {});
  app.kill();
  killFront();
  await sleep(1000);
  try {
    rmSync(data, { recursive: true, force: true });
  } catch {}
  const failed = results.filter((r) => !r.ok).length;
  console.log(`${results.length - failed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
}
