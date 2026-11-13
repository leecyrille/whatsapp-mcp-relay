// The real app on the real Windows taskbar, against the fake relay, measured by its pixels. The button shows on this
// PC's taskbar while it runs. It never takes focus, and its window stays off-screen. Nothing reaches WhatsApp.
//
//   node e2e/taskbar-real.mjs <playwright-core dir> [soak seconds]
import { spawn, execFile } from "node:child_process";
import { createRequire } from "node:module";
import { mkdtempSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeRelay } from "./fake-relay.mjs";

const [pwDir, soakArg] = process.argv.slice(2);
const soak = Number(soakArg ?? 0);
const require = createRequire(path.join(pwDir, "x.js"));
const { chromium } = require("playwright-core");

const exe = path.resolve("src-tauri/target/debug/wa-relay-app.exe");
const sampler = path.resolve("e2e/taskbar-sample.ps1");
const data = mkdtempSync(path.join(os.tmpdir(), "wa-taskbar-"));
const TITLE = "WA Relay test - please ignore";
const CDP = 9334;
const PORT = 18791;
const base = `http://127.0.0.1:${PORT}`;

const results = [];
function check(name, ok, detail = "") {
  results.push({ name, ok });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? ` :: ${typeof detail === "string" ? detail : JSON.stringify(detail)}` : ""}`);
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms = 10000) {
  const start = Date.now();
  while (Date.now() - start < ms) {
    try {
      const v = await fn();
      if (v) return v;
    } catch {}
    await sleep(100);
  }
  return null;
}
const control = (p, body) => fetch(`${base}/control/${p}`, { method: "POST", body: body ? JSON.stringify(body) : undefined }).then((r) => r.json());

const { server, DM, DM2 } = makeRelay();
await new Promise((r) => server.listen(PORT, "127.0.0.1", r));

const app = spawn(exe, [], {
  env: {
    ...process.env,
    WA_RELAY_URL: base,
    WA_DATA_DIR: data,
    WA_TEST_OFFSCREEN: "1",
    WA_TEST_REAL_TASKBAR: "1",
    WA_TEST_TITLE: TITLE,
    WA_TEST_BLINK_MS: "200", // Five times the real pace, so a handle leak shows sooner.
    WA_SECOND_INSTANCE: "1", // Never touches a real copy of the app that's running on this PC.
    WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: `--remote-debugging-port=${CDP}`,
  },
  stdio: "ignore",
});

const watch = (seconds, title = TITLE) =>
  new Promise((resolve, reject) => {
    execFile("powershell", ["-NoProfile", "-STA", "-ExecutionPolicy", "Bypass", "-File", sampler, "-Title", title, "-Seconds", String(seconds), "-AppPid", String(app.pid)], { timeout: (seconds + 60) * 1000 }, (err, out) => {
      if (err) return reject(err);
      resolve(JSON.parse(out.trim().split("\n").pop()));
    });
  });

let browser;
try {
  browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 30000);
  if (!browser) throw new Error("couldn't attach to the app's WebView");
  const findPage = (pred) =>
    until(async () => {
      for (const ctx of browser.contexts()) for (const p of ctx.pages()) if (await pred(p)) return p;
    }, 15000);
  const page = await findPage((p) => p.url().includes("tauri.localhost") || p.url().includes("index.html"));
  const invoke = (cmd, args) => page.evaluate(([c, a]) => window.__TAURI_INTERNALS__.invoke(c, a), [cmd, args]);
  const state = (action = "state") => invoke("attention_test", { action });

  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  const login = await findPage(async (p) => (await p.title().catch(() => "")) === "PIN page");
  await control("finish-login");
  await login.evaluate(() => (location.href = "/login-ok?done=1")).catch(() => {});
  await until(() => page.locator(".side .row .name").first().isVisible(), 15000);

  const readAll = async () => {
    for (const c of await invoke("chat_list")) if (c.unread > 0) await invoke("mark_read", { chat: c.jid });
    return until(async () => (await invoke("chat_list")).every((c) => c.unread === 0));
  };
  let stamp = Date.now() + 60000;
  const incoming = (chat = DM) => control("incoming", { id: `taskbar-${++stamp}`, chat, text: "A made-up message", ts: stamp });

  check("everything read to begin with", !!(await readAll()));
  const startHandles = await watch(0);

  // Closed to the tray, nothing unread.
  await state("close");
  check("closing hides the window", !!(await until(async () => !(await state()).visible)));
  await sleep(1500);
  const idle = await watch(0);
  check("closed with nothing unread: no taskbar button", !idle.found, idle);

  // A message arrives: the usual state.
  await incoming();
  const parked = await until(async () => {
    const s = await state();
    return s.parked && s.visible && s.minimized ? s : null;
  });
  check("a message brings back a minimised window", !!parked, parked ?? (await state()));
  check("the rule says both blink", parked?.tray === "blink" && parked?.taskbar === "blink", parked);
  await sleep(1500);
  const blink = await watch(25);
  check("the button is on the real taskbar", blink.found, blink);
  check("it really blinks: lit and unlit by its pixels", blink.pulses >= 10 && blink.changes >= 20, blink);
  check("still going at the end of the run", blink.lastChange > 22, blink);
  check("it never took the foreground", blink.foregroundApp === 0, blink);

  if (soak > 0) {
    const long = await watch(soak);
    check(`still blinking after ${soak} s more`, long.lastChange > soak - 4 && long.pulses > soak / 2.5, long);
    check("and never took the foreground", long.foregroundApp === 0, long);
  }

  const trayName = "WA Relay: ";
  const tray = await watch(10, trayName);
  if (tray.found) check("the tray icon's pixels change while it blinks", tray.colourChanges >= 20, tray);
  else console.log("NOTE  the tray icon is in the hidden overflow, so its pixels can't be measured here");
  const swaps = (await state()).swaps;
  await sleep(2000);
  check("the tray icon keeps being swapped", (await state()).swaps >= swaps + 5);

  // Read: both stop, the window goes away.
  await readAll();
  const gone = await until(async () => {
    const s = await state();
    return !s.visible && !s.parked ? s : null;
  });
  check("reading it hides the window again", !!gone, gone ?? (await state()));
  check("the rule says both stop", gone?.tray === "plain" && gone?.taskbar === "off", gone);
  await sleep(1500);
  check("the button has gone from the taskbar", !(await watch(0)).found);
  const swapsRead = (await state()).swaps;
  await sleep(1500);
  check("the tray stops swapping", (await state()).swaps === swapsRead);

  // Silence.
  await incoming();
  await until(async () => (await state()).parked);
  await invoke("silence_set", { minutes: 60 });
  const quiet = await until(async () => {
    const s = await state();
    return !s.visible ? s : null;
  });
  check("silencing takes the button away", !!quiet, quiet ?? (await state()));
  check("silenced: tray steady, taskbar off", quiet?.tray === "steady" && quiet?.taskbar === "off", quiet);
  await incoming();
  await sleep(2000);
  check("silenced: a new message brings nothing back", !(await state()).visible && !(await watch(0)).found);
  await invoke("silence_clear");
  check("silence ending brings it back", !!(await until(async () => (await state()).parked)));
  await sleep(1500);
  const after = await watch(8);
  check("and it blinks again", after.changes >= 6, after);

  // The Settings switch, and Windows' fewer animations, which both end up in the same call.
  await invoke("blink_set", { on: false });
  await until(async () => (await state()).taskbar === "steady");
  await sleep(5000); // Its last flash takes a few seconds to settle.
  const steady = await watch(8);
  check("switched off, the button stays lit and doesn't blink", steady.changes === 0 && steady.litAtEnd, steady);
  check("switched off, the tray is steady", (await state()).tray === "steady");
  await invoke("blink_set", { on: true });
  await until(async () => (await state()).taskbar === "blink");
  await sleep(1500);
  check("switched back on, it blinks", (await watch(8)).changes >= 6);

  // Opening it, as clicking the button does, without the focus a test must not take.
  await state("show");
  const open = await until(async () => {
    const s = await state();
    return s.visible && !s.minimized && !s.parked ? s : null;
  });
  check("opening a parked window makes it an ordinary window", !!open, open ?? (await state()));
  const shown = await page.evaluate(() => ({ vis: document.visibilityState, w: innerWidth, h: innerHeight }));
  check("its page is visible at full size", shown.vis === "visible" && shown.w > 400 && shown.h > 300, shown);
  check("the message that arrived while parked is there", await page.evaluate(() => document.body.innerText.includes("A made-up message")));
  await sleep(1000);
  check("it stays open: reading doesn't hide a window the user opened", !!(await readAll()) && !!(await until(async () => (await state()).taskbar === "off")) && (await sleep(1500), (await state()).visible));

  // Minimised by hand is the user's, not ours.
  await incoming();
  await state("minimize");
  await until(async () => (await state()).minimized);
  await sleep(1500);
  const byHand = await state();
  check("minimised by hand it isn't treated as parked", !byHand.parked && byHand.taskbar === "blink", byHand);
  const handBlink = await watch(6);
  check("and it blinks", handBlink.changes >= 4, handBlink);
  await readAll();
  await sleep(2000);
  const handRead = await state();
  check("reading leaves it minimised, not hidden", handRead.visible && handRead.minimized && handRead.taskbar === "off", handRead);
  await sleep(1500);
  const handOff = await watch(3);
  check("and the button stops being lit", handOff.found && !handOff.litAtEnd && handOff.changes === 0, handOff);

  // Closing with something unread.
  await state("show");
  await until(async () => !(await state()).minimized);
  await incoming(DM2);
  await until(async () => (await state()).taskbar === "blink");
  await state("close");
  const closedUnread = await until(async () => {
    const s = await state();
    return s.parked && s.minimized ? s : null;
  });
  check("closing with something unread leaves a blinking button", !!closedUnread, closedUnread ?? (await state()));
  await sleep(1500);
  check("and it does blink", (await watch(6)).changes >= 4);

  const endHandles = await watch(0);
  const finalSwaps = (await state()).swaps;
  check(`no icon handles leaked over ${finalSwaps} tray swaps`, finalSwaps > 100 && endHandles.gdi - startHandles.gdi < 60 && endHandles.user - startHandles.user < 60, { start: [startHandles.gdi, startHandles.user], end: [endHandles.gdi, endHandles.user], swaps: finalSwaps });
} catch (e) {
  check("the run finished", false, String(e));
} finally {
  await browser?.close().catch(() => {});
  app.kill();
  server.close();
  server.closeAllConnections?.();
  await sleep(800);
  try {
    rmSync(data, { recursive: true, force: true, maxRetries: 5, retryDelay: 300 });
  } catch {}
}
const failed = results.filter((r) => !r.ok);
console.log(`\n${results.length - failed.length} of ${results.length} passed`);
process.exit(failed.length ? 1 : 0);
