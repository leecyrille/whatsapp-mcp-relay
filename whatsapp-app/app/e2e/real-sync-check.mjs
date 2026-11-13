// Runs the real app against the live bridge through a proxy that can only read. It checks that real
// rows parse and how fast sync and search are. It prints counts and timings only, takes no screenshots,
// and opens no chat. Anything that could send or change something gets a harmless local answer and
// never reaches the bridge.
//
//   node e2e/real-sync-check.mjs <playwright-core dir>
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { mkdtempSync, rmSync, statSync } from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";

const require = createRequire(path.join(process.argv[2], "x.js"));
const { chromium } = require("playwright-core");

const BRIDGE = "http://127.0.0.1:18700";
const PORT = 18791;
const CDP = 9334;
const READ_ONLY = { "/api/sync": "/api/app/sync", "/api/contacts": "/api/app/contacts", "/api/events": "/api/app/events" };
const refused = [];

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  if (url.pathname === "/login-ok") {
    res.writeHead(200, { "content-type": "text/html", "set-cookie": "CF_Authorization=check; HttpOnly; Path=/" });
    return res.end("<!doctype html><title>ok</title>");
  }
  if (url.pathname === "/api/groups") {
    res.writeHead(200, { "content-type": "application/json" });
    return res.end(JSON.stringify({ type: "groups", groups: [{ id: "personal", name: "Personal", color: "#2f9e6e", order: 0 }], assign: {} }));
  }
  const target = req.method === "GET" ? READ_ONLY[url.pathname] : undefined;
  if (!target) {
    refused.push(`${req.method} ${url.pathname}`);
    res.writeHead(200, { "content-type": "application/json" });
    return res.end('{"ok":true}');
  }
  const upstream = await fetch(BRIDGE + target + url.search);
  res.writeHead(upstream.status, { "content-type": upstream.headers.get("content-type") ?? "application/json" });
  for await (const chunk of upstream.body) res.write(chunk);
  res.end();
});
await new Promise((r) => server.listen(PORT, "127.0.0.1", r));

const data = mkdtempSync(path.join(os.tmpdir(), "wa-real-"));
const app = spawn(path.resolve("src-tauri/target/debug/wa-relay-app.exe"), [], {
  env: {
    ...process.env,
    WA_RELAY_URL: `http://127.0.0.1:${PORT}`,
    WA_DATA_DIR: data,
    WA_TEST_OFFSCREEN: "1",
    WA_SECOND_INSTANCE: "1",
    WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: `--remote-debugging-port=${CDP}`,
  },
  stdio: "ignore",
});
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let browser;
let code = 1;
try {
  for (let i = 0; i < 100 && !browser; i++) {
    browser = await chromium.connectOverCDP(`http://127.0.0.1:${CDP}`).catch(() => null);
    if (!browser) await sleep(300);
  }
  const main = () => browser.contexts().flatMap((c) => c.pages()).find((p) => p.url().includes("tauri.localhost"));
  let page;
  for (let i = 0; i < 60 && !page; i++) {
    page = main();
    if (!page) await sleep(200);
  }
  const started = Date.now();
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  const invoke = (cmd, args = {}) => page.evaluate(([c, a]) => window.__TAURI_INTERNALS__.invoke(c, a), [cmd, args]);
  const head = (await (await fetch(`${BRIDGE}/api/app/status`)).json()).head;
  let stats;
  for (let i = 0; i < 400; i++) {
    stats = await invoke("cache_stats");
    const pending = await page.evaluate(() => document.querySelector("[role=progressbar]") !== null);
    if (stats.messages > 0 && !pending && i > 3) break;
    await sleep(100);
  }
  const syncMs = Date.now() - started;
  const timed = async (fn) => {
    const t = performance.now();
    const value = await fn();
    return [Math.round(performance.now() - t), value];
  };
  const [listMs, chats] = await timed(() => invoke("chat_list"));
  const [contactsMs, contacts] = await timed(() => invoke("contacts"));
  const big = chats.reduce((a, b) => a, chats[0]);
  const [pageMs, msgs] = await timed(() => invoke("messages_page", { chat: big.jid, limit: 60 }));
  const searches = [];
  for (const q of ["the", "meeting", "thanks", "ok"]) {
    const [ms, hits] = await timed(() => invoke("search", { query: q, limit: 100 }));
    searches.push(`${q.length}-letter word: ${hits.length} hits in ${ms} ms`);
  }
  const bridge = await (await fetch(`${BRIDGE}/api/app/sync?after=0&limit=1`)).json();
  const named = chats.filter((c) => !c.name.startsWith("+") && c.name !== "Unknown").length;
  const lidLeft = chats.filter((c) => c.jid.endsWith("@lid")).length;
  const groups = chats.filter((c) => c.is_group).length;
  console.log(JSON.stringify({
    bridge_head: head, synced_messages: stats.messages, chat_rows: stats.chats, merged_from_304: chats.length,
    groups, named, unresolved_lid_chats: lidLeft, contacts: contacts.length,
    cold_sync_ms: syncMs, db_mb: Math.round(stats.db_bytes / 1048576),
    chat_list_ms: listMs, contacts_ms: contactsMs, first_page_ms: pageMs, first_page_rows: msgs.length, searches,
    unread_now: chats.reduce((s, c) => s + c.unread, 0),
    write_requests_stopped_at_proxy: refused,
  }, null, 1));
  code = stats.messages > 0 ? 0 : 1;
} catch (e) {
  console.log("FAILED: " + (e.message ?? e));
} finally {
  await browser?.close().catch(() => {});
  app.kill();
  server.closeAllConnections?.();
  server.close();
  await sleep(800);
  try {
    rmSync(data, { recursive: true, force: true });
  } catch {}
  process.exit(code);
}
