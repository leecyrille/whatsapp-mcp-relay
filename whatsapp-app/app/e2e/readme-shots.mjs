// README screenshots of the real app, in light and dark, against the fake relay's made-up showcase data.
// Isolated data folder and an off-screen window. Nothing here reaches WhatsApp, Claude or the real relay.
//
//   node e2e/readme-shots.mjs <folder with playwright-core> <output folder>
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeRelay } from "./fake-relay.mjs";

const [pwDir, outDir] = process.argv.slice(2);
const { chromium } = createRequire(path.join(pwDir, "x.js"))("playwright-core");
const exe = path.resolve("src-tauri/target/debug/wa-relay-app.exe");
const data = mkdtempSync(path.join(os.tmpdir(), "wa-shots-"));
const CDP = 9334;
const PORT = 18791;
const base = `http://127.0.0.1:${PORT}`;
mkdirSync(outDir, { recursive: true });

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
const control = (p) => fetch(`${base}/control/${p}`, { method: "POST" }).then((r) => r.json());

const { server } = makeRelay({ showcase: true });
await new Promise((r) => server.listen(PORT, "127.0.0.1", r));
const app = spawn(exe, [], {
  env: {
    ...process.env,
    WA_RELAY_URL: base,
    WA_DATA_DIR: data,
    WA_TEST_OFFSCREEN: "1",
    WA_SECOND_INSTANCE: "1",
    WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: `--remote-debugging-port=${CDP}`,
  },
  stdio: "ignore",
});

let browser;
try {
  browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 30000);
  if (!browser) throw new Error("couldn't attach to the app's WebView");
  const findPage = (pred) =>
    until(async () => {
      for (const ctx of browser.contexts()) for (const p of ctx.pages()) if (await pred(p)) return p;
    });
  const page = await findPage((p) => p.url().includes("tauri.localhost") || p.url().includes("index.html"));
  if (!page) throw new Error("main page not found");
  await page.setViewportSize({ width: 1180, height: 760 }).catch(() => {});

  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  const login = await findPage(async (p) => (await p.title().catch(() => "")) === "PIN page");
  await control("finish-login");
  await login?.evaluate(() => (location.href = "/login-ok?done=1")).catch(() => {});

  const row = page.locator(".side .row", { hasText: "Weekend Hikers" }).first();
  if (!(await until(() => row.isVisible()))) throw new Error("chat list didn't load");
  await sleep(1500);
  await row.click();
  if (!(await until(async () => (await page.locator(".bubble").count()) >= 9))) throw new Error("conversation didn't load");
  await page.mouse.move(5, 5);
  await sleep(1200);

  for (const scheme of ["light", "dark"]) {
    await page.emulateMedia({ colorScheme: scheme });
    await sleep(700);
    const file = path.join(outDir, `wa-relay-${scheme}.png`);
    await page.screenshot({ path: file });
    console.log("saved", file);
  }
} finally {
  await browser?.close().catch(() => {});
  app.kill();
  server.close();
  await sleep(500);
  rmSync(data, { recursive: true, force: true });
}
process.exit(0);
