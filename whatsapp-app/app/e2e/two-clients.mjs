// Two real copies of the app, side by side, on the real relay code and the real bridge. One makes changes and
// the other has to show them without a restart. Group changes only touch the test front's temporary groups file.
// A picture change goes to the real store for a moment and is removed again, so run it on the chat named below,
// and expect any open WA Relay to show the test picture briefly. Prints no names or message text.
//
//   node e2e/two-clients.mjs <playwright-core dir> <chat jid for the picture round trip>
import { spawn, spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { mkdtempSync, rmSync } from "node:fs";
import { crc32, deflateSync } from "node:zlib";
import os from "node:os";
import path from "node:path";

const [pwDir, pictureChat] = process.argv.slice(2);
const require = createRequire(path.join(pwDir, "x.js"));
const { chromium } = require("playwright-core");
const FRONT = 18794;
const results = [];
function check(name, ok, detail = "") {
  results.push({ name, ok: !!ok });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? ` :: ${detail}` : ""}`);
}
// A real 16x16 PNG, made here so the real bridge has something valid to decode.
function testPng() {
  const chunk = (type, data) => {
    const body = Buffer.concat([Buffer.from(type), data]);
    const out = Buffer.alloc(body.length + 8);
    out.writeUInt32BE(data.length, 0);
    body.copy(out, 4);
    out.writeUInt32BE(crc32(body), body.length + 4);
    return out;
  };
  const header = Buffer.alloc(13);
  header.writeUInt32BE(16, 0);
  header.writeUInt32BE(16, 4);
  header[8] = 8;
  header[9] = 2;
  const row = Buffer.concat([Buffer.from([0]), Buffer.alloc(48, 0x55)]);
  const pixels = Buffer.concat(Array(16).fill(row));
  return Buffer.concat([Buffer.from("\x89PNG\r\n\x1a\n", "latin1"), chunk("IHDR", header), chunk("IDAT", deflateSync(pixels)), chunk("IEND", Buffer.alloc(0))]);
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms = 30000) {
  const start = Date.now();
  while (Date.now() - start < ms) {
    try {
      const v = await fn();
      if (v) return v;
    } catch {}
    await sleep(200);
  }
  return null;
}
spawnSync("taskkill", ["/F", "/T", "/IM", "relay.test.exe"], { stdio: "ignore" });
await until(() => fetch(`http://127.0.0.1:${FRONT}/login-ok`).then(() => false, () => true), 10000);
spawn(process.env.GO_EXE ?? "go", ["test", "-run", "TestLiveOutageFront", "-timeout", "20m", "./internal/relay"], {
  cwd: path.resolve("../relay"),
  env: { ...process.env, LIVE_OUTAGE_PORT: String(FRONT), LIVE_OUTAGE_WRITES: "1" },
  stdio: "ignore",
});
if (!(await until(() => fetch(`http://127.0.0.1:${FRONT}/login-ok`).then((r) => r.ok), 120000))) {
  console.log("FAIL  the test front didn't start");
  process.exit(1);
}

const exe = path.resolve("src-tauri/target/debug/wa-relay-app.exe");
const clients = [];
async function start(cdp) {
  const data = mkdtempSync(path.join(os.tmpdir(), "wa-two-"));
  const proc = spawn(exe, [], {
    env: { ...process.env, WA_RELAY_URL: `http://127.0.0.1:${FRONT}`, WA_DATA_DIR: data, WA_TEST_OFFSCREEN: "1", WA_SECOND_INSTANCE: "1", WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: `--remote-debugging-port=${cdp}` },
    stdio: "ignore",
  });
  const browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${cdp}`));
  const page = await until(async () => browser.contexts().flatMap((c) => c.pages()).find((p) => p.url().includes("tauri.localhost")));
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  const client = { proc, browser, page, data, invoke: (cmd, args = {}) => page.evaluate(([c, a]) => window.__TAURI_INTERNALS__.invoke(c, a), [cmd, args]) };
  clients.push(client);
  await until(async () => (await client.invoke("conn_state")).link === "ok", 120000);
  return client;
}
try {
  const a = await start(9341);
  const b = await start(9342);
  check("two separate copies are running and current", (await a.invoke("conn_state")).link === "ok" && (await b.invoke("conn_state")).link === "ok");
  const tab = (c, name) => c.page.getByRole("tab", { name });
  const tabs = (c) => c.page.getByRole("tab").allInnerTexts();

  // Rename.
  await a.page.getByRole("button", { name: "Settings", exact: true }).click();
  const before = await tab(b, /^Personal/).locator(".chat-count").innerText();
  const nameBox = a.page.getByLabel("Name of group Personal");
  await nameBox.fill("Family");
  await nameBox.blur();
  check("renaming on one PC shows on the other, no restart", await until(() => tab(b, /^Family/).isVisible()));
  check("renaming keeps the group's chats", (await tab(b, /^Family/).locator(".chat-count").innerText()) === before, before);

  // Colour.
  const swatch = a.page.getByLabel("Color for Family");
  await swatch.fill("#c026d3");
  await swatch.dispatchEvent("change");
  check("a colour change shows on the other PC", await until(async () => (await b.invoke("groups_get")).groups.find((g) => g.name === "Family")?.color === "#c026d3" && (await tab(b, /^Family/).locator(".dot").evaluate((d) => getComputedStyle(d).backgroundColor)) === "rgb(192, 38, 211)"));
  const dark = await tab(b, /^Family/).locator(".dot").evaluate((d) => getComputedStyle(d).boxShadow);
  check("the dot has a ring, so a colour can't vanish into the theme", dark !== "none", dark);

  // Create.
  await a.page.getByLabel("New group name").fill("Test Group");
  await a.page.getByRole("button", { name: "Add", exact: true }).click();
  check("a new group appears on the other PC", await until(() => tab(b, /^Test Group/).isVisible()));

  // A chat placed on one PC.
  const chats = await b.invoke("chat_list");
  const jid = chats[0].jid;
  const group = (await a.invoke("groups_get")).groups.find((g) => g.name === "Test Group");
  await a.invoke("assign", { jids: [jid], group: group.id });
  check("assigning a chat on one PC shows on the other", await until(async () => (await tab(b, /^Test Group/).locator(".chat-count").innerText()) === "1"));

  // Delete: its chats go to Unsorted, and no chat is deleted.
  const unsortedBefore = Number(await tab(b, /^Unsorted/).locator(".chat-count").innerText());
  await a.page.getByLabel("Delete group Test Group").click();
  check("deleting a group removes it from the other PC", await until(async () => (await tab(b, /^Test Group/).count()) === 0));
  check("deleting a group sends its chats to Unsorted, and deletes none", await until(async () => Number(await tab(b, /^Unsorted/).locator(".chat-count").innerText()) === unsortedBefore + 1) && (await b.invoke("chat_list")).length === chats.length);
  check("the built-in names stay refused", (await a.invoke("group_create", { name: "People", color: null }).then(() => "created", (e) => String(e))) !== "created");

  // Real pictures, looked up gently while the two apps show their chat lists.
  const gotReal = await until(async () => Object.keys((await b.invoke("pictures_get")).pictures ?? {}).length > 0, 90000);
  const seen = Object.keys((await b.invoke("pictures_get")).pictures ?? {}).length;
  check("real WhatsApp pictures arrive on demand", gotReal, `${seen} so far`);
  const shown = await until(() => b.page.locator(".side .row img").evaluateAll((imgs) => imgs.filter((i) => i.complete && i.naturalWidth > 0).length), 60000);
  check("a real picture actually draws in the chat list", shown > 0, `${shown} drawn`);

  // A picture set on one PC, on the real store, then removed.
  if (pictureChat) {
    const png = testPng();
    const before = (await b.invoke("pictures_get")).pictures?.[pictureChat] ?? 0;
    const file = await a.invoke("save_pasted", { bytes: [...png], ext: "png" });
    await a.invoke("picture_set", { jid: pictureChat, token: file.token });
    check("a picture set on one PC reaches the other", await until(async () => ((await b.invoke("pictures_get")).own ?? []).includes(pictureChat)));
    const during = (await b.invoke("pictures_get")).pictures?.[pictureChat];
    check("its version moved on", during > before, `${before} -> ${during}`);
    await a.invoke("picture_remove", { jid: pictureChat });
    check("removing it reaches the other too", await until(async () => !((await b.invoke("pictures_get")).own ?? []).includes(pictureChat)));
    const svg = await a.invoke("save_pasted", { bytes: [...Buffer.from("<svg xmlns='http://www.w3.org/2000/svg'/>")], ext: "png" });
    const refused = await a.invoke("picture_set", { jid: pictureChat, token: svg.token }).then(() => "accepted", (e) => String(e));
    check("the real bridge refuses SVG with a png name", refused !== "accepted", refused);
  }
} catch (e) {
  check("the test ran to the end", false, e.stack ?? String(e));
} finally {
  for (const c of clients) {
    await c.browser?.close().catch(() => {});
    c.proc.kill();
  }
  spawnSync("taskkill", ["/F", "/T", "/IM", "relay.test.exe"], { stdio: "ignore" });
  await sleep(1000);
  for (const c of clients) {
    try {
      rmSync(c.data, { recursive: true, force: true });
    } catch {}
  }
  const failed = results.filter((r) => !r.ok).length;
  console.log(`${results.length - failed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
}
