// A read that can't reach the home PC is kept and sent once it can: after a reconnect, and after the app restarts.
// Without this the chat stays unread on the phone and every other PC. Real app, fake relay, made-up chats.
//   node e2e/read-retry.mjs <playwright-core dir>
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { mkdtempSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeRelay } from "./fake-relay.mjs";

const [pwDir] = process.argv.slice(2);
const require = createRequire(path.join(pwDir, "x.js"));
const { chromium } = require("playwright-core");
const exe = path.resolve("src-tauri/target/debug/wa-relay-app.exe");
const data = mkdtempSync(path.join(os.tmpdir(), "wa-reads-"));
const CDP = 9342;
const PORT = 18792;
const base = `http://127.0.0.1:${PORT}`;
const A = "15550000088@s.whatsapp.net";
const B = "15550000089@s.whatsapp.net";

const results = [];
function check(name, ok, detail = "") {
  results.push(ok);
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${!ok && detail ? ` :: ${detail}` : ""}`);
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms = 10000) {
  const start = Date.now();
  while (Date.now() - start < ms) {
    try {
      const v = await fn();
      if (v) return v;
    } catch {}
    await sleep(60);
  }
  return null;
}
const control = (p, body) =>
  fetch(`${base}/control/${p}`, { method: "POST", body: body ? JSON.stringify(body) : undefined }).then((r) => r.json());
const readsFor = async (chat) => (await control("log")).read.filter((r) => r.chat === chat);

const { server } = makeRelay();
await new Promise((r) => server.listen(PORT, "127.0.0.1", r));
const now = Date.now();
await control("add-chats", {
  chats: [
    { jid: A, name: "Made-up Reader A", ts: now - 60000, read: false, text: "First made-up question" },
    { jid: B, name: "Made-up Reader B", ts: now - 50000, read: false, text: "Another made-up question" },
  ],
});
await control("add-messages", {
  messages: [
    { id: "ra2", chat: A, text: "Second made-up question", ts: now - 40000 },
    { id: "rb2", chat: B, text: "More made-up text", ts: now - 30000 },
  ],
});

let app;
let browser;
const pageErrors = [];
async function start() {
  app = spawn(exe, [], {
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
  browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 30000);
  if (!browser) throw new Error("couldn't attach to the app's WebView");
  const page = await until(async () => {
    for (const ctx of browser.contexts())
      for (const p of ctx.pages()) if (p.url().includes("tauri.localhost") || p.url().includes("index.html")) return p;
  }, 15000);
  if (!page) throw new Error("main page not found");
  page.on("pageerror", (e) => pageErrors.push(e.message));
  page.on("console", (m) => m.type() === "error" && pageErrors.push(m.text()));
  return page;
}
async function stop() {
  await browser?.close().catch(() => {});
  app?.kill();
  await sleep(1500);
}

try {
  let page = await start();
  const signIn = page.getByRole("button", { name: "Sign in", exact: true });
  await until(() => signIn.isVisible());
  await signIn.click();
  await until(async () => {
    for (const ctx of browser.contexts()) for (const p of ctx.pages()) if ((await p.title().catch(() => "")) === "PIN page") return p;
  }, 15000);
  await control("finish-login");
  for (const ctx of browser.contexts())
    for (const p of ctx.pages()) if ((await p.title().catch(() => "")) === "PIN page") await p.evaluate(() => (location.href = "/login-ok?done=1")).catch(() => {});

  let invoke = (cmd, args) => page.evaluate(([c, a]) => window.__TAURI_INTERNALS__.invoke(c, a), [cmd, args]);
  const unread = async (chat) => (await invoke("chat_list")).find((c) => c.jid === chat)?.unread;
  check("both made-up chats arrive unread", !!(await until(async () => (await unread(A)) === 2 && (await unread(B)) === 2, 30000)));

  // The home PC is up but refuses reads for a while.
  await control("fail-reads", { status: 503 });
  await invoke("mark_read", { chat: A });
  check("the chat clears here at once", (await unread(A)) === 0);
  check("the home PC was tried, and has nothing yet", (await control("log")).readTries >= 1 && (await readsFor(A)).length === 0);

  // Reads work again; a reconnect means a sync, and after it the waiting read goes.
  await control("fail-reads", { status: 0 });
  await control("drop-stream");
  const sent = await until(async () => {
    const r = await readsFor(A);
    return r.length ? r : null;
  }, 20000);
  check("after the reconnect the read reaches the home PC", !!sent, JSON.stringify(await readsFor(A)));
  check(
    "with both messages and how far was read",
    !!sent && sent[0].messages.map((m) => m.id).sort().join() === ["added-" + A, "ra2"].sort().join() && sent[0].up_to_ms === now - 40000,
    JSON.stringify(sent),
  );
  await control("drop-stream");
  await sleep(2500);
  check("and only once", (await readsFor(A)).length === 1, String((await readsFor(A)).length));

  // Offline this time, and the app is closed before it comes back.
  await control("outage", { mode: "refuse" });
  await until(async () => (await invoke("conn_state")).state !== "connected", 5000);
  await invoke("mark_read", { chat: B });
  check("offline, the chat still clears here", (await unread(B)) === 0);
  await stop();
  await control("outage", { mode: "" });
  page = await start();
  invoke = (cmd, args) => page.evaluate(([c, a]) => window.__TAURI_INTERNALS__.invoke(c, a), [cmd, args]);
  const later = await until(async () => {
    const r = await readsFor(B);
    return r.length ? r : null;
  }, 30000);
  check("a read made offline is sent after the app restarts", !!later && later[0].up_to_ms === now - 30000, JSON.stringify(later));
  check("and the chat is still read after the restart", (await unread(B)) === 0);

  // A read the home PC refuses outright isn't kept, or it would be retried for ever.
  await control("add-messages", { messages: [{ id: "ra3", chat: A, text: "A third made-up line", ts: Date.now() }] });
  await until(async () => (await unread(A)) === 1, 15000);
  await control("fail-reads", { status: 400 });
  const tries = (await control("log")).readTries;
  await invoke("mark_read", { chat: A });
  await control("fail-reads", { status: 0 });
  await control("drop-stream");
  await sleep(4000);
  check("a refused read is dropped, not retried", (await control("log")).readTries === tries + 1, String((await control("log")).readTries - tries));
  check("no page errors", pageErrors.length === 0, pageErrors.slice(0, 3).join(" | "));
} catch (e) {
  check("the run finished", false, e.message);
} finally {
  await stop();
  server.close();
  await sleep(800);
  rmSync(data, { recursive: true, force: true });
}
const passed = results.filter(Boolean).length;
console.log(`${passed} of ${results.length} passed`);
process.exit(passed === results.length ? 0 : 1);
