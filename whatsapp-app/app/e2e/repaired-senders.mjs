// What a PC does when the old-sender repair reaches it, at the real size: the real app, the fake relay,
// made-up rows. The repair is a one-off, so this is run by hand:
//   node e2e/repaired-senders.mjs <playwright-core dir>
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
const data = mkdtempSync(path.join(os.tmpdir(), "wa-repair-"));
const CDP = 9341;
const PORT = 18791;
const base = `http://127.0.0.1:${PORT}`;
const ROWS = 10184;
const CHATS = 68;
const PEOPLE = 107;

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

const { server } = makeRelay();
await new Promise((r) => server.listen(PORT, "127.0.0.1", r));

// Old group rows, sender the group's own ID and no address, as they are before the repair.
const old = Date.now() - 40 * 86400000;
const groupOf = (i) => `1203639${String(i % CHATS).padStart(11, "0")}@g.us`;
const rows = Array.from({ length: ROWS }, (_, i) => ({
  id: `old${i}`,
  chat: groupOf(i),
  sender: groupOf(i).split("@")[0],
  sender_jid: "",
  text: i === 7 ? "findme unusualword" : `Made-up old message ${i}`,
  ts: old + i * 1000,
}));
await control("add-chats", {
  chats: Array.from({ length: CHATS }, (_, i) => ({ jid: groupOf(i), name: `Made-up Group ${i}`, ts: old + ROWS * 2000 + i, read: true, text: "Newest made-up message" })),
});
await control("add-messages", { messages: rows });

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
const pageErrors = [];
try {
  browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 30000);
  if (!browser) throw new Error("couldn't attach to the app's WebView");
  const findPage = (pred) =>
    until(async () => {
      for (const ctx of browser.contexts()) for (const p of ctx.pages()) if (await pred(p)) return p;
    }, 15000);
  const page = await findPage((p) => p.url().includes("tauri.localhost") || p.url().includes("index.html"));
  if (!page) throw new Error("main page not found");
  page.on("pageerror", (e) => pageErrors.push(e.message));
  page.on("console", (m) => m.type() === "error" && pageErrors.push(m.text()));
  const signIn = page.getByRole("button", { name: "Sign in", exact: true });
  await until(() => signIn.isVisible());
  await signIn.click();
  const login = await findPage(async (p) => (await p.title().catch(() => "")) === "PIN page");
  await control("finish-login");
  await login.evaluate(() => (location.href = "/login-ok?done=1")).catch(() => {});

  const invoke = (cmd, args) => page.evaluate(([c, a]) => window.__TAURI_INTERNALS__.invoke(c, a), [cmd, args]);
  const chatList = () => invoke("chat_list");
  const page1 = (chat) => invoke("messages_page", { chat, limit: 500 });
  const old7 = async () => (await page1(groupOf(7))).filter((m) => m.id.startsWith("old"));
  const synced = await until(async () => {
    const made = (await chatList()).filter((c) => c.name.startsWith("Made-up Group"));
    return made.length === CHATS && (await old7()).length >= Math.floor(ROWS / CHATS) ? made : null;
  }, 90000);
  check("the old rows arrive first, as they are today", !!synced);
  const before = await chatList();
  const brief = (list) => JSON.stringify(list.map((c) => [c.jid, c.name, c.last_ms, c.preview, c.unread]).sort());
  const unreadBefore = before.reduce((n, c) => n + c.unread, 0);
  check("before: none of them has a sender address", (await old7()).every((m) => !m.sender_jid));

  // The repair: the same rows, same IDs and times, a real sender, and new sequence numbers. The bridge is
  // stopped while this happens, so the PC is cut off and catches up when it reconnects.
  const lidOf = (i) => `7${String(i % PEOPLE).padStart(14, "0")}`;
  await control("outage", { mode: "refuse" });
  await until(async () => (await invoke("conn_state")).state !== "connected", 5000);
  // The stream is down, so nothing hears this as a live message: it can only arrive through a pull.
  await control("add-messages", { messages: rows.map((m, i) => ({ ...m, sender: lidOf(i), sender_jid: `${lidOf(i)}@lid` })) });
  const started = Date.now();
  await control("outage", { mode: "" });
  const pulled = await until(async () => (await old7()).every((m) => m.sender_jid), 120000);
  check("the app pulls the repaired rows by itself when it reconnects", !!pulled);
  console.log(`      catching up after the reconnect took ${((Date.now() - started) / 1000).toFixed(1)} s`);

  let wrong = 0;
  let total = 0;
  const people = new Set();
  for (let c = 0; c < CHATS; c++) {
    const seen = (await page1(groupOf(c))).filter((m) => m.id.startsWith("old"));
    total += seen.length;
    for (const m of seen) {
      const i = Number(m.id.slice(3));
      if (m.sender_jid !== `${lidOf(i)}@lid`) wrong++;
      people.add(m.sender_jid);
    }
  }
  check("every row names the person it should, with none added or lost", wrong === 0 && total === ROWS, `${wrong} wrong, ${total} rows`);
  check("the chats hold many different people", people.size === PEOPLE, String(people.size));
  const after = await chatList();
  const unreadAfter = after.reduce((n, c) => n + c.unread, 0);
  check("unread counts don't change", unreadAfter === unreadBefore, `${unreadBefore} to ${unreadAfter}`);
  check("chat order, previews and times don't change", brief(after) === brief(before));
  const hits = await invoke("search", { query: "unusualword", limit: 10 });
  check("search still finds a repaired row, once", hits.length === 1, String(hits.length));

  // The list only draws what's in view, and the newest chat is the highest number.
  await page.locator(".side .row", { hasText: `Made-up Group ${CHATS - 1}` }).first().click();
  check("the chat opens and shows them", !!(await until(async () => (await page.locator(".convo").innerText()).includes("Made-up old message"))));
  const state = await invoke("silence_state");
  check("no notifications", state.test_toasts === 0, JSON.stringify(state));
  check("no page errors", pageErrors.length === 0, pageErrors.slice(0, 3).join(" | "));
} catch (e) {
  check("the run finished", false, e.message);
} finally {
  await browser?.close().catch(() => {});
  app.kill();
  server.close();
  await sleep(800);
  rmSync(data, { recursive: true, force: true });
}
const passed = results.filter(Boolean).length;
console.log(`${passed} of ${results.length} passed`);
process.exit(passed === results.length ? 0 : 1);
