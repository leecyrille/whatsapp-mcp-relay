// The app must not lock up when the tray, silence and the blink all change at once, from many threads. It did once:
// a worker waited for the main thread while holding the tray, and the main thread was waiting for the tray. And a
// second copy started while one is stuck must say so and leave, not hang too. Real app, fake relay, made-up chats.
//   node e2e/hang.mjs <playwright-core dir> <exe>
// The exe is a test build under an identifier of its own, so the second copy can't reach a real one on this PC:
//   npx tauri build --debug --no-bundle --config "{\"identifier\":\"app.warelay.hangtest\"}"
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { mkdtempSync } from "node:fs";
import { rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { makeRelay } from "./fake-relay.mjs";

const [pwDir, exeArg, only] = process.argv.slice(2);
const require = createRequire(path.join(pwDir, "x.js"));
const { chromium } = require("playwright-core");
const exe = path.resolve(exeArg);
let CDP = 9347;
let PORT = 18797;
let base = "";
const HAMMER_MS = 12000;
const A = "15550000091@s.whatsapp.net";

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
/** The answer, or null if there isn't one in time, which is what a hang looks like from here. */
const within = (promise, ms) => Promise.race([promise.catch(() => null), sleep(ms).then(() => null)]);
const control = (p, body) =>
  fetch(`${base}/control/${p}`, { method: "POST", body: body ? JSON.stringify(body) : undefined }).then((r) => r.json());

const servers = [];
async function newRelay() {
  const { server } = makeRelay();
  servers.push(server);
  base = `http://127.0.0.1:${++PORT}`;
  await new Promise((r) => server.listen(PORT, "127.0.0.1", r));
  await control("add-chats", { chats: [{ jid: A, name: "Made-up Reader", ts: Date.now() - 60000, read: false, text: "A made-up unread question" }] });
}

const env = (data, extra = {}) => {
  const e = {
    ...process.env,
    WA_RELAY_URL: base,
    WA_DATA_DIR: data,
    WA_TEST_OFFSCREEN: "1",
    WA_TEST_BLINK_MS: "1",
    WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: `--remote-debugging-port=${CDP}`,
    ...extra,
  };
  delete e.WA_SECOND_INSTANCE;
  return e;
};

const running = [];
async function start(data) {
  await newRelay();
  CDP++; // A copy that was stuck may leave its port in use for a while.
  const app = spawn(exe, [], { env: env(data), stdio: "ignore" });
  running.push(app);
  const browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 30000);
  if (!browser) throw new Error("couldn't attach to the app's WebView");
  const pages = () => browser.contexts().flatMap((c) => c.pages());
  const page = await until(async () => pages().find((p) => p.url().includes("tauri.localhost")), 15000);
  if (!page) throw new Error("main page not found");
  const signIn = page.getByRole("button", { name: "Sign in", exact: true });
  await until(() => signIn.isVisible());
  await signIn.click();
  const login = await until(async () => {
    for (const p of pages()) if ((await p.title().catch(() => "")) === "PIN page") return p;
  }, 15000);
  await control("finish-login");
  await login?.evaluate(() => (location.href = "/login-ok?done=1")).catch(() => {});
  const ready = await until(async () => (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("chat_list"))).length > 0, 20000);
  if (!ready) throw new Error("the app didn't sign in and sync");
  return { app, browser, page };
}
async function stop(app) {
  app?.kill();
  await sleep(1500);
}
const invoke = (page, name, args) => page.evaluate(([n, a]) => window.__TAURI_INTERNALS__.invoke(n, a), [name, args]);
const state = (page, action = "state") => invoke(page, "attention_test", { action });

const dirs = [];
const dataDir = () => {
  dirs.push(mkdtempSync(path.join(os.tmpdir(), "wa-hang-")));
  return dirs.at(-1);
};

try {
  if (only !== "second") {
    const { app, page } = await start(dataDir());
    check("something is unread, so the tray blinks", !!(await until(async () => (await state(page)).tray === "blink", 8000)));
    const before = await state(page);
    await state(page, `hammer:${HAMMER_MS}`);
    const start0 = Date.now();
    let answered = 0;
    let slowest = 0;
    let stuck = false;
    while (Date.now() - start0 < HAMMER_MS + 1000) {
      const asked = Date.now();
      // Answered on the main thread.
      if (!(await within(state(page), 8000))) {
        stuck = true;
        break;
      }
      slowest = Math.max(slowest, Date.now() - asked);
      answered++;
      await sleep(50);
    }
    // Busy isn't hung: one that's only slow answers once there's nothing left to do.
    if (stuck && (await until(async () => Date.now() - start0 > HAMMER_MS + 15000 && (await within(state(page), 10000)), HAMMER_MS + 30000))) {
      stuck = false;
      console.log("      (it stopped answering, and then recovered)");
    }
    check("the app keeps answering while the tray, silence and the blink all change at once", !stuck, `after ${answered} answers it never answered again`);
    if (!stuck) {
      const after = await until(async () => {
        const s = await state(page);
        return s.hammers === 0 ? s : null;
      }, 10000);
      check("every thread got to the end", !!after);
      console.log(`      ${after?.rounds} changes, ${after?.swaps - before.swaps} blinks, ${answered} answers, the slowest after ${slowest} ms`);
      check("and plenty happened", after?.rounds > 1000 && after?.swaps > before.swaps, JSON.stringify(after));
      await invoke(page, "silence_clear");
      await invoke(page, "blink_set", { on: true });
      check("afterwards the blink works as before", !!(await until(async () => (await state(page)).tray === "blink", 8000)));
      await invoke(page, "silence_set", { minutes: 60 });
      check("and so does silence", !!(await until(async () => (await state(page)).tray === "steady", 8000)));
      check("with the tooltip saying so", !!(await until(async () => /silenced until/.test((await state(page)).tooltip ?? ""), 8000)), JSON.stringify(await state(page)));
      await invoke(page, "silence_clear");
      check("and saying what's unread once it's off", !!(await until(async () => /^WA Relay: \d+ unread$/.test((await state(page)).tooltip ?? ""), 8000)), JSON.stringify(await state(page)));
    }
    await stop(app);
  }

  // A second copy, with the first one answering, and then with it stuck.
  {
    const data = dataDir();
    const { app, page } = await start(data);
    const second = async (limit) => {
      const began = Date.now();
      const copy = spawn(exe, [], { env: env(data), stdio: ["ignore", "pipe", "ignore"] });
      running.push(copy);
      let out = "";
      copy.stdout.on("data", (d) => (out += d));
      const code = await within(new Promise((r) => copy.on("exit", (c) => r({ code: c }))), limit);
      const took = Date.now() - began;
      if (!code) copy.kill();
      await sleep(200);
      return { code: code ? code.code : "still running", took, out };
    };
    const quick = await second(15000);
    check("with the first copy answering, a second hands over and leaves quietly", quick.code === 0 && quick.took < 9000 && !/isn't answering/.test(quick.out), JSON.stringify(quick));
    void state(page, "freeze:45000").catch(() => {}); // Its answer may only come once it's over.
    await sleep(1500);
    check("the first copy is now stuck", (await within(state(page), 1500)) === null);
    const held = await second(30000);
    console.log(`      the second copy left after ${(held.took / 1000).toFixed(1)} s`);
    check("a second copy says the first isn't answering, and leaves within 20 seconds", held.code === 0 && held.took < 20000 && /isn't answering/.test(held.out), JSON.stringify(held));
    check("without ending the first", app.exitCode === null);
    check("which carries on once it's unstuck", !!(await until(() => within(state(page), 2000), 60000)));
    const again = await second(15000);
    check("and takes a second copy's hand-over again", again.code === 0 && again.took < 9000 && !/isn't answering/.test(again.out), JSON.stringify(again));
    await stop(app);
  }
} catch (e) {
  check("the test ran to the end", false, String(e?.stack ?? e).slice(0, 300));
} finally {
  for (const p of running) p.kill();
  for (const s of servers) {
    s.closeAllConnections?.();
    s.close();
  }
  await sleep(2500);
  for (const d of dirs) await within(rm(d, { recursive: true, force: true }), 5000);
  const failed = results.filter((r) => !r).length;
  console.log(`\n${results.length - failed} of ${results.length} passed`);
  process.exit(failed ? 1 : 0);
}
