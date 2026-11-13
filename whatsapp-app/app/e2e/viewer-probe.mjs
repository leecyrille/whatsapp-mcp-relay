// What a script inside the document viewer could reach, tried one thing at a time. Made-up data only.
//   node e2e/viewer-probe.mjs <playwright-core dir>
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { mkdtempSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeRelay } from "./fake-relay.mjs";
import { pdf } from "./make-docs.mjs";

const [pwDir] = process.argv.slice(2);
const require = createRequire(path.join(pwDir, "x.js"));
const { chromium } = require("playwright-core");
const exe = path.resolve("src-tauri/target/debug/wa-relay-app.exe");
const data = mkdtempSync(path.join(os.tmpdir(), "wa-probe-"));
const CDP = 9344;
const PORT = 18794;
const base = `http://127.0.0.1:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms = 10000) {
  const start = Date.now();
  while (Date.now() - start < ms) {
    try {
      const v = await fn();
      if (v) return v;
    } catch {}
    await sleep(50);
  }
  return null;
}
const control = (p, body) => fetch(`${base}/control/${p}`, { method: "POST", body: body ? JSON.stringify(body) : undefined }).then((r) => r.json());
const { server, GROUP } = makeRelay();
await new Promise((r) => server.listen(PORT, "127.0.0.1", r));
const app = spawn(exe, [], { env: { ...process.env, WA_RELAY_URL: base, WA_DATA_DIR: data, WA_TEST_OFFSCREEN: "1", WA_SECOND_INSTANCE: "1", WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: `--remote-debugging-port=${CDP}` }, stdio: "ignore" });
let exited = null;
app.on("exit", (code) => (exited = code));
let browser;
try {
  browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 30000);
  const findPage = (pred) => until(async () => { for (const ctx of browser.contexts()) for (const p of ctx.pages()) if (await pred(p)) return p; }, 15000);
  const page = await findPage((p) => p.url().includes("tauri.localhost"));
  page.on("console", (m) => m.type() !== "info" && console.log("  console:", m.type(), m.text().slice(0, 200)));
  page.on("crash", () => console.log("  PAGE CRASHED"));
  await page.addInitScript(() => window.addEventListener("message", (e) => console.log("main got message", JSON.stringify(e.data).slice(0, 120))));
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  const login = await findPage(async (p) => (await p.title().catch(() => "")) === "PIN page");
  await control("finish-login");
  await login.evaluate(() => (location.href = "/login-ok?done=1")).catch(() => {});
  await until(() => page.locator(".side .row", { hasText: "Bingo Committee" }).first().isVisible(), 20000);
  await control("doc", { id: "p1", chat: GROUP, filename: "Made-up.pdf", mime: "application/pdf", base64: pdf(2).toString("base64") });
  await page.locator(".side .row", { hasText: "Bingo Committee" }).first().click();
  const row = page.locator(".doc", { hasText: "Made-up.pdf" });
  await until(() => row.isVisible(), 15000);
  await row.getByRole("button", { name: "Preview file" }).click();
  const viewer = await until(() => page.frames().find((f) => f.url().includes("/viewer")), 15000);
  await sleep(3000);
  console.log("dialog says:", (await page.getByRole("dialog").innerText()).replace(/\s+/g, " ").slice(0, 300));
  console.log("viewer body:", (await viewer.evaluate(() => document.body.innerHTML.length + " chars, " + document.querySelectorAll("canvas").length + " canvases, origin " + location.origin + ", notes: " + [...document.querySelectorAll(".note")].map((n) => n.textContent).join(" | "))));
  console.log("viewer state:", await viewer.evaluate(() => JSON.stringify({ selfOrigin: self.origin, theme: document.documentElement.dataset.theme ?? null, head: document.head.innerHTML.slice(0, 400), loaded: performance.getEntriesByType("resource").map((r) => r.name.split("/").pop()) })));
  console.log("workers:", await viewer.evaluate(async () => {
    const tryOne = (type) => new Promise((r) => {
      try {
        const w = new Worker(URL.createObjectURL(new Blob(["postMessage('hi')"], { type: "text/javascript" })), { type });
        w.onmessage = (e) => r(type + " ok");
        w.onerror = (e) => r(type + " error " + (e.message || "no message"));
        setTimeout(() => r(type + " no answer"), 3000);
      } catch (e) { r(type + " threw " + e.message); }
    });
    return [await tryOne("classic"), await tryOne("module")].join(", ");
  }));
  console.log("what's injected:", await viewer.evaluate(() => JSON.stringify({ internals: typeof window.__TAURI_INTERNALS__, ipc: typeof window.ipc, webview: typeof window.chrome?.webview, postMessage: typeof window.chrome?.webview?.postMessage })));
  for (const [what, code] of [
    ["chrome.webview.postMessage", "window.chrome?.webview?.postMessage ? (window.chrome.webview.postMessage('probe'), 'sent') : 'absent'"],
    ["window.ipc.postMessage", "window.ipc?.postMessage ? (window.ipc.postMessage('probe'), 'sent') : 'absent'"],
  ]) {
    try {
      console.log(what, "->", await viewer.evaluate(code));
    } catch (e) {
      console.log(what, "-> error", String(e).slice(0, 120));
    }
    await sleep(800);
    console.log("  app still running:", exited === null);
  }
  try {
    const r = await viewer.evaluate(() => Promise.race([window.__TAURI_INTERNALS__.invoke("chat_list").then((x) => "WORKED " + JSON.stringify(x).length, (e) => "refused: " + String(e)), new Promise((res) => setTimeout(() => res("no answer in 5 s"), 5000))]));
    console.log("invoke chat_list ->", r);
  } catch (e) {
    console.log("invoke chat_list -> error", String(e).slice(0, 160));
  }
  await sleep(1000);
  console.log("app still running:", exited === null, exited);
} catch (e) {
  console.log("stopped:", e.message);
} finally {
  await browser?.close().catch(() => {});
  app.kill();
  server.close();
  await sleep(800);
  rmSync(data, { recursive: true, force: true });
}
