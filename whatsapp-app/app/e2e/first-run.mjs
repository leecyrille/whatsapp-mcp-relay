// The first run of a copy with no relay built in: it asks for the relay's address, refuses bad ones, saves a
// good one and starts again on it. Debug build, fake relay, isolated data folder, off-screen window.
//
//   node e2e/first-run.mjs <playwright-core dir> <screenshot dir>
import { spawn, spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeRelay } from "./fake-relay.mjs";

const [pwDir, shotDir] = process.argv.slice(2);
const require = createRequire(path.join(pwDir, "x.js"));
const { chromium } = require("playwright-core");

const exe = path.resolve("src-tauri/target/debug/wa-relay-app.exe");
const data = mkdtempSync(path.join(os.tmpdir(), "wa-first-run-"));
const CDP = 9335;
const PORT = 18792;
const base = `http://127.0.0.1:${PORT}`;
mkdirSync(shotDir, { recursive: true });

const results = [];
function check(name, ok, detail = "") {
  results.push(ok);
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${!ok && detail ? ` :: ${detail}` : ""}`);
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms = 15000) {
  const start = Date.now();
  while (Date.now() - start < ms) {
    try {
      const v = await fn();
      if (v) return v;
    } catch {}
    await sleep(80);
  }
  return null;
}
const relayLog = () => fetch(`${base}/control/log`, { method: "POST" }).then((r) => r.json());

// Only this test's own copies, found by the debug build's path. Never anything else by name.
function stopOurs() {
  const target = exe.replace(/'/g, "''");
  spawnSync("powershell", ["-NoProfile", "-Command", `Get-Process | Where-Object { $_.Path -eq '${target}' } | Stop-Process -Force`]);
}
const ours = () =>
  spawnSync("powershell", ["-NoProfile", "-Command", `@(Get-Process | Where-Object { $_.Path -eq '${exe.replace(/'/g, "''")}' }).Count`], { encoding: "utf8" }).stdout.trim();

const { server } = makeRelay();
await new Promise((r) => server.listen(PORT, "127.0.0.1", r));
stopOurs();
const app = spawn(exe, [], {
  env: {
    ...process.env,
    WA_RELAY_URL: "", // no relay: the first run must ask
    WA_DATA_DIR: data,
    WA_TEST_OFFSCREEN: "1",
    WA_SECOND_INSTANCE: "1",
    WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: `--remote-debugging-port=${CDP}`,
  },
  stdio: "ignore",
});

async function attach() {
  const browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 40000);
  if (!browser) throw new Error("couldn't attach to the app's WebView");
  const page = await until(async () => {
    for (const ctx of browser.contexts()) for (const p of ctx.pages()) if (p.url().includes("tauri.localhost") || p.url().includes("index.html")) return p;
  });
  if (!page) throw new Error("main page not found");
  await page.setViewportSize({ width: 1000, height: 700 }).catch(() => {});
  return { browser, page };
}

let browser;
const pageErrors = [];
try {
  let page;
  ({ browser, page } = await attach());
  page.on("pageerror", (e) => pageErrors.push(e.message));

  const field = page.getByLabel("Relay address");
  check("with no relay, the first run asks for one", !!(await until(() => field.isVisible())));
  check("and doesn't offer to sign in yet", (await page.getByRole("button", { name: "Sign in", exact: true }).count()) === 0);
  check("nothing has been asked of any relay", (await relayLog()).requests.every((r) => r.path.startsWith("/control/")));
  const setup = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("relay_get"));
  check("the app says it has no relay", setup.configured === false && setup.address === "", JSON.stringify(setup));
  await page.screenshot({ path: path.join(shotDir, "50-first-run-relay.png") });

  const go = page.getByRole("button", { name: "Continue" });
  check("Continue waits for an address", await go.isDisabled());
  const problem = page.getByRole("alert");
  for (const [bad, why] of [
    ["ftp://relay.example.com", /https:\/\//],
    ["https://relay.example.com/some/path", /nothing after the host name/],
    ["https://user:secret@relay.example.com", /user name or password/],
    ["not an address at all", /web address|host name/],
  ]) {
    await field.fill(bad);
    await go.click();
    check(`refuses ${bad}`, !!(await until(async () => why.test(await problem.innerText()))), await problem.innerText().catch(() => ""));
  }
  check("a refused address leaves the app where it was", (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("relay_get"))).configured === false);

  // A good one saves and starts the app again on it.
  await field.fill(` ${base}/ `);
  await go.click();
  const restarted = await until(() => !browser.isConnected() || browser.contexts().every((c) => c.pages().every((p) => p.isClosed())), 20000);
  check("a good address restarts the app", !!restarted);
  await browser.close().catch(() => {});
  ({ browser, page } = await attach());
  page.on("pageerror", (e) => pageErrors.push(e.message));
  const after = await until(() => page.evaluate(() => window.__TAURI_INTERNALS__.invoke("relay_get")));
  check("it starts again on the saved relay, tidied", after?.configured === true && after?.address === base, JSON.stringify(after));
  const signIn = page.getByRole("button", { name: "Sign in", exact: true });
  check("then asks to sign in, as any first run does", !!(await until(() => signIn.isVisible())));
  await signIn.click();
  const login = await until(async () => {
    for (const ctx of browser.contexts()) for (const p of ctx.pages()) if ((await p.title().catch(() => "")) === "PIN page") return p;
  });
  check("signing in opens that relay's login", !!login);
  await fetch(`${base}/control/finish-login`, { method: "POST" });
  if (login) await login.evaluate(() => (location.href = "/login-ok?done=1")).catch(() => {});
  const synced = await until(async () => (await relayLog()).requests.some((r) => r.path.startsWith("/api/") && r.cookie?.includes("CF_Authorization=test-login")), 20000);
  check("once signed in, it syncs from that relay with its login", !!synced);
  check("only one copy is running after the restart", ours() === "1", ours());
} catch (e) {
  check("run completed", false, e.stack ?? String(e));
} finally {
  check("no script errors", pageErrors.length === 0, pageErrors.join(" | "));
  await browser?.close().catch(() => {});
  app.kill();
  stopOurs();
  server.close();
  await sleep(1500);
  try {
    rmSync(data, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 });
  } catch (e) {
    console.log(`NOTE  couldn't remove ${data}: ${e.code}`);
  }
  const passed = results.filter(Boolean).length;
  console.log(`${passed} of ${results.length} passed`);
  process.exit(passed === results.length ? 0 : 1);
}
