// End-to-end test of the real app binary against the fake relay. Uses an isolated data folder and an
// off-screen window. Nothing here reaches WhatsApp, Claude or the real relay.
//
//   node e2e/run.mjs <playwright-core dir> <screenshot dir>
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, existsSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeRelay } from "./fake-relay.mjs";
import { ciede2000, contrast as contrastOf, labOf, oklabDistance, oklabOf, rgbOf } from "./colour-math.mjs";

const [pwDir, shotDir] = process.argv.slice(2);
const require = createRequire(path.join(pwDir, "x.js"));
const { chromium } = require("playwright-core");

const exe = path.resolve("src-tauri/target/debug/wa-relay-app.exe");
const data = mkdtempSync(path.join(os.tmpdir(), "wa-e2e-"));
const CDP = 9333;
const PORT = 18790;
const base = `http://127.0.0.1:${PORT}`;
mkdirSync(shotDir, { recursive: true });
const pickedPhoto = path.join(data, "made-up-photo.png");
const pickedVcard = path.join(data, "made-up-contacts.vcf");
writeFileSync(pickedPhoto, Buffer.from([137, 80, 78, 71, 1, 2, 3, 4]));

const results = [];
function check(name, ok, detail = "") {
  results.push({ name, ok });
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
const relayLog = () => control("log");

const { server, DM, DM2, GROUP } = makeRelay();
await new Promise((r) => server.listen(PORT, "127.0.0.1", r));

const app = spawn(exe, [], {
  env: {
    ...process.env,
    WA_RELAY_URL: base,
    WA_DATA_DIR: data,
    WA_TEST_OFFSCREEN: "1",
    WA_SECOND_INSTANCE: "1", // Never touches a real copy of the app that's running on this PC.
    WA_TEST_PICK_FILE: pickedPhoto,
    WA_TEST_PICK_VCARD: pickedVcard,
    WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: `--remote-debugging-port=${CDP}`,
  },
  stdio: "ignore",
});

let browser;
const pageErrors = [];
try {
  browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 30000);
  if (!browser) throw new Error("couldn't attach to the app's WebView");
  const findPage = async (pred) =>
    until(async () => {
      for (const ctx of browser.contexts()) for (const p of ctx.pages()) if (await pred(p)) return p;
    }, 15000);
  const page = await findPage((p) => p.url().includes("tauri.localhost") || p.url().includes("index.html"));
  if (!page) throw new Error("main page not found");
  page.on("pageerror", (e) => {
    pageErrors.push(`${e.message} (after "${results.at(-1)?.name ?? "start"}")`);
    if (e.message.includes("each_key_duplicate"))
      void page
        .evaluate(() => ({
          search: document.querySelector("#search")?.value,
          results: [...document.querySelectorAll(".results .row, .results .hit")].map((r) => r.textContent.trim().slice(0, 60)),
          convo: [...document.querySelectorAll(".convo .row")].map((r) => r.id),
          header: document.querySelector(".convo header .name")?.textContent,
        }))
        .then((d) => console.log("DUPLICATE KEY CONTEXT", JSON.stringify(d)))
        .catch(() => {});
  });
  page.on("console", (m) => m.type() === "error" && pageErrors.push(m.text()));
  await page.setViewportSize({ width: 1180, height: 780 }).catch(() => {});

  // First run: no login yet.
  const signIn = page.getByRole("button", { name: "Sign in", exact: true });
  check("first run asks to sign in", !!(await until(() => signIn.isVisible())));
  await page.screenshot({ path: path.join(shotDir, "01-signin.png") });
  await signIn.click();

  const login = await findPage(async (p) => (await p.title().catch(() => "")) === "PIN page");
  check("sign-in opens in its own window", !!login);
  if (login) {
    const reachable = await login.evaluate(async () => {
      const t = window.__TAURI_INTERNALS__;
      if (!t) return "no ipc object";
      try {
        const chats = await t.invoke("chat_list");
        return "CALLED: " + JSON.stringify(chats).length;
      } catch (e) {
        return "refused: " + String(e);
      }
    });
    check("sign-in window can't call app commands", !reachable.startsWith("CALLED"), reachable);
    const external = await login.evaluate(async () => {
      try {
        location.href = "https://example.com/";
      } catch {}
      await new Promise((r) => setTimeout(r, 700));
      return location.href;
    }).catch(() => "navigated");
    check("sign-in window stays on allowed hosts", !String(external).includes("example.com"), external);
    await control("finish-login");
    await login.evaluate(() => (location.href = "/login-ok?done=1")).catch(() => {});
  }
  const closed = await until(async () => {
    for (const ctx of browser.contexts()) for (const p of ctx.pages()) if ((await p.title().catch(() => "")).match(/PIN|Signed in/)) return false;
    return true;
  });
  check("sign-in window closes after login", !!closed);

  // Sync.
  const firstRow = page.locator(".side .row .name").first();
  check("chat list appears after sync", !!(await until(() => firstRow.isVisible(), 15000)));
  const log0 = await relayLog();
  const apiCalls = log0.requests.filter((r) => r.path.startsWith("/api/"));
  check("every request carries the cookie and client header", apiCalls.length > 0 && apiCalls.every((r) => r.cookie?.includes("CF_Authorization") && r.client === "1"));
  check("no request carries a browser Origin", apiCalls.every((r) => !r.origin));

  const names = await until(async () => {
    const list = await page.locator(".side .row .name").allTextContents();
    return list.includes("Sample Person") ? list : null;
  });
  check("phone and LID chats merge into one named person", names?.filter((n) => n === "Sample Person").length === 1 && names.length === 3, JSON.stringify(names));
  check("newest chat is first", names?.[0] === "Sample Person", JSON.stringify(names));
  check("chat with unread shows a count", (await page.locator(".side .row .count").first().textContent())?.trim() === "2");
  await page.screenshot({ path: path.join(shotDir, "02-chats.png") });

  // Reading and blue ticks.
  await page.evaluate(() => window.focus());
  await page.locator(".side .row", { hasText: "Sample Person" }).click();
  check("messages from both addresses show together", !!(await until(async () => (await page.locator(".bubble").count()) === 4)));
  const readLog = await until(async () => {
    const l = await relayLog();
    return l.read.length >= 2 ? l : null;
  }, 4000);
  const hasFocus = await page.evaluate(() => document.hasFocus());
  if (hasFocus) {
    check("opening a chat sends read receipts per address", readLog?.read.length === 2 && readLog.read.every((r) => r.receipts === true), JSON.stringify(readLog?.read));
    check("unread clears after reading", !!(await until(async () => (await page.locator(".side .row .count").count()) === 0)));
  } else {
    console.log("NOTE  off-screen window isn't focused, so the read-receipt check was skipped");
  }
  check("quoted reply renders", (await page.locator(".quote").count()) === 1);
  check("links render as links", (await page.locator(".text a").getAttribute("href")) === "https://example.com/report.pdf");
  await page.screenshot({ path: path.join(shotDir, "03-conversation.png") });

  // Sending.
  const box = page.getByLabel("Message", { exact: true });
  await box.fill("Thanks, see you at 9");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  const sent = await until(async () => {
    const l = await relayLog();
    return l.sent.length === 1 ? l.sent[0] : null;
  });
  check("send reaches the relay with the right chat and text", sent?.chat === DM && sent?.text === "Thanks, see you at 9", JSON.stringify(sent));
  check("send carries a client ID for safe retry", !!sent?.client_id);
  check("sent message appears in the chat", !!(await until(() => page.locator(".row.mine", { hasText: "Thanks, see you at 9" }).isVisible())));
  check("composer clears after sending", (await box.inputValue()) === "");
  await box.fill("line one");
  await box.press("Shift+Enter");
  await box.type("line two");
  check("Shift+Enter makes a new line", (await box.inputValue()) === "line one\nline two");
  await box.fill("");

  // Replying.
  await page.locator(".convo .row", { hasText: "Bring the relay test set" }).hover();
  await page.locator(".convo .row", { hasText: "Bring the relay test set" }).getByRole("button", { name: "Reply" }).click();
  check("reply bar shows", await page.locator(".reply").isVisible());
  await box.fill("Will do");
  await box.press("Enter");
  const replied = await until(async () => {
    const l = await relayLog();
    return l.sent.length === 2 ? l.sent[1] : null;
  });
  check("reply sends a quote", replied?.quote?.id === "a3" && replied?.text === "Will do", JSON.stringify(replied));

  // Live messages.
  await until(() => page.locator(".convo .row.mine", { hasText: "Will do" }).isVisible());
  const before = await page.locator(".bubble").count();
  await control("incoming", { id: "live1", chat: DM, text: "Live hello from the stream" });
  check("live message appears without reloading", !!(await until(() => page.locator(".convo .row", { hasText: "Live hello from the stream" }).isVisible())));
  check("message count grew by one", (await page.locator(".bubble").count()) === before + 1);
  await control("incoming", { id: "live2", chat: DM2, text: "Another chat wakes up" });
  check("a message in another chat raises its count", !!(await until(async () => (await page.locator(".side .row", { hasText: "Bob Plumber" }).locator(".count").textContent())?.trim() === "1")));
  check("a chat with a new message moves to the top", !!(await until(async () => (await page.locator(".side .row .name").first().textContent()) === "Bob Plumber")));

  // Paging older messages.
  await page.locator(".side .row", { hasText: "Bob Plumber" }).click();
  await until(async () => (await page.locator(".bubble").count()) > 40);
  const firstCount = await page.locator(".bubble").count();
  check("long chat opens with one page, not everything", firstCount <= 130, String(firstCount));
  for (let i = 0; i < 8; i++) {
    await page.locator(".scroller").evaluate((el) => (el.scrollTop = -el.scrollHeight));
    await sleep(120);
  }
  check("scrolling up loads older messages", !!(await until(() => page.locator(".convo .row", { hasText: "Older message number 179" }).count())), String(await page.locator(".bubble").count()));
  check("reaching the top offers older from the phone", await page.getByRole("button", { name: /Load older messages/ }).isVisible());

  // Search.
  const search = page.locator("#search");
  await search.fill("substat");
  const hit = page.locator(".hit").first();
  check("search finds a prefix across chats", !!(await until(() => hit.isVisible())));
  check("match is highlighted", (await page.locator(".hit mark").first().textContent())?.toLowerCase() === "substat" || (await page.locator(".hit mark").first().textContent())?.toLowerCase() === "substation");
  await page.screenshot({ path: path.join(shotDir, "04-search.png") });
  await hit.click();
  check("clicking a result opens that message in context", !!(await until(() => page.locator(".row.hl", { hasText: "substation" }).isVisible())));
  for (const evil of ['"', "a OR", "NEAR(", "(((", "*"]) {
    await search.fill(evil);
    await sleep(60);
  }
  check("odd search input doesn't crash", !pageErrors.some((e) => /fts5|syntax/i.test(e)), pageErrors.join("; "));
  await search.fill("");

  // Groups.
  await page.locator(".side .row", { hasText: "Bingo Committee" }).click();
  await until(async () => (await page.locator(".bubble").count()) >= 5);
  await page.getByLabel("Group for this chat").selectOption("work");
  const assigned = await until(async () => {
    const l = await relayLog();
    return l.assign.length === 1 ? l.assign[0] : null;
  });
  check("moving a chat to a group reaches the relay", assigned?.group === "work" && assigned?.jids[0] === GROUP, JSON.stringify(assigned));
  await page.getByRole("tab", { name: /^Work/ }).click();
  check("group tab filters the list", !!(await until(async () => (await page.locator(".side .row").count()) === 1)));
  await page.getByRole("tab", { name: /^Unsorted/ }).click();
  check("unsorted tab shows the rest", !!(await until(async () => (await page.locator(".side .row").count()) === 2)));
  const tabCount = async (name) => Number(await page.getByRole("tab", { name }).locator(".chat-count").innerText());
  check("each tab counts its chats, not just its unread ones", (await tabCount(/^All/)) === 3 && (await tabCount(/^Work/)) === 1 && (await tabCount(/^Unsorted/)) === 2 && (await tabCount(/^Personal/)) === 0);
  check("People and Groups are worked out from the address", (await tabCount(/^People/)) === 2 && (await tabCount(/^Groups/)) === 1);
  const tabOrder = (await page.getByRole("tab").allInnerTexts()).map((s) => s.trim().split(/\s+/)[0]);
  check("built-in tabs come first, custom groups below", tabOrder.join(",") === "All,People,Groups,Unread,Unsorted,Personal,Work", tabOrder.join(","));
  await page.getByRole("tab", { name: /^Groups/ }).click();
  check("a group chat in a custom group is still under Groups", !!(await until(async () => (await page.locator(".side .row").count()) === 1)) && (await page.locator(".side .row", { hasText: "Bingo Committee" }).count()) === 1);
  await page.getByRole("tab", { name: /^People/ }).click();
  check("People shows only one-to-one chats", !!(await until(async () => (await page.locator(".side .row").count()) === 2)));
  await page.getByRole("tab", { name: /^Unsorted/ }).click();
  await page.getByRole("tab", { name: /^All/ }).click();

  // Group rendering and media.
  check("group senders show by contact name", (await page.locator(".sender").allTextContents()).includes("Bob Plumber"));
  check("old rows with no real sender show no wrong name", !(await page.locator(".sender").allTextContents()).some((s) => /\d{10}/.test(s)));
  check("WhatsApp formatting renders", (await page.locator(".text b", { hasText: "Reminder" }).count()) === 1 && (await page.locator(".text i", { hasText: "draw" }).count()) === 1);
  check("reactions show", (await page.locator(".reactions").first().textContent())?.includes("👍"));
  const photo = page.locator(".convo .row", { hasText: "Photo from last week" }).locator("img.full");
  await photo.scrollIntoViewIfNeeded().catch(() => {});
  const loaded = await until(() => photo.evaluate((img) => img.complete && img.naturalWidth > 0), 12000);
  check("photo downloads through the app and displays", !!loaded);
  await page.screenshot({ path: path.join(shotDir, "05-group-media.png") });
  await page.locator(".convo .row", { hasText: "Photo from last week" }).locator(".pic").click();
  check("photo opens full size", !!(await until(() => page.getByRole("dialog", { name: "Image" }).isVisible())));
  await page.keyboard.press("Escape");
  await page.locator(".convo .row", { has: page.locator(".meta-chip") }).getByRole("button", { name: "Play video" }).click();
  check("media the phone no longer has says so", !!(await until(() => page.locator(".media-err", { hasText: "no longer on the phone" }).isVisible(), 8000)));
  const exeRow = page.locator(".convo .row", { hasText: "run-me.exe" });
  await exeRow.getByRole("button", { name: "Open file" }).click();
  check("a program can't be opened from a message", !!(await until(() => page.locator(".toast", { hasText: "could run code" }).isVisible())));

  // Files.
  const picked = await page.evaluate(async () => {
    const png = Uint8Array.from(atob("iVBORw0KGgoAAAANSUhEUgAAAAoAAAAKCAYAAACNMs+9AAAAFUlEQVR42mNkYPj/n4EIwMRIJBhVSAwAVhQBEhQmF1wAAAAASUVORK5CYII="), (c) => c.charCodeAt(0));
    return window.__TAURI_INTERNALS__.invoke("save_pasted", { bytes: Array.from(png), ext: "png" });
  });
  check("pasted image becomes a token, not a path", !!picked.token && !JSON.stringify(picked).includes("\\"), JSON.stringify(picked));
  const preview = await page.evaluate(
    (t) =>
      new Promise((resolve) => {
        const img = new Image();
        img.crossOrigin = "anonymous";
        img.onload = () => {
          const c = document.createElement("canvas");
          c.width = 4;
          c.height = 4;
          c.getContext("2d").drawImage(img, 0, 0, 4, 4);
          try {
            resolve(c.toDataURL("image/jpeg").length > 20);
          } catch {
            resolve(false);
          }
        };
        img.onerror = () => resolve(false);
        img.src = `http://wamedia.localhost/local/${t}`;
      }),
    picked.token,
  );
  check("a picked file can be previewed and thumbnailed", preview === true);
  await page.evaluate(
    ({ token, chat }) =>
      window.__TAURI_INTERNALS__.invoke("send_file", {
        chat, token, kind: "image", caption: "From the app", thumb: null, width: 10, height: 10, seconds: 0, quote: null, clientId: "e2e-file-1",
      }),
    { token: picked.token, chat: GROUP },
  );
  const finished = await until(async () => {
    const l = await relayLog();
    return l.finished.length === 1 ? l.finished[0] : null;
  });
  check("file upload completes with its details", finished?.caption === "From the app" && finished?.bytes > 0 && finished?.client_id === "e2e-file-1", JSON.stringify(finished));
  const badToken = await page.evaluate(() =>
    window.__TAURI_INTERNALS__.invoke("send_file", {
      chat: "x@g.us", token: "C:\\Windows\\win.ini", kind: "document", caption: "", thumb: null, width: 0, height: 0, seconds: 0, quote: null, clientId: "e2e-evil",
    }).then(() => "sent", (e) => String(e)),
  );
  check("the UI can't send a file by naming its path", badToken !== "sent", badToken);
  const badUrl = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("open_link", { url: "file:///C:/Windows/System32/calc.exe" }).then(() => "opened", (e) => String(e)));
  check("message links can't open local files", badUrl !== "opened", badUrl);
  const badPath = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("claude_api", { method: "GET", path: "/../send", body: null }).then(() => "called", (e) => String(e)));
  check("Claude calls can't reach other routes", badPath !== "called", badPath);

  // Contacts.
  await page.getByRole("button", { name: "Contacts", exact: true }).click();
  check("contacts list shows people without chats", !!(await until(() => page.locator(".page .row", { hasText: "Carol NoChat" }).isVisible())));
  await page.getByLabel("Search contacts").fill("sample");
  check("contact search filters", !!(await until(async () => (await page.locator(".page .row").count()) === 1)));
  await page.getByLabel("Search contacts").fill("");
  await page.screenshot({ path: path.join(shotDir, "06-contacts.png") });

  // Claude.
  await page.getByRole("button", { name: "Claude", exact: true }).click();
  check("Claude panel lists allowed folders", !!(await until(async () => (await page.getByLabel("Folder for this conversation").locator("option").count()) === 2)));
  await page.screenshot({ path: path.join(shotDir, "07-claude-new.png") });
  const ask = page.getByLabel("Message to Claude");
  await ask.fill("Clean up the old log");
  await ask.press("Enter");
  const card = page.locator(".approval.waiting");
  check("an action waits for approval", !!(await until(() => card.isVisible())));
  check("the approval shows exactly what will run", (await card.locator("pre").first().textContent())?.includes("del C:\\Projects\\relay\\old.log"));
  const shownCommand = await card.locator("pre").first().textContent();
  check("blank-line padding can't push a command out of sight", shownCommand.includes("blank lines") && shownCommand.split("\n").length <= 4, shownCommand);
  check("invisible characters are named on the card", shownCommand.includes("U+202E") && !shownCommand.includes("\u202E"), shownCommand);
  check("the rest of the input is shown too", (await card.textContent()).includes("run_in_background"));
  check("the card says how long the input really is", (await card.textContent()).includes("7 lines"));
  check("streamed text arrives", !!(await until(() => page.locator(".msg.bot", { hasText: "I'll check that now." }).isVisible())));
  check("read-only tool use is visible", (await page.locator(".tool", { hasText: "notes.txt" }).count()) === 1);
  const unapproved = await relayLog();
  check("nothing is approved until the user answers", unapproved.approvals.length === 0);
  await page.screenshot({ path: path.join(shotDir, "08-claude-approval.png") });
  await card.getByRole("button", { name: "Approve" }).click();
  const approved = await until(async () => {
    const l = await relayLog();
    return l.approvals.length === 1 ? l.approvals[0] : null;
  });
  check("Approve reaches the runner", approved?.id === "ap1" && approved?.approve === true, JSON.stringify(approved));
  check("the job finishes and the card updates", !!(await until(() => page.locator(".a-state.ok").isVisible())) && !!(await until(() => page.locator(".msg.bot", { hasText: "Deleted it." }).isVisible())));

  // Sorting.
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await page.getByRole("button", { name: "Sort chats with Claude" }).click();
  await page.screenshot({ path: path.join(shotDir, "09-sort-options.png") });
  await page.getByRole("button", { name: "Ask Claude" }).click();
  check("suggestions come back for review", !!(await until(() => page.locator(".props li").first().isVisible())));
  const sentToClaude = (await relayLog()).sortRequest;
  check("already-sorted chats aren't re-sent", !sentToClaude.items.some((i) => i.jid === GROUP), JSON.stringify(sentToClaude.items.map((i) => i.jid)));
  check("snippets are included when chosen", sentToClaude.items.some((i) => i.snippets?.length > 0));
  check("nothing moves before accepting", (await relayLog()).assign.length === 1);
  const boxes = page.locator(".props input[type=checkbox]");
  check("unsure suggestions start unticked", (await boxes.nth(0).isChecked()) && !(await boxes.nth(1).isChecked()));
  await page.screenshot({ path: path.join(shotDir, "10-sort-review.png") });
  await page.getByRole("button", { name: "Accept 1" }).click();
  const accepted = await until(async () => {
    const l = await relayLog();
    return l.assign.length === 2 ? l.assign[1] : null;
  });
  check("accepting applies only what was ticked", accepted?.group === "work" && accepted?.jids.length === 1 && accepted.jids[0] === DM2, JSON.stringify(accepted));

  // Settings, and turning off blue ticks.
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  check("settings show cached totals", !!(await until(() => page.getByText(/messages in \d+ chats/).isVisible())));
  await page.screenshot({ path: path.join(shotDir, "11-settings.png") });
  check("automatic updates default to on", await page.getByLabel(/Update automatically/).isChecked());
  check("settings show this version", await page.getByText(/^Version \d+\.\d+\.\d+\./).isVisible());
  await page.getByLabel(/Update automatically/).uncheck();
  check("turning updates off sticks", (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("settings_get"))).auto_update === false);
  await page.getByLabel(/Update automatically/).check();
  const updaterPlugin = await page.evaluate(async () => {
    try {
      await window.__TAURI_INTERNALS__.invoke("plugin:updater|check", {});
      return "CALLED";
    } catch (e) {
      return String(e);
    }
  });
  check("the page can't call the updater plugin directly", updaterPlugin !== "CALLED", updaterPlugin);
  check("a test build without the test setting doesn't look for updates", (await relayLog()).updateChecks === 0);
  await page.getByLabel(/Send read receipts/).uncheck();
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await control("incoming", { id: "live3", chat: DM, text: "Check the tick setting" });
  await page.locator(".side .row", { hasText: "Sample Person" }).click();
  if (hasFocus) {
    const afterToggle = await until(async () => {
      const l = await relayLog();
      const last = l.read[l.read.length - 1];
      return last?.messages.some((m) => m.id === "live3") ? last : null;
    }, 4000);
    check("with receipts off, no blue ticks are asked for", afterToggle?.receipts === false, JSON.stringify(afterToggle));
  }

  // Responsive and dark mode.
  await page.setViewportSize({ width: 520, height: 760 }).catch(() => {});
  await page.emulateMedia({ colorScheme: "dark" });
  await sleep(200);
  check("narrow window shows one column", !(await page.locator(".side").isVisible()) && (await page.locator(".convo").isVisible()));
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1);
  check("no sideways scrolling when narrow", !overflow);
  await page.screenshot({ path: path.join(shotDir, "12-narrow-dark.png") });
  await page.setViewportSize({ width: 1180, height: 780 }).catch(() => {});
  await sleep(150);
  await page.screenshot({ path: path.join(shotDir, "13-wide-dark.png") });
  await page.emulateMedia({ colorScheme: "light" });

  // Login lapses.
  await control("relogin");
  const banner = page.locator(".banner", { hasText: "signed out" });
  check("a lapsed login shows sign-in and keeps saved chats", !!(await until(() => banner.isVisible(), 12000)) && (await page.locator(".bubble").count()) > 0);
  await page.screenshot({ path: path.join(shotDir, "14-signed-out.png") });
  await control("relogin-off");
  await until(async () => (await banner.count()) === 0, 40000);

  // Disconnected: each failure says what's wrong and when things were last current.
  const alert = page.getByRole("alert");
  const warned = (text, ms = 16000) => until(() => page.locator(".banner", { hasText: text }).isVisible(), ms);
  const clear = () => until(async () => (await page.locator(".banner").count()) === 0, 40000);
  check("connected: no warning, and the dot says up to date", !!(await clear()) && (await page.locator(".status-dot.good").count()) === 1);

  await control("drop-stream");
  let flashed = false;
  for (let i = 0; i < 40; i++) {
    if ((await page.locator(".banner.down").count()) > 0) flashed = true;
    await sleep(200);
  }
  check("one dropped connection raises no alarm", !flashed);

  const outageFrom = Date.now();
  await control("outage", { mode: "530" });
  check("tunnel or home PC down: says so", !!(await warned(/Can't reach your home PC/)));
  const delay = Date.now() - outageFrom;
  check("a closed connection is reported within about 8 seconds", delay < 12000 && delay > 6000, String(delay));
  check("the warning says when things were last current", !!(await page.locator(".banner", { hasText: /Chats may be out of date: last updated today at/ }).isVisible()));
  check("the dot stops saying up to date", (await page.locator(".status-dot.good").count()) === 0);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  check("the warning stays visible in other views", await page.locator(".banner.down").isVisible());
  await page.getByRole("button", { name: "Chats", exact: true }).click();

  // Everything saved keeps working.
  await page.locator(".side .row", { hasText: "Sample Person" }).click();
  check("saved chats still open during an outage", !!(await until(async () => (await page.locator(".bubble").count()) > 0)));
  const offlineHits = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("search", { query: "substation", limit: 10 }));
  check("offline search still works during an outage", offlineHits.length > 0);

  // A message sent now must never look sent, and must never go later by itself.
  check("the composer warns before sending", !!(await until(() => page.locator(".offline-note").isVisible())));
  const sentBefore = (await relayLog()).sent.length;
  await page.locator("textarea").first().fill("written during the outage");
  await page.locator("textarea").first().press("Enter");
  const notSent = page.locator(".pending.failed", { hasText: "written during the outage" });
  check("a message sent during an outage is marked Not sent", !!(await until(() => notSent.getByText("Not sent.").isVisible())));
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  check("it's still there after looking at another tab", !!(await until(() => notSent.isVisible())));

  await control("outage", { mode: "502" });
  check("relay down: told apart", !!(await warned(/WA Relay isn't running/)));
  await control("outage", { mode: "refuse" });
  check("no connection at all: says this computer can't get online", !!(await warned(/can't get online/)));

  // Coming back: catches up by itself, and the message still doesn't go until asked.
  await control("incoming-offline", { id: "missed1", chat: DM, text: "Sent while you were away" });
  await control("outage", { mode: "" });
  check("the warning clears by itself on reconnect", !!(await clear()));
  check("a message missed during the outage arrives by itself", !!(await until(() => page.locator(".bubble", { hasText: "Sent while you were away" }).isVisible())));
  check("the unsent message doesn't go by itself", (await relayLog()).sent.length === sentBefore && (await notSent.isVisible()));
  await notSent.getByRole("button", { name: "Retry" }).click();
  check("Retry sends it once, and it stops being marked", !!(await until(async () => (await relayLog()).sent.length === sentBefore + 1 && (await notSent.count()) === 0)));

  // A silent stream counts as disconnected.
  const silentFrom = Date.now();
  await control("outage", { mode: "silent" });
  check("a stream that goes silent counts as disconnected", !!(await warned(/Can't reach your home PC/, 40000)));
  const silentDelay = Date.now() - silentFrom;
  check("silence is reported within about 25 seconds", silentDelay < 30000, String(silentDelay));
  await control("outage", { mode: "" });
  check("it recovers from silence by itself", !!(await clear()));

  // The bridge side, as the relay reports it.
  await control("status", { connected: false, logged_in: false, bridge_down: true });
  check("relay up, bridge down: told apart", !!(await warned(/WhatsApp service isn't running/)));
  await control("status", { connected: false, logged_in: false });
  check("bridge up, not linked: told apart", !!(await warned(/isn't linked/)));
  await control("status", { connected: false, logged_in: true });
  check("linked, not connected: told apart", !!(await warned(/can't reach WhatsApp/)));
  await control("status", { connected: true, logged_in: true });
  check("clears when WhatsApp is back", !!(await clear()));

  await control("status", { connected: false, logged_in: true });
  await sleep(2000);
  await control("status", { connected: true, logged_in: true });
  await sleep(9000);
  check("a brief WhatsApp blip raises no alarm", (await page.locator(".banner.down").count()) === 0);
  void alert;

  // Pictures and colours.
  const groupedRow = page.locator(".side .row", { hasText: "Bingo Committee" });
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await page.getByRole("tab", { name: /^All/ }).click();
  check("a chat in a group shows the group's colour", await until(async () => /#3b7ddd|59, 125, 221/i.test(await groupedRow.getAttribute("style"))));
  check("no picture yet: the initials or icon show", (await groupedRow.locator("img").count()) === 0 || (await until(async () => (await groupedRow.locator("img").count()) === 0)));
  await control("picture", { jid: GROUP, v: 111 });
  const avatarLoaded = () => groupedRow.locator("img").evaluate((img) => img.complete && img.naturalWidth > 0);
  check("a picture announced by the home PC appears by itself", !!(await until(avatarLoaded)));
  const picsBefore = (await relayLog()).pictureGets ?? 0;
  await page.getByRole("tab", { name: /^Work/ }).click();
  await page.getByRole("tab", { name: /^All/ }).click();
  await until(avatarLoaded);
  check("a saved picture isn't fetched again", ((await relayLog()).pictureGets ?? 0) === picsBefore);
  await groupedRow.click();
  const header = page.locator(".convo header");
  await header.getByRole("button", { name: /Change picture/ }).click();
  check("the header offers to change the picture and says it's WA Relay only", await page.getByText("Nothing changes on WhatsApp.").isVisible());
  await page.keyboard.press("Escape");
  await page.mouse.click(600, 400);
  const token = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("save_pasted", { bytes: [137, 80, 78, 71], ext: "png" }).then((f) => f.token));
  await page.evaluate(([jid, t]) => window.__TAURI_INTERNALS__.invoke("picture_set", { jid, token: t }), [GROUP, token]);
  const wrote = (await relayLog()).pictureWrites?.at(-1);
  check("setting a picture sends it to the home PC", wrote?.method === "POST" && wrote?.jid === GROUP && wrote?.bytes === 4, JSON.stringify(wrote));
  await header.getByRole("button", { name: /Change picture/ }).click();
  check("a replaced picture offers to go back", await until(() => page.getByRole("menuitem", { name: /Go back to the WhatsApp picture/ }).isVisible()));
  await page.getByRole("menuitem", { name: /Go back to the WhatsApp picture/ }).click();
  check("going back removes the replacement", await until(async () => (await relayLog()).pictureWrites?.at(-1)?.method === "DELETE"));
  const refused = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("picture_remove", { jid: "../../secret" }).then(() => "called", (e) => String(e)));
  check("a bad address can't be used", refused !== "called", refused);

  // Lots of groups, wrapping. Long names and big counts must fit, and the chat list must stay usable.
  const realGroups = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("groups_get"));
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  const back = page.getByRole("button", { name: "Back to chats" });
  const measure = () =>
    page.evaluate(() => {
      const side = document.querySelector(".side").getBoundingClientRect();
      const tabs = [...document.querySelectorAll('.side [role="tab"]')];
      const rows = [...document.querySelectorAll(".tabs")];
      const by = (t) => tabs.find((b) => b.textContent.trim().startsWith(t))?.getBoundingClientRect();
      const custom = document.querySelector(".tabs.custom");
      const list = document.querySelector(".side .list");
      return {
        tabs: tabs.length,
        outside: tabs.filter((b) => {
          const r = b.getBoundingClientRect();
          return r.left < side.left - 0.5 || r.right > side.right + 0.5;
        }).length,
        clippedSideways: rows.filter((r) => r.scrollWidth > r.clientWidth + 1).length,
        contentCut: tabs.filter((b) => b.scrollWidth > b.clientWidth + 1).length,
        topRowTogether: by("All").top === by("People").top && by("All").top === by("Groups").top,
        unsortedVisible: by("Unsorted").bottom <= side.bottom && by("Unsorted").right <= side.right + 0.5,
        rowGap: getComputedStyle(custom).rowGap,
        customHeight: custom.clientHeight,
        customScrolls: custom.scrollHeight > custom.clientHeight + 1,
        listHeight: list.clientHeight,
        sideWidth: Math.round(side.width),
      };
    });
  const manyGroups = Array.from({ length: 14 }, (_, i) => ({ id: `g${i}`, name: i % 3 === 0 ? `A really long group name number ${i} here`.slice(0, 40) : `Group ${i}`, color: `#${(0x224466 + i * 0x101820).toString(16).slice(0, 6)}`, order: i }));
  await control("groups", { groups: manyGroups, assign: {} });
  await until(() => page.getByRole("tab", { name: /^Group 13/ }).count());
  for (const [label, width, height] of [["wide", 1180, 780], ["the minimum window size", 420, 480]]) {
    await page.setViewportSize({ width, height });
    if (await back.count()) await back.click();
    await sleep(300);
    const m = await measure();
    await page.screenshot({ path: path.join(shotDir, `20-groups-${width}.png`) });
    check(`14 groups, ${label}: nothing is outside the column or clipped sideways`, m.tabs === 19 && m.outside === 0 && m.clippedSideways === 0 && m.contentCut === 0, JSON.stringify(m));
    check(`14 groups, ${label}: built-in tabs stay together on top, with Unsorted visible`, m.topRowTogether && m.unsortedVisible, JSON.stringify(m));
    check(`14 groups, ${label}: spacing is even`, m.rowGap === "4px", m.rowGap);
    check(`14 groups, ${label}: groups are capped, and scroll inside their own space`, m.customHeight <= 151 && m.customScrolls, JSON.stringify(m));
    check(`14 groups, ${label}: the chat list keeps room`, m.listHeight >= 140, String(m.listHeight));
    const lastGroup = page.getByRole("tab", { name: /^Group 13/ });
    await lastGroup.scrollIntoViewIfNeeded();
    await lastGroup.click();
    check(`14 groups, ${label}: the last group can be reached and selected`, (await lastGroup.getAttribute("aria-selected")) === "true");
    await page.getByRole("tab", { name: /^All/ }).click();
    const firstRow = page.locator(".side .row").first();
    await firstRow.click();
    check(`14 groups, ${label}: chats below can still be opened`, !!(await until(() => page.locator(".convo header").isVisible())));
    if (await back.count()) await back.click();
    await sleep(150);

    // Big numbers, with unread badges, on the same tabs.
    const real = await page.evaluate(() =>
      [...document.querySelectorAll(".chat-count")].map((c) => {
        const was = c.firstChild.nodeValue;
        c.firstChild.nodeValue = "12345";
        return was;
      }),
    );
    const big = await measure();
    await page.evaluate((was) => document.querySelectorAll(".chat-count").forEach((c, i) => (c.firstChild.nodeValue = was[i])), real);
    check(`14 groups, ${label}: five-figure counts still fit`, big.outside === 0 && big.clippedSideways === 0 && big.contentCut === 0 && big.topRowTogether, JSON.stringify(big));
  }
  await control("groups", { groups: [{ id: "a", name: "Office", color: "#3b7ddd", order: 0 }, { id: "b", name: "Garden", color: "#d97706", order: 1 }, { id: "c", name: "Personal", color: "#2f9e6e", order: 2 }], assign: {} });
  await until(() => page.getByRole("tab", { name: /^Garden/ }).count());
  await page.setViewportSize({ width: 420, height: 480 });
  await sleep(300);
  const few = await measure();
  await page.screenshot({ path: path.join(shotDir, "21-groups-few.png") });
  check("a few groups: all show with no scrolling", !few.customScrolls && few.outside === 0 && few.clippedSideways === 0, JSON.stringify(few));
  await control("groups", { groups: realGroups.groups, assign: realGroups.assign });
  await page.setViewportSize({ width: 1180, height: 780 });

  // New-message highlight, and People and Groups inside a custom group. All names here are made up.
  const when = Date.now() + 60000;
  const madeUp = (kind, i) => ({
    jid: kind === "Person" ? `1555200${String(i).padStart(4, "0")}@s.whatsapp.net` : `12036320000000${String(i).padStart(4, "0")}@g.us`,
    name: `Made-up ${kind} ${i}`,
    ts: when - i * 1000 - (kind === "Person" ? 0 : 500),
    read: i % 2 === 1,
  });
  const people = Array.from({ length: 40 }, (_, i) => madeUp("Person", i));
  const chatGroups = Array.from({ length: 40 }, (_, i) => madeUp("Group", i));
  await control("add-chats", { chats: [...people, ...chatGroups] });
  const big = Object.fromEntries([...people, ...chatGroups].map((c) => [c.jid, "a"]));
  big[people[39].jid] = "b";
  big[chatGroups[39].jid] = "c";
  big[people[38].jid] = "c";
  await control("groups", {
    groups: [
      { id: "a", name: "Big", color: "#3b7ddd", order: 0 },
      { id: "b", name: "OnlyPeople", color: "#d97706", order: 1 },
      { id: "c", name: "Both", color: "#2f9e6e", order: 2 },
    ],
    assign: big,
  });
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await page.getByRole("tab", { name: /^All/ }).click();
  const rowNamed = (name) => page.locator(".side .row.abs").filter({ has: page.locator(".name", { hasText: new RegExp(`^${name}$`) }) });
  // Newest first, so an unread row and a read one sit together near the end of the made-up chats.
  const newRow = rowNamed("Made-up Group 38");
  const oldRow = rowNamed("Made-up Person 39");
  const scrollToChat = (name) =>
    page.evaluate(async (n) => {
      const at = (await window.__TAURI_INTERNALS__.invoke("chat_list")).findIndex((c) => c.name === n);
      document.querySelector(".side .list").scrollTop = Math.max(0, at * 70 - 300);
    }, name);
  await scrollToChat("Made-up Group 38");
  await until(async () => (await newRow.isVisible()) && (await oldRow.isVisible()));
  check("a chat with new messages is marked, and one without isn't", (await newRow.evaluate((r) => r.classList.contains("unread"))) && !(await oldRow.evaluate((r) => r.classList.contains("unread"))));

  // Works out what's really on screen, by laying the row's colours over the panel.
  const look = (row) =>
    row.evaluate((r) => {
      const parse = (c) => {
        const n = c.match(/[\d.]+/g).map(Number);
        return { rgb: n.slice(0, 3), a: n.length > 3 ? n[3] : 1 };
      };
      const over = (top, under) => top.rgb.map((v, i) => v * top.a + under[i] * (1 - top.a));
      const luminance = (rgb) => {
        const [a, b, c] = rgb.map((v) => ((v /= 255) <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4));
        return 0.2126 * a + 0.7152 * b + 0.0722 * c;
      };
      const contrast = (x, y) => {
        const [hi, lo] = [luminance(x), luminance(y)].sort((p, q) => q - p);
        return Math.round(((hi + 0.05) / (lo + 0.05)) * 100) / 100;
      };
      const panel = parse(getComputedStyle(document.querySelector(".side")).backgroundColor).rgb;
      const style = getComputedStyle(r);
      let bg = over(parse(style.backgroundColor), panel);
      // The pulsing amber is its own layer. This reads it at whatever opacity it has right now.
      const amber = getComputedStyle(r, "::before");
      if (amber.content !== "none" && r.classList.contains("unread")) {
        const layer = parse(amber.backgroundColor);
        bg = over({ rgb: layer.rgb, a: layer.a * Number(amber.opacity) }, bg);
      }
      const ratio = (sel) => contrast(parse(getComputedStyle(r.querySelector(sel)).color).rgb, bg);
      return {
        bg: bg.map(Math.round).join(","),
        name: ratio(".name"),
        preview: ratio(".preview"),
        time: ratio(".time"),
        nameWeight: getComputedStyle(r.querySelector(".name")).fontWeight,
        previewWeight: getComputedStyle(r.querySelector(".preview")).fontWeight,
        stripe: style.boxShadow.includes("inset") && style.boxShadow.includes("4px"),
      };
    });
  const readable = (l) => l.name >= 4.5 && l.preview >= 4.5 && l.time >= 4.5;
  // Holds every pulse at one point, to look at it. 0 ms is the strongest, and 800 ms the faintest.
  const holdPulse = (ms) => page.evaluate((at) => {
    for (const a of document.querySelector(".side").getAnimations({ subtree: true })) {
      if (a.animationName === "unread-pulse") {
        a.pause();
        a.currentTime = at;
      }
    }
  }, ms);
  const pulseState = () => page.evaluate(() => {
    const all = document.querySelector(".side").getAnimations({ subtree: true }).filter((a) => a.animationName === "unread-pulse");
    return { count: all.length, running: all.filter((a) => a.playState === "running").length, phases: new Set(all.map((a) => Math.round((a.currentTime % 1600) / 40))).size, onlyOpacity: all.every((a) => a.effect.getKeyframes().every((k) => Object.keys(k).filter((p) => !["offset", "computedOffset", "easing", "composite"].includes(p)).join() === "opacity")), seconds: all[0]?.effect.getTiming().duration / 1000 };
  });
  const amberOpacity = (row) => row.evaluate((r) => Number(getComputedStyle(r, "::before").opacity));
  const running = await pulseState();
  check("the amber really is animating, on every unread row in view", running.count > 0 && running.running === running.count, JSON.stringify(running));
  check("it only animates opacity, which is cheap", running.onlyOpacity, JSON.stringify(running));
  check("it pulses well under 3 times a second", running.seconds >= 1.2 && running.seconds <= 2, String(running.seconds));
  check("every row is on the same beat", running.phases === 1, JSON.stringify(running));
  const seen = new Set();
  for (let i = 0; i < 9; i++) {
    seen.add(Math.round((await amberOpacity(newRow)) * 10));
    await sleep(120);
  }
  check("over time, the amber goes between strong and faint", Math.max(...seen) >= 7 && Math.min(...seen) <= 4, [...seen].join());
  await page.evaluate(() => (document.querySelector(".side .list").scrollTop += 700));
  await sleep(200);
  await page.evaluate(() => (document.querySelector(".side .list").scrollTop -= 700));
  await sleep(200);
  check("rows scrolled away and back are still on the same beat", (await pulseState()).phases === 1, JSON.stringify(await pulseState()));
  await page.mouse.move(900, 400);
  for (const scheme of ["light", "dark"]) {
    await page.emulateMedia({ colorScheme: scheme });
    await sleep(150);
    await holdPulse(800);
    const faint = await look(newRow);
    await page.screenshot({ path: path.join(shotDir, `44-pulse-faint-${scheme}.png`) });
    await holdPulse(0);
    const fresh = await look(newRow);
    await page.screenshot({ path: path.join(shotDir, `45-pulse-strong-${scheme}.png`) });
    check(`${scheme}: readable at the faint point of the pulse`, readable(faint), JSON.stringify(faint));
    check(`${scheme}: the faint point is still different from a read chat, and from the strong point`, faint.bg !== fresh.bg, JSON.stringify({ faint, fresh }));
    check(`${scheme}: the title stays bold through the whole pulse`, faint.nameWeight === fresh.nameWeight && Number(fresh.nameWeight) >= 700, JSON.stringify([faint.nameWeight, fresh.nameWeight]));
    const old = await look(oldRow);
    check(`${scheme}: a new-message row looks different, with a tint and bold text`, fresh.bg !== old.bg && Number(fresh.nameWeight) > Number(old.nameWeight) && Number(fresh.previewWeight) > Number(old.previewWeight), JSON.stringify({ fresh, old }));
    check(`${scheme}: name, preview and time are readable on the tint`, readable(fresh), JSON.stringify(fresh));
    check(`${scheme}: the group colour stripe stays on a tinted row`, fresh.stripe, JSON.stringify(fresh));
    await newRow.hover();
    await sleep(100);
    const hovered = await look(newRow);
    check(`${scheme}: hover shows on a tinted row and stays readable`, hovered.bg !== fresh.bg && readable(hovered), JSON.stringify(hovered));
    await page.mouse.move(900, 400);
    await newRow.evaluate((r) => r.classList.add("on"));
    const selected = await look(newRow);
    await newRow.evaluate((r) => r.classList.remove("on"));
    check(`${scheme}: an open chat keeps its own colour and stays readable`, selected.bg !== fresh.bg && selected.bg !== old.bg && readable(selected), JSON.stringify(selected));
    await newRow.evaluate((r) => r.classList.add("on"));
    await holdPulse(800);
    check(`${scheme}: an open chat is readable at the faint point too`, readable(await look(newRow)));
    await newRow.evaluate((r) => r.classList.remove("on"));
    await newRow.hover();
    await sleep(100);
    check(`${scheme}: hover is readable at the faint point too`, readable(await look(newRow)));
    await page.mouse.move(900, 400);
    await page.evaluate(() => {
      for (const a of document.querySelector(".side").getAnimations({ subtree: true })) if (a.animationName === "unread-pulse") a.play();
    });
    await page.screenshot({ path: path.join(shotDir, `22-unread-${scheme}.png`) });
  }

  // Steady instead of blinking, by the setting and by Windows' own.
  const steady = async () => ({ pulses: (await pulseState()).count, strongest: await amberOpacity(newRow) });
  await page.emulateMedia({ reducedMotion: "reduce" });
  await sleep(150);
  const reduced = await steady();
  check("if Windows asks for less motion, the highlight is steady and strong", reduced.pulses === 0 && reduced.strongest === 1, JSON.stringify(reduced));
  const attention = () => page.evaluate(() => window.__TAURI_INTERNALS__.invoke("attention_test", { action: "state" }));
  // Windows doesn't flash the window that's in front, so the taskbar's expected state depends on that.
  const expectedOn = (s, how) => (s.focused ? "off" : how);
  const heldSteady = await until(async () => {
    const s = await attention();
    return s.tray === "steady" && s.taskbar === expectedOn(s, "steady") ? s : null;
  });
  check("less motion leaves the tray and taskbar steady too", !!heldSteady, JSON.stringify(await attention()));
  await page.emulateMedia({ reducedMotion: "no-preference" });
  await sleep(150);
  check("and it blinks again when that's off", (await pulseState()).count > 0);
  const bothBlink = await until(async () => {
    const s = await attention();
    return s.tray === "blink" && s.taskbar === expectedOn(s, "blink") ? s : null;
  });
  check("with unread chats, the tray and taskbar both blink", !!bothBlink, JSON.stringify(await attention()));
  const swapsBefore = (await attention()).swaps;
  await sleep(2000);
  check("the tray icon really swaps while blinking", (await attention()).swaps >= swapsBefore + 2);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const blinkSetting = page.getByLabel(/Make unread chats blink/);
  check("the blink is on by default", await blinkSetting.isChecked());
  await blinkSetting.uncheck();
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await until(() => page.locator(".side .list").isVisible());
  await scrollToChat("Made-up Group 38");
  await until(() => newRow.isVisible());
  await sleep(150);
  const switchedOff = await steady();
  check("the setting turns the blink off, leaving a steady strong highlight", switchedOff.pulses === 0 && switchedOff.strongest === 1, JSON.stringify(switchedOff));
  check("the setting is saved on this PC", (await page.evaluate(() => localStorage.getItem("wa-blink"))) === "0");
  const switchSteady = await until(async () => {
    const s = await attention();
    return s.tray === "steady" && s.taskbar === expectedOn(s, "steady") ? s : null;
  });
  check("the setting leaves the tray and taskbar steady too", !!switchSteady, JSON.stringify(await attention()));
  const steadySwaps = (await attention()).swaps;
  await sleep(1800);
  check("and the tray icon stops swapping", (await attention()).swaps === steadySwaps);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await blinkSetting.check();
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await until(() => page.locator(".side .list").isVisible());
  await page.evaluate(() => (document.querySelector(".side .list").scrollTop = 0));

  // Inside a custom group.
  const heads = () => page.locator(".side .list-head").allTextContents();
  const listState = () =>
    page.evaluate(() => {
      const list = document.querySelector(".side .list");
      const items = [...list.querySelectorAll(".list-head, .row.abs")].map((e) => ({ head: e.classList.contains("list-head"), text: e.classList.contains("list-head") ? e.textContent : e.querySelector(".name").textContent, top: e.offsetTop, h: e.offsetHeight })).sort((a, b) => a.top - b.top);
      return { items, scrollHeight: list.scrollHeight, clientHeight: list.clientHeight, overlap: items.some((x, i) => i > 0 && items[i - 1].top + items[i - 1].h > x.top) };
    });
  const scrollList = (y) => page.evaluate((v) => (document.querySelector(".side .list").scrollTop = v), y);
  await page.getByRole("tab", { name: /^Big/ }).click();
  check("a custom group starts with the People heading, labelled exactly", !!(await until(async () => (await heads())[0] === "People")));
  check("the heading isn't turned into capitals", (await page.locator(".side .list-head").first().evaluate((e) => getComputedStyle(e).textTransform)) === "none" && (await page.locator(".side .list-head").first().innerText()) === "People");
  const big0 = await listState();
  const bigPeople = people.length - 2;
  const bigChats = bigPeople + chatGroups.length - 1;
  check("the list height allows for both headings", big0.scrollHeight === 2 * 30 + bigChats * 70, JSON.stringify([big0.scrollHeight, bigChats]));
  check("a long group still only draws the rows near the view", big0.items.length < 30 && !big0.overlap, String(big0.items.length));
  check("only people are under People", big0.items.filter((x) => !x.head).every((x) => x.text.startsWith("Made-up Person")));
  await scrollList(30 + bigPeople * 70 - 140);
  const middle = await until(async () => {
    const s = await listState();
    return s.items.some((x) => x.head && x.text === "Groups") ? s : null;
  });
  const at = middle?.items.findIndex((x) => x.head && x.text === "Groups") ?? -1;
  check("Groups comes after People, with one kind on each side and nothing overlapping", at > 0 && middle.items.slice(0, at).every((x) => x.text.startsWith("Made-up Person")) && middle.items.slice(at + 1).every((x) => x.text.startsWith("Made-up Group")) && !middle.overlap, JSON.stringify(middle?.items.map((x) => x.text)));
  await page.screenshot({ path: path.join(shotDir, "23-headings-boundary.png") });
  await scrollList(big0.scrollHeight);
  // Newest first, so the bottom of the list is the oldest chat.
  const lastInBig = rowNamed("Made-up Group 38");
  check("the last chat can be reached and opened", !!(await until(() => lastInBig.isVisible())));
  await lastInBig.click();
  check("clicking under a heading opens the right chat", !!(await until(async () => (await page.locator(".convo header").innerText()).includes("Made-up Group 38"))));
  await scrollList(0);
  await control("incoming", { id: "after-heading", chat: chatGroups[1].jid, text: "A made-up new message", ts: when + 5000 });
  await scrollList(30 + bigPeople * 70 - 140);
  const raised = rowNamed("Made-up Group 1");
  check("a new message moves a chat up within Groups, highlighted", !!(await until(async () => {
    const s = await listState();
    const i = s.items.findIndex((x) => x.head && x.text === "Groups");
    return i >= 0 && s.items[i + 1]?.text === "Made-up Group 1" && (await raised.evaluate((r) => r.classList.contains("unread")));
  })));
  await page.getByRole("tab", { name: /^Both/ }).click();
  check("a group with one of each shows both headings", !!(await until(async () => (await heads()).join() === "People,Groups")));
  await page.getByRole("tab", { name: /^OnlyPeople/ }).click();
  check("a heading with nothing under it isn't shown", !!(await until(async () => (await heads()).join() === "People")));
  for (const tab of [/^All/, /^People/, /^Groups/, /^Unsorted/]) {
    await page.getByRole("tab", { name: tab }).click();
    await sleep(100);
    check(`no headings in the ${tab.source.slice(1)} tab`, (await heads()).length === 0);
  }
  await page.getByRole("tab", { name: /^Big/ }).click();
  await until(async () => (await heads()).length > 0);
  await page.locator("#search").fill("Made-up Person 2");
  check("search shows its own list, with no People and Groups headings", !!(await until(async () => (await page.locator(".results .section").count()) > 0 && (await heads()).length === 0)));
  await page.locator("#search").fill("");
  await page.setViewportSize({ width: 420, height: 480 });
  if (await back.count()) await back.click();
  await scrollList(0);
  await sleep(200);
  const narrow = await page.locator(".side .list-head").first().evaluate((e) => {
    const r = e.getBoundingClientRect();
    const side = document.querySelector(".side").getBoundingClientRect();
    return r.left >= side.left && r.right <= side.right && e.scrollWidth <= e.clientWidth;
  });
  check("at the minimum window size, headings fit", narrow);
  await page.screenshot({ path: path.join(shotDir, "24-headings-narrow.png") });
  await page.emulateMedia({ colorScheme: "light" });
  await page.setViewportSize({ width: 1180, height: 780 });

  // Strictly newest first, unread or not, even for a message that arrives long after it was sent (The user, 2026-11-10).
  await page.getByRole("tab", { name: /^All/ }).click();
  await scrollList(0);
  const order = () => page.evaluate(() => [...document.querySelectorAll(".side .row.abs")].sort((a, b) => a.offsetTop - b.offsetTop).map((r) => ({ name: r.querySelector(".name").textContent, unread: r.classList.contains("unread"), top: r.offsetTop })));
  const appList = () => page.evaluate(() => window.__TAURI_INTERNALS__.invoke("chat_list"));
  const lateChat = people[9];
  check("that chat starts out with nothing unread", !(await appList()).find((c) => c.jid === lateChat.jid).unread);
  // Dated after this chat was last read but before many newer chats, like a message delivered late.
  await control("incoming", { id: "delivered-late", chat: lateChat.jid, text: "A made-up late message", ts: lateChat.ts + 1 });
  check("a message delivered late makes its chat unread", !!(await until(async () => (await appList()).find((c) => c.jid === lateChat.jid).unread > 0)));
  const listNames = (await appList()).map((c) => c.name);
  const lateAt = listNames.indexOf(lateChat.name);
  check("its chat stays where its time puts it, not at the top", listNames[lateAt - 1] === "Made-up Group 8" && listNames[lateAt + 1] === "Made-up Group 9", JSON.stringify(listNames.slice(Math.max(0, lateAt - 1), lateAt + 2)));
  await scrollList(lateAt * 70 - 200);
  const lateRow = await until(async () => (await order()).find((r) => r.name === lateChat.name));
  check("and the list shows it there, amber", !!lateRow && lateRow.top === lateAt * 70 && lateRow.unread, JSON.stringify(lateRow));
  await scrollList(0);
  await sleep(100);
  const inView = await order();
  check("every row is in the app's order, newest first, with read and unread mixed", inView.length > 5 && inView.every((r, k) => r.name === listNames[k] && r.top === k * 70) && new Set(inView.map((r) => r.unread)).size === 2, JSON.stringify(inView.slice(0, 4)));
  const newestFirst = await appList();
  check("the app's list is newest first", newestFirst.every((c, k) => k === 0 || newestFirst[k - 1].last_ms >= c.last_ms));
  await page.getByRole("tab", { name: /^Big/ }).click();
  await until(async () => (await heads())[0] === "People");
  await scrollList(0);
  await sleep(100);
  const inHeading = (await order()).filter((r) => r.name.startsWith("Made-up Person")).map((r) => r.name);
  const headingWant = newestFirst.filter((c) => c.name.startsWith("Made-up Person") && big[c.jid] === "a").map((c) => c.name).slice(0, inHeading.length);
  check("inside a custom group's heading, newest first too", inHeading.length > 3 && inHeading.join() === headingWant.join(), `${inHeading.join()} | ${headingWant.join()}`);
  await page.locator("#search").fill("Made-up Person 1");
  await until(() => page.locator(".results .row").first().isVisible());
  const searchNames = await page.locator(".results .row .name").allTextContents();
  await page.locator("#search").fill("");
  const searchWant = (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("chat_list"))).filter((c) => c.name.toLowerCase().includes("made-up person 1")).slice(0, 8).map((c) => c.name);
  check("search keeps its own order, newest first", searchNames.join() === searchWant.join(), `${searchNames.join()} | ${searchWant.join()}`);

  // An unread chat out of view glows along that edge of the list, on the rows' beat, and a click scrolls to it.
  await page.getByRole("tab", { name: /^All/ }).click();
  await scrollList(0);
  const glowBelow = page.locator(".side .edge.below");
  const glowAbove = page.locator(".side .edge.above");
  // Which unread chats are more than half out of view, worked out from the app's own list.
  const outOfView = () =>
    page.evaluate(async () => {
      const list = document.querySelector(".side .list");
      const all = await window.__TAURI_INTERNALS__.invoke("chat_list");
      const tops = all.map((c, k) => (c.unread > 0 ? { name: c.name, top: k * 70 } : null)).filter(Boolean);
      return { below: tops.filter((u) => u.top + 35 > list.scrollTop + list.clientHeight), above: tops.filter((u) => u.top + 35 < list.scrollTop) };
    });
  const fullyInView = (name) =>
    page.evaluate((n) => {
      const list = document.querySelector(".side .list").getBoundingClientRect();
      const row = [...document.querySelectorAll(".side .row.abs")].find((r) => r.querySelector(".name").textContent === n);
      if (!row) return false;
      const box = row.getBoundingClientRect();
      return box.top >= list.top - 1 && box.bottom <= list.bottom + 1;
    }, name);
  const chatsLabel = (n, where) => `${n} unread chat${n === 1 ? "" : "s"} further ${where}`;
  let gone = await outOfView();
  check("with unread chats further down, the bottom edge glows", gone.below.length > 0 && !!(await until(() => glowBelow.isVisible())), JSON.stringify(gone.below.length));
  check("and the top edge doesn't, at the top", gone.above.length === 0 && (await glowAbove.count()) === 0);
  check("it says how many, for a screen reader", (await glowBelow.getAttribute("aria-label")) === chatsLabel(gone.below.length, "down"), await glowBelow.getAttribute("aria-label"));
  const edgeFit = await page.evaluate(() => {
    const list = document.querySelector(".side .list");
    const l = list.getBoundingClientRect();
    const g = document.querySelector(".side .edge.below").getBoundingClientRect();
    return { bottom: Math.abs(g.bottom - l.bottom) < 1, left: Math.abs(g.left - l.left) < 1, right: Math.abs(g.right - (l.right - (list.offsetWidth - list.clientWidth))) < 1, height: Math.round(g.height) };
  });
  check("the glow runs along the bottom edge, short of the scrollbar", edgeFit.bottom && edgeFit.left && edgeFit.right && edgeFit.height <= 24, JSON.stringify(edgeFit));
  const beats = await page.evaluate(() => {
    const anims = document.querySelector(".side").getAnimations({ subtree: true }).filter((a) => a.animationName === "unread-pulse");
    const edge = anims.filter((a) => a.effect.target.classList.contains("edge"));
    return { edge: edge.length, running: edge.every((a) => a.playState === "running"), phases: new Set(anims.map((a) => Math.round((a.currentTime % 1600) / 40))).size, onlyOpacity: edge.every((a) => a.effect.getKeyframes().every((k) => Object.keys(k).filter((q) => !["offset", "computedOffset", "easing", "composite"].includes(q)).join() === "opacity")) };
  });
  check("it blinks on the same beat as the unread rows, animating only opacity", beats.edge === 1 && beats.running && beats.phases === 1 && beats.onlyOpacity, JSON.stringify(beats));
  await page.screenshot({ path: path.join(shotDir, "46-edge-glow-bottom.png") });
  const nextDown = gone.below[0].name;
  await glowBelow.click();
  check("clicking it scrolls to the next unread chat down", !!(await until(() => fullyInView(nextDown))), nextDown);
  await scrollList(1e7);
  await sleep(150);
  gone = await outOfView();
  check("at the bottom, the top edge glows for the unread chats above", gone.above.length > 0 && !!(await until(() => glowAbove.isVisible())) && (await glowAbove.getAttribute("aria-label")) === chatsLabel(gone.above.length, "up"), JSON.stringify(gone.above.length));
  check("and the bottom edge only glows if one is still below", (await glowBelow.count()) === (gone.below.length > 0 ? 1 : 0), JSON.stringify(gone.below.length));
  await page.screenshot({ path: path.join(shotDir, "47-edge-glow-top.png") });
  const nextUp = gone.above[gone.above.length - 1].name;
  await glowAbove.click();
  check("clicking the top glow scrolls up to the nearest unread chat", !!(await until(() => fullyInView(nextUp))), nextUp);
  await scrollList(0);
  await until(() => glowBelow.isVisible());
  const edgeSteady = () => glowBelow.evaluate((g) => ({ animation: getComputedStyle(g, "::before").animationName, opacity: Number(getComputedStyle(g, "::before").opacity) }));
  await page.emulateMedia({ reducedMotion: "reduce" });
  await sleep(150);
  const edgeReduced = await edgeSteady();
  check("with fewer animations in Windows, the glow is steady and strong", edgeReduced.animation === "none" && edgeReduced.opacity === 1, JSON.stringify(edgeReduced));
  await page.emulateMedia({ reducedMotion: "no-preference" });
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByLabel(/Make unread chats blink/).uncheck();
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await until(() => glowBelow.isVisible());
  const edgeOff = await edgeSteady();
  check("with the blink setting off, the glow is steady too", edgeOff.animation === "none" && edgeOff.opacity === 1, JSON.stringify(edgeOff));
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByLabel(/Make unread chats blink/).check();
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await until(() => glowBelow.isVisible());
  check("and it blinks again when the setting is back on", (await edgeSteady()).animation === "unread-pulse");

  const unreadTab = page.getByRole("tab", { name: /^Unread/ });
  await unreadTab.click();
  const unreadExpected = (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("chat_list"))).filter((c) => c.unread > 0).length;
  const tabNumber = Number(await unreadTab.locator(".chat-count").innerText());
  check("the Unread tab counts every chat with unread messages", tabNumber === unreadExpected && unreadExpected > 0, `${tabNumber} ${unreadExpected}`);
  await until(async () => (await order()).length > 0);
  check("the Unread tab only lists unread chats", (await order()).every((r) => r.unread));
  // The off-screen test window never has focus, so reading is done for it here.
  const markRead = (jid) => page.evaluate((chat) => window.__TAURI_INTERNALS__.invoke("mark_read", { chat }), jid);
  await rowNamed(lateChat.name).click();
  await markRead(lateChat.jid);
  const stays = await until(async () => {
    const row = rowNamed(lateChat.name);
    return (await row.count()) === 1 && !(await row.evaluate((r) => r.classList.contains("unread")));
  });
  check("a chat stays in Unread while it's open, even once read", !!stays);
  const movedTo = (await order()).find((r) => r.name !== lateChat.name && r.unread).name;
  await rowNamed(movedTo).click();
  await markRead((await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("chat_list"))).find((c) => c.name === movedTo).jid);
  check("it leaves once you switch chats", !!(await until(async () => (await rowNamed(lateChat.name).count()) === 0)));
  await page.getByRole("tab", { name: /^All/ }).click();
  await page.getByRole("tab", { name: /^Unread/ }).click();
  await sleep(150);
  check("and one you've read doesn't come back when you return to the tab", (await rowNamed(movedTo).count()) === 0);
  const toRead = (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("chat_list"))).filter((c) => c.unread > 0);
  for (const chat of toRead) await page.evaluate((jid) => window.__TAURI_INTERNALS__.invoke("mark_read", { chat: jid }).catch(() => {}), chat.jid);
  await page.getByRole("tab", { name: /^All/ }).click();
  await page.getByRole("tab", { name: /^Unread/ }).click();
  check("an empty Unread tab says so", !!(await until(() => page.getByText("No unread chats").isVisible(), 8000)), String(await unreadTab.locator(".chat-count").innerText()));
  await page.getByRole("tab", { name: /^All/ }).click();
  await scrollList(0);
  await sleep(150);
  check("with everything read, neither edge glows", (await page.locator(".side .edge").count()) === 0);
  const tabsFitAt = await page.evaluate(() => {
    const side = document.querySelector(".side");
    side.style.setProperty("--side-width", "260px");
    const box = side.getBoundingClientRect();
    const tabs = [...document.querySelectorAll('.side [role="tab"]')];
    const fit = { width: Math.round(box.width), inside: tabs.every((b) => b.getBoundingClientRect().right <= box.right + 0.5), cut: tabs.filter((b) => b.scrollWidth > b.clientWidth + 1).length, order: tabs.slice(0, 5).map((b) => b.textContent.trim().split(/\s+/)[0]).join() };
    side.style.removeProperty("--side-width");
    return fit;
  });
  check("the top tabs fit at the narrowest sidebar, in the right order", tabsFitAt.width === 260 && tabsFitAt.inside && tabsFitAt.cut === 0 && tabsFitAt.order === "All,People,Groups,Unread,Unsorted", JSON.stringify(tabsFitAt));
  await page.setViewportSize({ width: 1180, height: 780 });

  // Square groups and round people, everywhere.
  await page.getByRole("tab", { name: /^All/ }).click();
  await scrollList(0);
  await sleep(200);
  const radius = (row) => row.locator(".avatar").first().evaluate((a) => getComputedStyle(a).borderRadius);
  const personRow = rowNamed("Made-up Person 2");
  const groupRow2 = rowNamed("Made-up Group 2");
  check("in the list, a group is square and a person is round", (await radius(groupRow2)) === "22%" && (await radius(personRow)) === "50%", `${await radius(groupRow2)} ${await radius(personRow)}`);
  for (const scheme of ["light", "dark"]) {
    await page.emulateMedia({ colorScheme: scheme });
    await sleep(150);
    await page.screenshot({ path: path.join(shotDir, `25-avatars-${scheme}.png`), clip: { x: 56, y: 0, width: 400, height: 500 } });
  }
  await page.emulateMedia({ colorScheme: "light" });
  await groupRow2.click();
  await until(() => page.locator(".convo header .avatar").isVisible());
  check("in a group's header, it's square, and so is its picture button", (await page.locator(".convo header .avatar").evaluate((a) => getComputedStyle(a).borderRadius)) === "22%" && (await page.locator(".convo header .edit").evaluate((a) => getComputedStyle(a).borderRadius)) === "22%");
  await personRow.click();
  await until(async () => (await page.locator(".convo header").innerText()).includes("Made-up Person 2"));
  check("in a person's header, it's round", (await page.locator(".convo header .avatar").evaluate((a) => getComputedStyle(a).borderRadius)) === "50%");
  await page.locator("#search").fill("Made-up");
  await until(() => page.locator(".results .row").first().isVisible());
  const inSearch = await page.locator(".results .row .avatar").evaluateAll((all) => [...new Set(all.map((a) => getComputedStyle(a).borderRadius))].sort());
  check("in search, it's the same", inSearch.join() === "22%,50%", inSearch.join());
  await page.locator("#search").fill("");
  await page.getByRole("button", { name: "Contacts", exact: true }).click();
  await until(() => page.locator(".page .row .avatar").first().isVisible());
  const inContacts = await page.locator(".page .row .avatar").evaluateAll((all) => [...new Set(all.map((a) => getComputedStyle(a).borderRadius))]);
  check("in Contacts, people are round", inContacts.join() === "50%", inContacts.join());
  await page.getByRole("button", { name: "Chats", exact: true }).click();

  // Dragging the chat list wider and narrower.
  const handle = page.locator(".side .resize");
  await until(() => handle.isVisible());
  const widths = () =>
    page.evaluate(() => {
      const side = document.querySelector(".side").getBoundingClientRect();
      const convo = document.querySelector(".convo").getBoundingClientRect();
      const rows = [...document.querySelectorAll(".side .row.abs")].map((r) => Math.round(r.getBoundingClientRect().width));
      const tabs = [...document.querySelectorAll('.side [role="tab"]')];
      return {
        side: Math.round(side.width),
        convo: Math.round(convo.width),
        rowsFit: rows.length > 0 && rows.every((w) => w <= Math.round(side.width)),
        tabsInside: tabs.every((b) => b.getBoundingClientRect().right <= side.right + 0.5),
        tabRowsClipped: [...document.querySelectorAll(".tabs")].some((r) => r.scrollWidth > r.clientWidth + 1),
      };
    });
  const dragTo = async (x) => {
    const box = await handle.boundingBox();
    await page.mouse.move(box.x + box.width / 2, 300);
    await page.mouse.down();
    await page.mouse.move(x, 300, { steps: 30 });
    await page.mouse.up();
    await sleep(100);
  };
  check("the list starts at its usual width", (await widths()).side === 340, JSON.stringify(await widths()));
  await dragTo(56 + 480);
  const dragged = await widths();
  check("dragging the edge resizes the list, and the rows and tabs follow", Math.abs(dragged.side - 480) <= 2 && dragged.rowsFit && dragged.tabsInside && !dragged.tabRowsClipped, JSON.stringify(dragged));
  await page.screenshot({ path: path.join(shotDir, "26-sidebar-wide.png") });
  await dragTo(1100);
  const widest = await widths();
  check("it stops at its maximum, with the chat still usable", widest.side === 640 && widest.convo >= 380, JSON.stringify(widest));
  await dragTo(100);
  const narrowest = await widths();
  check("it stops at its minimum, and the tabs still fit", narrowest.side === 260 && narrowest.tabsInside && !narrowest.tabRowsClipped && narrowest.rowsFit, JSON.stringify(narrowest));
  await page.screenshot({ path: path.join(shotDir, "27-sidebar-narrow.png") });
  const frames = await page.evaluate(async () => {
    const list = document.querySelector(".side");
    let worst = 0;
    let last = performance.now();
    let on = true;
    const tick = () => {
      const now = performance.now();
      worst = Math.max(worst, now - last);
      last = now;
      if (on) requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
    for (let w = 260; w <= 620; w += 6) {
      list.style.setProperty("--side-width", `${w}px`);
      await new Promise((r) => requestAnimationFrame(r));
    }
    on = false;
    return Math.round(worst);
  });
  console.log(`      worst frame while resizing, debug build: ${frames} ms`);
  check("resizing with a long list doesn't stall", frames < 250, `${frames} ms`);
  await handle.dblclick();
  await sleep(100);
  check("double-click goes back to the usual width", (await widths()).side === 340, JSON.stringify(await widths()));
  await handle.focus();
  await page.keyboard.press("ArrowRight");
  await page.keyboard.press("ArrowRight");
  check("the arrow keys resize it too", (await widths()).side === 380, JSON.stringify(await widths()));
  check("the width is saved on this PC", (await page.evaluate(() => localStorage.getItem("wa-sidebar-width"))) === "380");
  await dragTo(1100);
  await page.setViewportSize({ width: 800, height: 600 });
  await sleep(200);
  const small = await widths();
  check("a smaller window keeps the chat usable, whatever width was saved", small.convo >= 380 && small.side + small.convo + 56 <= 801, JSON.stringify(small));
  await page.setViewportSize({ width: 420, height: 480 });
  await sleep(200);
  if (await back.count()) await back.click();
  await sleep(200);
  check("at the minimum window size there's no handle, and the list takes the window", !(await handle.isVisible()) && (await page.locator(".side").evaluate((s) => Math.round(s.getBoundingClientRect().width))) === 364, String(await page.locator(".side").evaluate((s) => s.getBoundingClientRect().width)));
  await page.setViewportSize({ width: 1180, height: 780 });
  await sleep(200);
  await handle.dblclick();

  // Silencing notifications.
  const silence = () => page.evaluate(() => window.__TAURI_INTERNALS__.invoke("silence_state"));
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const group = page.getByRole("group", { name: "Silence notifications" });
  check("Settings offers exactly the six lengths", (await group.getByRole("button").allInnerTexts()).join() === "15 minutes,30 minutes,1 hour,2 hours,3 hours,4 hours", (await group.getByRole("button").allInnerTexts()).join());
  const refusedLength = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("silence_set", { minutes: 5 }).then(() => "accepted", (e) => String(e)));
  check("any other length is refused", refusedLength !== "accepted", refusedLength);
  const before1 = (await silence()).test_toasts;
  await control("incoming", { id: "not-silenced", chat: people[3].jid, text: "A made-up message", ts: Date.now() + 70000 });
  check("not silenced: a message would notify", !!(await until(async () => (await silence()).test_toasts === before1 + 1)));
  const asked = Date.now();
  await group.getByRole("button", { name: "1 hour" }).click();
  check("once silenced, Settings says until when, with a way back", !!(await until(() => group.getByText(/silenced until/).isVisible())) && (await group.getByRole("button", { name: "Turn back on" }).isVisible()));
  const silencedState = await silence();
  check("it stores an end time an hour away", Math.abs(silencedState.until_ms - asked - 3600000) < 15000, JSON.stringify(silencedState.until_ms));
  await page.screenshot({ path: path.join(shotDir, "28-silenced.png") });
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  const unreadBefore = (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("chat_list"))).find((c) => c.jid === people[5].jid).unread;
  await control("incoming", { id: "silenced", chat: people[5].jid, text: "A made-up message", ts: Date.now() + 80000 });
  check("silenced: the message still arrives, and is counted as unread", !!(await until(async () => (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("chat_list"))).find((c) => c.jid === people[5].jid).unread === unreadBefore + 1)));
  await sleep(300);
  check("silenced: no notification", (await silence()).test_toasts === before1 + 1);
  const silencedAttention = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("attention_test", { action: "state" }));
  check("silenced: nothing blinks, and the tray still shows unread", silencedAttention.tray === "steady" && silencedAttention.taskbar === "off", JSON.stringify(silencedAttention));
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await group.getByRole("button", { name: "Turn back on" }).click();
  check("turning it back on early works", !!(await until(async () => (await silence()).until_ms === null)) && !!(await until(() => group.getByRole("button", { name: "1 hour" }).isVisible())));
  const backOn = await until(async () => {
    const s = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("attention_test", { action: "state" }));
    return s.tray === "blink" && s.taskbar === (s.focused ? "off" : "blink") ? s : null;
  });
  check("back on, both blink again", !!backOn);
  await sleep(300);
  check("what was held back isn't shown afterward", (await silence()).test_toasts === before1 + 1);
  await control("incoming", { id: "back-on", chat: people[7].jid, text: "A made-up message", ts: Date.now() + 90000 });
  check("after that, messages notify again", !!(await until(async () => (await silence()).test_toasts === before1 + 2)));

  // Mentions and sender names. All made up.
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await page.getByRole("tab", { name: /^All/ }).click();
  await page.evaluate(() => (document.querySelector(".side .list").scrollTop = 0));
  await control("incoming", { id: "mentions", chat: GROUP, text: "Ask @777777777 or @15550000004, not @999999999 or a@777777777. Also @15550000005", ts: Date.now() + 120000, sender: "777777777", sender_jid: "777777777@lid", push_name: "Dana" });
  const groupRowNow = rowNamed("Bingo Committee");
  await until(() => groupRowNow.isVisible());
  const shownPreview = await until(async () => {
    const text = await groupRowNow.locator(".preview").innerText();
    return text.includes("Dana Saved") ? text : null;
  });
  check("the preview line shows names, not IDs", shownPreview && !shownPreview.includes("777777777 ") && shownPreview.includes("@Dana Saved (Made-up)"), String(shownPreview));
  await groupRowNow.click();
  const mentionRow = page.locator(".convo .row", { hasText: "not @999999999" });
  await until(() => mentionRow.isVisible());
  // A row's text can still be drawing just after it appears.
  const mentionText = (await until(async () => await mentionRow.locator(".text").innerText())) ?? "";
  check("a mention by hidden ID shows the saved name", mentionText.startsWith("Ask @Dana Saved (Made-up) or @Dana Saved (Made-up), not"), mentionText);
  check("nothing known keeps the raw text, and an email isn't touched", mentionText.includes("not @999999999 or a@777777777."), mentionText);
  check("mentions are styled", (await mentionRow.locator(".text .mention").count()) === 3);
  check("a name with markup in it stays plain text", (await mentionRow.locator(".text b, .text img").count()) === 0 && mentionText.includes("@<b>Eve</b> &amp; <img src=x>"), mentionText);
  check("the sender shows the saved name, not the profile name or digits", (await mentionRow.locator(".sender").innerText()) === "Dana Saved (Made-up)", await mentionRow.locator(".sender").innerText());
  await page.locator("#search").fill("Dana Saved");
  const foundMention = await until(async () => {
    const hits = await page.locator(".results .hit .snippet").allInnerTexts();
    return hits.find((h) => h.includes("@Dana Saved (Made-up)"));
  });
  check("searching a person's name finds their mentions", !!foundMention, String(foundMention));
  await page.locator("#search").fill("");

  // Who's in a group.
  const memberNames = () => page.locator(".convo header .member-name").allInnerTexts();
  const someMembers = [
    { jid: "11111@lid", phone: "15559990000@s.whatsapp.net", lid: "11111@lid", admin: false },
    { jid: "777777777@lid", admin: true },
    { jid: "15550000002@s.whatsapp.net" },
    { jid: "15550000099@s.whatsapp.net" },
    { jid: "15550000005@s.whatsapp.net" },
    { jid: "444444444@lid" },
    { jid: "15550000006@s.whatsapp.net" },
  ];
  await control("status", { connected: true, logged_in: true, me_pn: "15559990000@s.whatsapp.net", me_lid: "11111@lid" });
  await control("group-facts", { members: someMembers });
  await sleep(300);
  await control("group-changed", { jid: GROUP });
  const named = await until(async () => {
    const names = await memberNames();
    return names.length === 7 ? names : null;
  });
  check("a change to the group refreshes the list by itself", !!named);
  check("a group's header lists everyone, with the best name for each", named?.join("|").includes("Dana Saved (Made-up)") && named?.includes("Bob Plumber") && named?.includes("+15550000099") && named?.includes("Unknown member"), String(named));
  check("you're marked as You, and come first", named?.[0] === "You" && !named?.includes("+15559990000"), String(named));
  check("the total shows", (await page.locator(".convo header .members-count").innerText()).includes("7 people"), await page.locator(".convo header .members-count").innerText());
  const longOne = await page.evaluate(() => {
    const list = document.querySelector(".convo header .members");
    const header = document.querySelector(".convo header");
    return { nameWidth: Math.max(...[...list.querySelectorAll(".member-name")].map((n) => n.getBoundingClientRect().width)), sideways: list.scrollWidth > list.clientWidth + 1 || header.scrollWidth > header.clientWidth + 1 };
  });
  check("a very long name shortens and doesn't break the header", longOne.nameWidth <= 281 && !longOne.sideways, JSON.stringify(longOne));
  check("admins are marked", (await page.locator(".convo header .member", { hasText: "Dana Saved" }).locator("small").innerText()) === "admin");
  check("names are plain text", (await page.locator(".convo header .member b, .convo header .member img").count()) === 0 && named.includes("<b>Eve</b> &amp; <img src=x>"), String(named));
  const getsBefore = (await relayLog()).membersGets;
  await control("incoming", { id: "no-refetch", chat: GROUP, text: "A made-up message", ts: Date.now() + 121000 });
  await sleep(600);
  check("a message doesn't ask for the members again", (await relayLog()).membersGets === getsBefore);
  await page.screenshot({ path: path.join(shotDir, "29-members.png") });
  const manyMembers = Array.from({ length: 500 }, (_, i) => ({ jid: `1555300${String(i).padStart(4, "0")}@s.whatsapp.net` }));
  await control("group-facts", { members: [{ jid: "15550000003@s.whatsapp.net" }, ...manyMembers] });
  await control("group-changed", { jid: GROUP });
  await until(async () => (await page.locator(".convo header .members-count").innerText()).includes("501"));
  for (const [width, height] of [[1180, 780], [420, 480]]) {
    await page.setViewportSize({ width, height });
    await sleep(250);
    const room = await page.evaluate(() => {
      const list = document.querySelector(".convo header .members");
      return { list: list.clientHeight, scrolls: list.scrollHeight > list.clientHeight, messages: document.querySelector(".convo .scroller").clientHeight, sideways: list.scrollWidth > list.clientWidth + 1 };
    });
    await page.screenshot({ path: path.join(shotDir, `30-members-many-${width}.png`) });
    check(`500 members at ${width} wide: it scrolls, and messages keep room`, room.list <= 171 && room.scrolls && room.messages >= 120 && !room.sideways, JSON.stringify(room));
  }
  await page.setViewportSize({ width: 1180, height: 780 });
  await control("group-facts", { membersFail: true });
  // Read chats can be far down the list, so these are opened through search.
  const openNamed = async (name) => {
    await page.locator("#search").fill(name);
    const found = page.locator(".results .row").filter({ has: page.locator(".name", { hasText: new RegExp(`^${name}$`) }) }).first();
    await until(() => found.isVisible());
    await found.click();
    await page.locator("#search").fill("");
    await until(async () => (await page.locator(".convo header .name").innerText()) === name);
  };
  await openNamed("Bob Plumber");
  check("one-to-one headers don't change", (await page.locator(".convo header .members").count()) === 0 && (await page.locator(".convo header .sub").innerText()).includes("+15550000002"));
  await openNamed("Bingo Committee");
  check("if it can't refresh, the saved list still shows, marked as such", !!(await until(async () => (await page.locator(".convo header .members-count").innerText()).includes("couldn't refresh"))) && (await memberNames()).length === 501);
  await control("group-facts", { membersFail: false, members: someMembers });
  await control("group-changed", { jid: GROUP });
  await until(async () => (await memberNames()).length === 7);

  // @mentions while typing. All made up, against the fake relay; nothing here reaches WhatsApp.
  {
    const box = page.getByLabel("Message", { exact: true });
    const list = page.getByRole("listbox", { name: "People to mention" });
    const options = async () => (await list.locator(".option-name").allInnerTexts()).map((s) => s.trim());
    const said = page.locator(".composer .mentioning");
    const sentLog = async () => (await relayLog()).sent;
    const nextSent = async (count) => until(async () => ((await sentLog()).length === count + 1 ? (await sentLog()).at(-1) : null));
    const digitsIn = (text) => [...text.matchAll(/(?<![\p{L}\p{N}])@(\d+)/gu)].map((m) => m[1]);
    const agrees = (sent) => (sent.mentions ?? []).every((j) => digitsIn(sent.text).includes(j.split("@")[0]));

    await box.click();
    await box.fill("");
    await box.pressSequentially("Hi @");
    check("@ in a group opens the member list", !!(await until(() => list.isVisible())));
    const everyone = await options();
    check("everyone but you is offered", everyone.length === 6 && !everyone.includes("You"), JSON.stringify(everyone));
    check("someone with only a hidden ID is offered, by the app's name for them", everyone.includes("Unknown member") && everyone.includes("Dana Saved (Made-up)"), JSON.stringify(everyone));
    check("each one has a picture or initials", (await list.locator(".option .avatar").count()) === 6);
    await page.screenshot({ path: path.join(shotDir, "40-mention-picker.png") });
    await box.pressSequentially("PLU");
    check("typing filters by any part of a name, any case", !!(await until(async () => JSON.stringify(await options()) === '["Bob Plumber"]')), JSON.stringify(await options()));
    await page.keyboard.press("Escape");
    check("Escape closes it and keeps the text", !(await list.isVisible()) && (await box.inputValue()) === "Hi @PLU");
    await box.pressSequentially("m");
    check("it stays closed for that @ after Escape", !(await list.isVisible()));
    await box.pressSequentially("x");
    check("no match, no list", !(await list.isVisible()));

    const before = (await sentLog()).length;
    await box.fill("");
    await box.pressSequentially("Hi @dana");
    await until(() => list.isVisible());
    await page.keyboard.press("Enter");
    check("Enter picks the person and doesn't send", (await box.inputValue()) === "Hi @Dana Saved (Made-up) " && (await sentLog()).length === before, await box.inputValue());
    check("it says who will be mentioned", !!(await until(async () => (await said.innerText()).includes("Dana Saved (Made-up)"))));
    await box.pressSequentially("and @");
    await until(() => list.isVisible());
    const second = (await options())[1];
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("Tab");
    check("arrow keys and Tab pick", (await box.inputValue()).endsWith(`@${second} `), `${await box.inputValue()} / ${second}`);
    await box.press("End");
    await sleep(150);
    check("a picked mention doesn't reopen the list", !(await list.isVisible()));
    await box.pressSequentially("mail a@");
    check("an @ inside a word, as in an email address, doesn't open it", !(await list.isVisible()));
    await box.pressSequentially("example.com @bob");
    await until(() => list.isVisible());
    await list.getByRole("option", { name: "Bob Plumber" }).click();
    check("clicking picks", (await box.inputValue()).endsWith("@Bob Plumber "), await box.inputValue());
    await page.keyboard.press("Backspace");
    await page.keyboard.press("Backspace");
    check("Backspace takes a whole mention at once", (await box.inputValue()).endsWith("example.com "), await box.inputValue());
    check("and it's no longer listed as mentioned", !(await said.innerText()).includes("Bob Plumber"));
    await page.keyboard.press("Enter");
    const first = await nextSent(before);
    check("the text sent has numbers, not names", !!first && first.text.startsWith("Hi @777777777 and @") && !first.text.includes("Dana") && first.text.includes("a@example.com"), first?.text);
    check("the mention list is the two people left in the text", first?.mentions?.length === 2 && first.mentions[0] === "777777777@lid" && !first.mentions.includes("15550000002@s.whatsapp.net") && agrees(first), JSON.stringify(first?.mentions));
    const shownRow = page.locator(".convo .row", { hasText: "a@example.com" }).first();
    check("the sent message shows the names", !!(await until(async () => (await shownRow.innerText()).includes("Hi @Dana Saved (Made-up) and @"))), await shownRow.innerText().catch(() => ""));

    // Editing inside a name stops it being a mention, so text and list can't disagree.
    await box.pressSequentially("@dana");
    await until(() => list.isVisible());
    await page.keyboard.press("Enter");
    await page.keyboard.press("Home");
    for (let i = 0; i < 3; i++) await page.keyboard.press("ArrowRight");
    await page.keyboard.type("x");
    check("a name edited inside isn't a mention any more", (await said.count()) === 0, await box.inputValue());
    await page.keyboard.press("End");
    await page.keyboard.press("Enter");
    const edited = await nextSent(before + 1);
    check("so it's sent as plain text with no mention", !!edited && edited.text.startsWith("@Daxna Saved") && !(edited.mentions?.length), JSON.stringify(edited));

    // A draft keeps its mentions while another chat is open.
    await box.pressSequentially("Draft for @bob");
    await until(() => list.isVisible());
    await page.keyboard.press("Enter");
    await openNamed("Bob Plumber");
    await page.getByLabel("Message", { exact: true }).pressSequentially("@");
    await sleep(200);
    check("one-to-one chats don't offer mentions", !(await list.isVisible()));
    await page.getByLabel("Message", { exact: true }).fill("");
    await openNamed("Bingo Committee");
    check("coming back, the draft and its mention are still there", (await box.inputValue()) === "Draft for @Bob Plumber " && (await said.innerText()).includes("Bob Plumber"), await box.inputValue());
    await box.press("End");
    await page.keyboard.press("Enter");
    const drafted = await nextSent(before + 2);
    check("and it sends as a mention", drafted?.text === "Draft for @15550000002" && JSON.stringify(drafted.mentions) === '["15550000002@s.whatsapp.net"]', JSON.stringify(drafted));

    // With a reply, and someone known only by a hidden ID.
    const target = page.locator(".convo .row", { hasText: "a@example.com" }).first();
    await target.hover();
    await target.getByRole("button", { name: "Reply" }).click();
    await box.pressSequentially("@unkn");
    await until(() => list.isVisible());
    await page.keyboard.press("Escape");
    check("Escape closes the list first, not the reply", (await page.locator(".composer .reply").count()) === 1);
    await box.fill("");
    await box.pressSequentially("@unkn");
    await until(() => list.isVisible());
    await page.keyboard.press("Enter");
    await page.keyboard.press("Enter");
    const replied = await nextSent(before + 3);
    check("a reply carries its quote and the mention", !!replied?.quote?.id && replied.text === "@444444444" && JSON.stringify(replied.mentions) === '["444444444@lid"]', JSON.stringify(replied));

    // A caption on a file.
    const finishedBefore = (await relayLog()).finished.length;
    // Pasted, which goes through the composer's own path. "Send files" would open Windows' file dialog.
    await box.evaluate((el) => {
      const png = Uint8Array.from(atob("iVBORw0KGgoAAAANSUhEUgAAAAoAAAAKCAYAAACNMs+9AAAAFUlEQVR42mNkYPj/n4EIwMRIJBhVSAwAVhQBEhQmF1wAAAAASUVORK5CYII="), (c) => c.charCodeAt(0));
      const data = new DataTransfer();
      data.items.add(new File([png], "made-up.png", { type: "image/png" }));
      el.dispatchEvent(new ClipboardEvent("paste", { clipboardData: data, bubbles: true, cancelable: true }));
    });
    check("a pasted picture attaches", !!(await until(async () => (await page.locator(".composer .att").count()) === 1)));
    await box.pressSequentially("Photo for @bob");
    await until(() => list.isVisible());
    await page.keyboard.press("Enter");
    await page.keyboard.press("Enter");
    const finished = await until(async () => ((await relayLog()).finished.length === finishedBefore + 1 ? (await relayLog()).finished.at(-1) : null), 15000);
    check("a caption carries its mention", finished?.caption === "Photo for @15550000002" && JSON.stringify(finished.mentions) === '["15550000002@s.whatsapp.net"]', JSON.stringify(finished && { caption: finished.caption, mentions: finished.mentions }));
    const noMention = (await sentLog()).slice(0, before).filter((s) => s.mentions);
    check("messages without mentions don't send a list", noMention.length === 0);

    // Members that couldn't be fetched.
    await control("group-facts", { membersFail: true });
    await openNamed("Made-up Group 1");
    await page.getByLabel("Message", { exact: true }).pressSequentially("@");
    check("if the members can't be fetched, it says so", !!(await until(async () => (await list.innerText().catch(() => "")).includes("Couldn't get this group's members"))), await list.innerText().catch(() => "none"));
    await page.keyboard.press("Escape");
    await page.getByLabel("Message", { exact: true }).fill("");
    await control("group-facts", { membersFail: false });

    // The largest group WhatsApp allows.
    await openNamed("Bingo Committee");
    const biggest = Array.from({ length: 1024 }, (_, i) => ({ jid: `1555400${String(i).padStart(4, "0")}@s.whatsapp.net` }));
    await control("group-facts", { members: [{ jid: "11111@lid", phone: "15559990000@s.whatsapp.net", lid: "11111@lid" }, ...biggest] });
    await control("group-changed", { jid: GROUP });
    await until(async () => (await page.locator(".convo header .members-count").innerText()).includes("1025"), 15000);
    await box.fill("");
    let started = Date.now();
    await box.pressSequentially("@");
    await until(() => list.isVisible());
    const opened = Date.now() - started;
    check("with 1,024 members it opens quickly and shows the first 200", (await list.getByRole("option").count()) === 200 && opened < 1500, `${opened} ms`);
    started = Date.now();
    await box.pressSequentially("1555400102");
    const narrowed = await until(async () => ((await options()).length === 4 ? await options() : null), 5000);
    check("and narrows by part of a number quickly", !!narrowed && Date.now() - started < 3000, `${Date.now() - started} ms ${JSON.stringify(narrowed)}`);
    await page.keyboard.press("Escape");
    await box.fill("");
    await control("group-facts", { members: someMembers });
    await control("group-changed", { jid: GROUP });
    await until(async () => (await memberNames()).length === 7);
  }

  // The message menu: right-click, the keyboard, edit, delete, forward and info. All made up, against the fake
  // relay, which applies the bridge's rules. Nothing here reaches WhatsApp.
  {
    const box = page.getByLabel("Message", { exact: true });
    const menu = page.getByRole("menu", { name: "Message options" });
    const items = async () => (await menu.getByRole("menuitem").allInnerTexts()).map((s) => s.trim());
    const rowWith = (text) => page.locator(".convo .row", { hasText: text }).first();
    const changes = async () => (await relayLog()).changes ?? [];
    const toastText = () => page.locator(".toast").innerText().catch(() => "");
    const dialog = page.getByRole("alertdialog");
    await openNamed("Bingo Committee");
    await until(() => rowWith("Photo from last week").isVisible());

    await rowWith("Photo from last week").click({ button: "right" });
    check("right-click opens the message menu", !!(await until(() => menu.isVisible())));
    const theirs = await items();
    check("someone else's file offers what applies, and no edit or info", JSON.stringify(theirs) === JSON.stringify(["Reply", "React", "Forward…", "Copy text", "Save file…", "Delete…"]), JSON.stringify(theirs));
    check("the first option has the keyboard", (await page.evaluate(() => document.activeElement?.textContent?.trim())) === "Reply");
    await page.keyboard.press("ArrowDown");
    check("arrow keys move through it", (await page.evaluate(() => document.activeElement?.textContent?.trim())) === "React");
    await page.keyboard.press("Escape");
    check("Escape closes it", !(await menu.isVisible()));
    const more = rowWith("Photo from last week").getByRole("button", { name: "More options for this message" });
    await more.focus();
    await page.keyboard.press("Enter");
    check("the More button opens it from the keyboard too", !!(await until(() => menu.isVisible())));
    await page.keyboard.press("End");
    check("End reaches Delete", (await page.evaluate(() => document.activeElement?.textContent?.trim())) === "Delete…");
    await page.keyboard.press("Escape");
    check("and focus goes back to the button", await more.evaluate((el) => el === document.activeElement));

    // The reaction picker and the menu draw above every message, inside the window. Rows use content-visibility,
    // which clips whatever a row draws outside itself, so the picker used to show as a thin strip under the
    // message above (The user, 2026-11-09).
    const picker = page.getByRole("menu", { name: "React" });
    const onTop = (loc) =>
      loc.evaluate((box) => {
        const r = box.getBoundingClientRect();
        const inside = r.left >= 0 && r.top >= 0 && r.right <= innerWidth && r.bottom <= innerHeight;
        const hits = [...box.querySelectorAll("button")].map((b) => {
          const c = b.getBoundingClientRect();
          return box.contains(document.elementFromPoint(c.left + c.width / 2, c.top + c.height / 2));
        });
        return inside && hits.length > 0 && hits.every(Boolean);
      });
    const lastRow = page.locator(".convo .row").last();
    await lastRow.scrollIntoViewIfNeeded().catch(() => {});
    await lastRow.hover();
    await lastRow.getByRole("button", { name: "React", exact: true }).click();
    check("the reaction picker opens from the last message", !!(await until(() => picker.isVisible())));
    check("every reaction in it is on top of the messages and inside the window", await onTop(picker));
    const pickBox = await picker.boundingBox();
    const rowBox = await lastRow.boundingBox();
    check("it opens above the message when there's room", !!pickBox && !!rowBox && pickBox.y + pickBox.height <= rowBox.y + rowBox.height);
    await picker.getByRole("menuitem", { name: "😮" }).click();
    const reacted = await until(async () => ((await relayLog()).react?.emoji === "😮" ? (await relayLog()).react : null));
    check("picking one reacts, and the picker closes", !!reacted && !(await picker.isVisible()), JSON.stringify(reacted));
    await lastRow.hover();
    await lastRow.getByRole("button", { name: "React", exact: true }).click();
    await until(() => picker.isVisible());
    await page.keyboard.press("Escape");
    check("Escape closes the picker", !!(await until(async () => !(await picker.isVisible()))));
    const middle = page.locator(".convo .row", { hasText: "Photo from last week" }).first();
    await middle.click({ button: "right" });
    await until(() => menu.isVisible());
    check("the right-click menu is on top too", await onTop(menu));
    await menu.getByRole("menuitem", { name: "React" }).click();
    check("React in the menu opens the picker, on top", !!(await until(() => picker.isVisible())) && (await onTop(picker)));
    await page.mouse.click(5, 5);
    check("a click elsewhere closes it", !!(await until(async () => !(await picker.isVisible()))));
    await lastRow.hover();
    await lastRow.getByRole("button", { name: "More options for this message" }).click();
    await until(() => menu.isVisible());
    check("the More menu from the last message is on top and inside the window", await onTop(menu));
    await page.keyboard.press("Escape");

    // Copy text, with names for the mentions.
    await page.evaluate(() => {
      window.__copied = [];
      navigator.clipboard.writeText = async (s) => void window.__copied.push(s);
    });
    await rowWith("not @999999999").click({ button: "right" });
    await menu.getByRole("menuitem", { name: "Copy text" }).click();
    const copied = await page.evaluate(() => window.__copied.at(-1));
    check("Copy text copies what's shown, with names", copied?.startsWith("Ask @Dana Saved (Made-up) or") && copied.includes("not @999999999"), copied);

    // Editing a message of the user's, still in the 15 minutes.
    await box.fill("unsent draft");
    const mine = rowWith("Draft for @Bob Plumber");
    await mine.click({ button: "right" });
    const ownItems = await items();
    check("a recent message of yours offers Edit and Message info", ownItems.includes("Edit") && ownItems.includes("Message info") && ownItems.includes("Forward…"), JSON.stringify(ownItems));
    await menu.getByRole("menuitem", { name: "Edit" }).click();
    check("Edit puts the text in the box, with names back", (await box.inputValue()) === "Draft for @Bob Plumber" && (await page.locator(".composer").innerText()).includes("Edit message"), await box.inputValue());
    check("and its mention still counts", (await page.locator(".composer .mentioning").innerText()).includes("Bob Plumber"));
    await page.keyboard.press("Escape");
    check("Escape cancels and brings the draft back", (await box.inputValue()) === "unsent draft" && !(await page.locator(".composer").innerText()).includes("Edit message"));
    await mine.click({ button: "right" });
    await menu.getByRole("menuitem", { name: "Edit" }).click();
    await box.press("End");
    await box.pressSequentially(" now");
    await page.keyboard.press("Enter");
    const edit = await until(async () => (await changes()).find((c) => c.path === "/api/edit"));
    check("saving sends the edit with the mention", edit?.text === "Draft for @15550000002 now" && JSON.stringify(edit.mentions) === '["15550000002@s.whatsapp.net"]', JSON.stringify(edit));
    check("the message shows the new text, marked edited", !!(await until(async () => (await rowWith("Draft for @Bob Plumber now").innerText().catch(() => "")).includes("edited"))));
    check("and the draft is back in the box", (await box.inputValue()) === "unsent draft", await box.inputValue());
    const overlap = await rowWith("Draft for @Bob Plumber now").evaluate((row) => {
      const text = row.querySelector(".text");
      const stamp = row.querySelector(".stamp");
      // The words only: the spacer is there to sit under the time, by design.
      const words = [];
      const walk = document.createTreeWalker(text, NodeFilter.SHOW_TEXT);
      for (let n = walk.nextNode(); n; n = walk.nextNode()) {
        const range = document.createRange();
        range.selectNodeContents(n);
        words.push(...[...range.getClientRects()].filter((r) => r.width > 0));
      }
      const s = stamp.getBoundingClientRect();
      return words.some((w) => w.right > s.left + 1 && w.left < s.right && w.bottom > s.top + 1 && w.top < s.bottom);
    });
    check("the edited label doesn't sit on the text", !overlap);
    await box.fill("");

    // An older message of the user's: no Edit, but Info and Delete for everyone.
    const older = rowWith("See you all there");
    await older.click({ button: "right" });
    const olderItems = await items();
    check("after 15 minutes, Edit isn't offered", !olderItems.includes("Edit") && olderItems.includes("Message info"), JSON.stringify(olderItems));
    await menu.getByRole("menuitem", { name: "Message info" }).click();
    const infoBox = page.getByRole("dialog", { name: "Message info" });
    check("Message info shows the status and says what it can't", !!(await until(() => infoBox.isVisible())) && (await infoBox.innerText()).includes("Delivered") && (await infoBox.innerText()).includes("only has the overall status"));
    await infoBox.getByRole("button", { name: "Close" }).click();

    await older.click({ button: "right" });
    await menu.getByRole("menuitem", { name: "Delete…" }).click();
    check("deleting asks first, and says what each kind does", !!(await until(() => dialog.isVisible())) && (await dialog.innerText()).includes("stays on your phone") && (await dialog.innerText()).includes("This message was deleted"));
    await page.screenshot({ path: path.join(shotDir, "41-delete-dialog.png") });
    await dialog.getByRole("button", { name: "Cancel" }).click();
    check("Cancel deletes nothing", (await changes()).filter((c) => c.path !== "/api/edit").length === 0 && (await older.isVisible()));
    await older.click({ button: "right" });
    await menu.getByRole("menuitem", { name: "Delete…" }).click();
    await dialog.getByRole("button", { name: "Delete for everyone" }).click();
    check("Delete for everyone leaves the deleted marker", !!(await until(async () => (await page.locator(".convo .row .gone").count()) > 0)) && (await changes()).some((c) => c.path === "/api/revoke"));

    // Someone else's message: Delete for me only.
    const other = rowWith("Old row with no real sender");
    await other.click({ button: "right" });
    await menu.getByRole("menuitem", { name: "Delete…" }).click();
    await until(() => dialog.isVisible());
    check("someone else's message can't be deleted for everyone", (await dialog.getByRole("button", { name: "Delete for everyone" }).count()) === 0);
    await dialog.getByRole("button", { name: "Delete for me" }).click();
    check("Delete for me removes it here", !!(await until(async () => (await page.locator(".convo .row", { hasText: "Old row with no real sender" }).count()) === 0)) && (await changes()).some((c) => c.path === "/api/hide"));

    // Forwarding.
    await rowWith("Photo from last week").click({ button: "right" });
    await menu.getByRole("menuitem", { name: "Forward…" }).click();
    const fwd = page.getByRole("dialog", { name: "Forward message" });
    check("Forward opens a chat picker that says the limit", !!(await until(() => fwd.isVisible())) && (await fwd.innerText()).includes("Up to 5 chats"));
    const search = fwd.getByLabel("Search chats to forward to");
    await search.fill("Made-up Person");
    const boxes = fwd.locator(".chat input[type=checkbox]");
    for (let i = 0; i < 5; i++) await boxes.nth(i).check();
    check("a sixth chat can't be picked", await boxes.nth(5).isDisabled());
    await page.screenshot({ path: path.join(shotDir, "42-forward.png") });
    for (let i = 2; i < 5; i++) await boxes.nth(i).uncheck();
    await fwd.getByRole("button", { name: "Forward", exact: true }).click();
    const fw = await until(async () => (await relayLog()).forwards?.at(-1));
    check("it forwards to the chats picked", fw?.to?.length === 2 && fw.id === "g2", JSON.stringify(fw));
    check("and says where it went, by name", !!(await until(async () => /^Forwarded to Made-up Person \d+, Made-up Person \d+\.$/.test(await toastText()))), await toastText());

    await control("group-facts", { forwardRoom: 1 });
    await rowWith("Photo from last week").click({ button: "right" });
    await menu.getByRole("menuitem", { name: "Forward…" }).click();
    await search.fill("Made-up Person");
    await boxes.nth(0).check();
    await boxes.nth(1).check();
    await fwd.getByRole("button", { name: "Forward", exact: true }).click();
    check("a limit names the chat it left out", !!(await until(async () => (await toastText()).includes("Not sent to Made-up Person") && (await toastText()).includes("forwarding limit"))), await toastText());
    await control("group-facts", { forwardRoom: null });

    // A reply to a row with no recorded sender is refused, and says why.
    await page.locator(".convo .row").first().hover();
    const stored = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("send_text", { chat: "120363000000000001@g.us", text: "reply", quote: { id: "g3", sender: "", text: "x" }, clientId: "e2e-g3" }).then(() => "sent", (e) => String(e)));
    check("a quote with no known sender isn't sent, and says why", stored.includes("no sender recorded"), stored);
    // Contact cards. All made up, against the fake relay, which keeps the bridge's rules.
    {
      const cardBox = page.getByRole("dialog", { name: /contact card|Bob/i });
      const field = (label) => cardBox.getByLabel(label, { exact: true });
      await openNamed("Bob Plumber");
      await page.getByRole("button", { name: /^Chat options for/ }).click();
      await page.getByRole("menuitem", { name: "Contact card…" }).click();
      check("a person's chat opens a new card for them", !!(await until(() => page.getByRole("dialog", { name: "New contact card" }).isVisible())));
      const fresh = page.getByRole("dialog", { name: "New contact card" });
      check("it starts with their name, number and the link", (await fresh.getByLabel("Name shown").inputValue()) === "Bob Plumber" &&
        (await fresh.getByLabel("Phone numbers: value").first().inputValue()) === "+15550000002" && (await fresh.innerText()).includes("Unlink"));
      check("it says where it's kept and that it doesn't sync to the phone", (await fresh.innerText()).includes("doesn't sync to your phone"));
      await fresh.getByLabel("Name shown").fill("Bob the Plumber (Card)");
      await fresh.getByLabel("Company").fill("Made-up Plumbing");
      await fresh.getByLabel("Title").fill("Owner");
      await fresh.getByRole("button", { name: "+ Add an email" }).click();
      await fresh.getByLabel("Email addresses: value").fill("bob@example.com");
      await fresh.getByRole("button", { name: "+ Add an address" }).click();
      await fresh.getByLabel("City").fill("Sampletown");
      await fresh.getByRole("button", { name: "+ Add a date" }).click();
      await fresh.getByLabel("Dates: label").fill("birthday");
      await fresh.getByLabel("Dates: value").fill("--05-31");
      await fresh.getByLabel("Notes").fill("Made-up note <b>not markup</b>");
      await page.screenshot({ path: path.join(shotDir, "43-card.png") });
      await fresh.getByRole("button", { name: "Save" }).click();
      const saved = await until(async () => (await relayLog()).cards?.filter((c) => c.path === "/api/cards/save").at(-1)?.body);
      check("saving sends every field, linked to them", saved?.card.company === "Made-up Plumbing" && saved.card.emails[0].value === "bob@example.com" &&
        saved.card.addresses[0].city === "Sampletown" && saved.card.dates[0].value === "--05-31" && saved.card.links[0] === "15550000002@s.whatsapp.net" && saved.expect === undefined && saved.expect_version === 0, JSON.stringify(saved));
      check("and the card's name is used for them", !!(await until(async () => (await page.locator(".convo header .name").innerText()) === "Bob the Plumber (Card)", 8000)), await page.locator(".convo header .name").innerText());

      // The cards list, searched by any detail.
      await page.getByRole("button", { name: "Contacts", exact: true }).click();
      await page.getByRole("tab", { name: /^Cards/ }).click();
      const cardSearch = page.getByLabel("Search contact cards");
      const rows = () => page.locator(".card-row .name").allInnerTexts();
      for (const [q, want] of [["made-up plumbing", 1], ["sampletown", 1], ["(555) 000-0002", 1], ["nothing like it", 0]]) {
        await cardSearch.fill(q);
        check(`cards are found by "${q}"`, !!(await until(async () => (await rows()).length === want)), JSON.stringify(await rows()));
      }
      await cardSearch.fill("");
      check("markup in a card stays text", !(await page.locator(".card-row b").count()));

      // History, undo, delete and bringing back.
      await page.locator(".card-row", { hasText: "Bob the Plumber (Card)" }).click();
      const open = page.getByRole("dialog", { name: "Bob the Plumber (Card)" });
      await until(() => open.isVisible());
      await open.getByLabel("Title").fill("A bad edit");
      await open.getByRole("button", { name: "Save" }).click();
      await page.locator(".card-row", { hasText: "Bob the Plumber (Card)" }).click();
      await until(() => open.isVisible());
      await open.getByRole("button", { name: "History" }).click();
      check("history lists each change and who made it", !!(await until(async () => (await open.locator(".history li").count()) === 2)) && (await open.innerText()).includes("signed in as user@example.com"));
      await open.locator(".history li").first().getByRole("button", { name: "Undo this change" }).click();
      check("a bad edit can be undone", !!(await until(async () => (await open.getByRole("button", { name: "Back to the card" }).isVisible()) && (await relayLog()).cards.some((c) => c.path === "/api/cards/undo"))));
      await open.getByRole("button", { name: "Back to the card" }).click();
      check("and the card is as it was", (await open.getByLabel("Title").inputValue()) === "Owner");
      await open.getByRole("button", { name: "Delete…" }).click();
      check("deleting asks first and says it can come back", (await open.innerText()).includes("can be brought back"));
      await open.getByRole("button", { name: "Delete card" }).click();
      check("a deleted card leaves the list", !!(await until(async () => (await rows()).length === 0)));
      check("and its name stops showing for them", !!(await until(async () => !(await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("contacts"))).some((c) => c.name === "Bob the Plumber (Card)"), 8000)));
      await page.getByRole("button", { name: "Deleted cards…" }).click();
      await page.locator(".gone", { hasText: "Bob the Plumber (Card)" }).getByRole("button", { name: "Bring back" }).click();
      check("a deleted card can be brought back", !!(await until(async () => (await rows()).includes("Bob the Plumber (Card)"))));

      // Linking: an exact number is a match, a name alone needs a yes.
      await page.getByRole("button", { name: "New card" }).click();
      const blankCard = page.getByRole("dialog", { name: "New contact card" });
      await blankCard.getByLabel("Name shown").fill("Sample Person");
      await blankCard.getByRole("button", { name: "Find matching WhatsApp chats" }).click();
      const loose = blankCard.locator(".linked", { hasText: "same name only" });
      check("a name alone is only a possibility", !!(await until(() => loose.isVisible())));
      await loose.getByRole("button", { name: "Link" }).click();
      check("and linking on it asks first", (await blankCard.innerText()).includes("Only the name matches") && (await blankCard.locator(".linked", { hasText: "Unlink" }).count()) === 0);
      await blankCard.getByRole("button", { name: "Cancel" }).click();
      await page.getByRole("button", { name: "New card" }).click();
      await blankCard.getByLabel("Name shown").fill("Number Match");
      await blankCard.getByRole("button", { name: "+ Add a number" }).click();
      await blankCard.getByLabel("Phone numbers: value").fill("555-000-0001");
      await blankCard.getByRole("button", { name: "Find matching WhatsApp chats" }).click();
      const exact = blankCard.locator(".linked", { hasText: "same number" });
      await until(() => exact.isVisible());
      await exact.getByRole("button", { name: "Link" }).click();
      check("an exact number links at once", (await blankCard.getByRole("button", { name: "Unlink" }).count()) === 1);
      await blankCard.getByRole("button", { name: "Save" }).click();
      await until(async () => (await rows()).includes("Number Match"));
      await page.getByRole("button", { name: "New card" }).click();
      await blankCard.getByLabel("Name shown").fill("Second Claim");
      await blankCard.getByRole("button", { name: "+ Add a number" }).click();
      await blankCard.getByLabel("Phone numbers: value").fill("+15550000001");
      await page.evaluate(() => 0);
      const claim = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("card_save", { card: { display: "Second Claim", links: ["15550000001@s.whatsapp.net"] }, expect: 0 }).then(() => "saved", (e) => String(e)));
      check("one person can't be on two cards", claim.includes("already linked to Number Match"), claim);
      await blankCard.getByRole("button", { name: "Cancel" }).click();

      // An edit made elsewhere in the meantime isn't overwritten.
      const stale = await page.evaluate(async () => {
        const all = (await window.__TAURI_INTERNALS__.invoke("cards_get")).cards;
        const c = all.find((x) => x.display === "Number Match");
        await window.__TAURI_INTERNALS__.invoke("card_save", { card: { ...c, title: "first" }, expect: c.version });
        return window.__TAURI_INTERNALS__.invoke("card_save", { card: { ...c, title: "second" }, expect: c.version }).then(() => "saved", (e) => String(e));
      });
      check("an edit made elsewhere since isn't overwritten", stale.includes("changed somewhere else"), stale);

      // Importing a .vcf file, previewed first.
      writeFileSync(pickedVcard, [
        "BEGIN:VCARD", "VERSION:3.0", "FN:Bob Imported", "ORG:Other Plumbing", "EMAIL:bob@example.com", "TEL;TYPE=WORK:+1 555 000 0099", "END:VCARD",
        "BEGIN:VCARD", "VERSION:3.0", "FN:Brand New Person", "TEL;TYPE=CELL:+1 555 000 0077", "END:VCARD",
        "BEGIN:VCARD", "VERSION:3.0", "FN:Unclear One", "EMAIL:bob@example.com", "TEL:555-000-0001", "END:VCARD",
        "BEGIN:VCARD", "VERSION:3.0", "FN:Broken", "EMAIL:not an address", "END:VCARD",
      ].join("\r\n"));
      const before = (await relayLog()).cards.length;
      await page.getByRole("button", { name: "Import vCard…" }).click();
      const imp = page.getByRole("dialog", { name: "Import contact cards" });
      check("an import shows what it will do first", !!(await until(async () => (await imp.innerText()).includes("1 new, 1 matching a card you have (1 differing detail), 1 unclear, 1 unreadable"))), await imp.innerText().catch(() => ""));
      check("nothing is saved by the preview", (await relayLog()).cards.slice(before).every((c) => !c.body.apply));
      await page.screenshot({ path: path.join(shotDir, "44-import.png") });
      await imp.getByLabel(/Replace company/).check();
      await imp.getByRole("button", { name: "Import" }).click();
      check("importing says what it did", !!(await until(async () => (await page.locator(".toast").innerText().catch(() => "")).includes("Imported: 1 new card, 1 updated."))));
      const applied = (await relayLog()).cards.filter((c) => c.path === "/api/cards/import").at(-1).body;
      const first = applied.choices.find((c) => c.index === 0);
      check("only the ticked detail is replaced, and the unclear one skipped", first.action === "merge" && JSON.stringify(first.take) === '["company"]' &&
        applied.choices.find((c) => c.index === 2).action === "skip", JSON.stringify(applied.choices));
      check("and the list shows the new card", !!(await until(async () => (await rows()).includes("Brand New Person"))));

      // Offline, the last copy is shown and nothing can change.
      await control("outage", { mode: "refuse" });
      await page.getByRole("tab", { name: /^WhatsApp/ }).click();
      await page.getByRole("tab", { name: /^Cards/ }).click();
      check("offline, the saved copy shows and can't be changed", !!(await until(async () => (await page.locator("section.page").innerText()).includes("last copy saved here"), 8000)) && (await rows()).includes("Brand New Person") && (await page.getByRole("button", { name: "New card" }).isDisabled()));
      await control("outage", { mode: "" });
      await until(async () => (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("conn_state"))).link === "ok", 15000);
      // Later checks expect people by their own names again.
      await page.evaluate(async () => {
        const all = (await window.__TAURI_INTERNALS__.invoke("cards_get")).cards;
        for (const c of all) await window.__TAURI_INTERNALS__.invoke("card_delete", { id: c.id, expect: c.version });
      });
      await until(async () => (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("contacts"))).some((c) => c.name === "Bob Plumber"), 8000);
      await page.getByRole("button", { name: "Chats", exact: true }).click();
      await openNamed("Bingo Committee");
    }
    const hitsNow = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("search", { query: "Bob Plumber", limit: 200 }));
    check("search lists each message once after edits, deletes and forwards", new Set(hitsNow.map((h) => h.rowid)).size === hitsNow.length, JSON.stringify(hitsNow.map((h) => [h.rowid, h.snippet])));
    const chatsNow = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("chat_list"));
    check("and the chat list each chat once", new Set(chatsNow.map((c) => c.jid)).size === chatsNow.length);
  }

  // The real group photo. Only ever a fake bridge here.
  const pictureButton = page.locator(".convo header .edit");
  const photos = async () => (await relayLog()).groupPhotos ?? [];
  await control("group-facts", { access: { can_change: true, why: "admin" } });
  await pictureButton.click();
  const realItem = page.getByRole("menuitem", { name: "Change real group photo…" });
  check("a group where you may offers the real photo", !!(await until(() => realItem.isVisible())));
  check("the two options are labelled apart", (await page.getByText("On WhatsApp, for everyone").isVisible()) && (await page.getByText("Only in WA Relay, for you").isVisible()) && (await page.getByText("Nothing changes on WhatsApp.").isVisible()));
  await page.screenshot({ path: path.join(shotDir, "31-photo-menu.png") });
  await realItem.click();
  const dialog = page.getByRole("alertdialog");
  check("it asks first, and says everyone will see it and may get a notice", !!(await until(() => dialog.isVisible())) && (await dialog.innerText()).includes("Everyone in Bingo Committee will see the new photo") && (await dialog.innerText()).includes("may show them a notice"), await dialog.innerText());
  await page.screenshot({ path: path.join(shotDir, "32-photo-confirm.png") });
  await dialog.getByRole("button", { name: "Cancel" }).click();
  check("Cancel changes nothing", (await photos()).length === 0 && !(await dialog.isVisible()));
  await pictureButton.click();
  await until(() => realItem.isVisible());
  await realItem.click();
  await dialog.getByRole("button", { name: "Change it for everyone" }).click();
  check("confirming sends it, once, to that group", !!(await until(async () => (await photos()).length === 1)) && (await photos())[0].jid === GROUP && (await photos())[0].bytes === 8, JSON.stringify(await photos()));
  check("it says it's done", !!(await until(() => page.getByRole("alert").getByText("Group photo changed on WhatsApp.").isVisible())));
  await control("group-facts", { photoError: { status: 409, error: "WhatsApp refused the change. You may not be allowed to change this group's photo." } });
  await pictureButton.click();
  await until(() => realItem.isVisible());
  await realItem.click();
  await dialog.getByRole("button", { name: "Change it for everyone" }).click();
  check("a refusal is shown plainly", !!(await until(() => dialog.getByText("WhatsApp refused the change.").isVisible())));
  await page.screenshot({ path: path.join(shotDir, "33-photo-refused.png") });
  await dialog.getByRole("button", { name: "Close" }).click();
  check("a refusal doesn't look like a lapsed login", (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("conn_state"))).need_login === false);
  check("a refused photo doesn't fall back to a custom picture", !(await page.evaluate((jid) => window.__TAURI_INTERNALS__.invoke("pictures_get").then((p) => (p.own ?? []).includes(jid)), GROUP)));
  await control("group-facts", { photoError: null });
  const token2 = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("save_pasted", { bytes: [137, 80, 78, 71], ext: "png" }).then((f) => f.token));
  await page.evaluate(([jid, t]) => window.__TAURI_INTERNALS__.invoke("picture_set", { jid, token: t }), [GROUP, token2]);
  await pictureButton.click();
  await until(() => realItem.isVisible());
  await realItem.click();
  await dialog.getByRole("button", { name: "Change it for everyone" }).click();
  check("if a custom picture is covering it, that's said", !!(await until(() => page.getByRole("alert").getByText(/still showing here/).isVisible())));
  await page.evaluate((jid) => window.__TAURI_INTERNALS__.invoke("picture_remove", { jid }), GROUP);
  for (const [why, says] of [["admins_only", "Only this group's admins"], ["unknown", "Couldn't tell if you may"], ["not_member", "aren't a member"]]) {
    await control("group-facts", { access: { can_change: false, why } });
    await page.mouse.click(700, 500);
    await pictureButton.click();
    check(`${why}: only the custom picture is offered, and it says why`, !!(await until(() => page.getByRole("menu").getByText(says).isVisible())) && !(await realItem.isVisible()) && (await page.getByRole("menuitem", { name: "Choose a picture…" }).isVisible()));
    await page.mouse.click(700, 500);
  }
  await openNamed("Bob Plumber");
  await page.mouse.click(700, 500);
  await pictureButton.click();
  check("a person only gets the custom picture", !!(await until(() => page.getByRole("menuitem", { name: "Choose a picture…" }).isVisible())) && !(await realItem.isVisible()) && !(await page.getByText("On WhatsApp, for everyone").isVisible()));
  await page.mouse.click(700, 500);

  // New groups. Only ever a fake bridge here.
  const newButton = page.getByRole("button", { name: "New group", exact: true });
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  for (const tab of [/^All/, /^Unread/, /^Big/]) {
    await page.getByRole("tab", { name: tab }).click();
    check(`+ New group shows in the ${tab.source.slice(1)} tab`, await newButton.isVisible());
  }
  await page.getByRole("tab", { name: /^All/ }).click();
  await newButton.click();
  const realOption = page.getByRole("menuitem", { name: /New WhatsApp group/ });
  const customOption = page.getByRole("menuitem", { name: /New custom group/ });
  check("the two kinds are labelled apart", (await realOption.innerText()).includes("people you add are notified") && (await customOption.innerText()).includes("Nobody is told"));
  await page.screenshot({ path: path.join(shotDir, "34-new-menu.png") });
  await customOption.click();
  const customDialog = page.getByRole("dialog", { name: "New custom group" });
  await customDialog.getByLabel("Custom group name").fill("Made-up Crew");
  await customDialog.getByLabel("Custom group colour").fill("#112233");
  await customDialog.getByRole("button", { name: "Add group" }).click();
  check("a custom group is made in one small dialog, with its colour", !!(await until(() => page.getByRole("tab", { name: /^Made-up Crew/ }).isVisible())) && (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("groups_get"))).groups.some((g) => g.name === "Made-up Crew" && g.color === "#112233"));
  check("a custom group creates nothing on WhatsApp", ((await relayLog()).groupCreates ?? []).length === 0);
  const groupOfBob = (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("groups_get"))).groups.find((g) => g.name === "Made-up Crew");
  await page.evaluate(([id, jid]) => window.__TAURI_INTERNALS__.invoke("assign", { jids: [jid], group: id }), [groupOfBob.id, DM2]);
  const creates = async () => (await relayLog()).groupCreates ?? [];
  const realDialog = page.getByRole("dialog", { name: "New WhatsApp group" });
  await newButton.click();
  await realOption.click();
  await until(() => realDialog.getByRole("group", { name: "People to add" }).locator("label").first().isVisible());
  const next = realDialog.getByRole("button", { name: "Next" });
  const jobName = "YEC-2607 - S167-T2 Cooling Fan & YEC-2608 - Made-up setting name, 64";
  check("it can't go on with no name or members, so nothing has a default list", await next.isDisabled());
  await realDialog.getByLabel("WhatsApp group name").fill(jobName);
  check("a name alone isn't enough", await next.isDisabled());
  await realDialog.getByLabel("Search your contacts").fill("Carol");
  check("contacts can be searched", (await realDialog.locator(".person").count()) === 1);
  await realDialog.locator(".person", { hasText: "Carol NoChat" }).locator("input").check();
  await realDialog.getByLabel("Search your contacts").fill("");
  await realDialog.getByLabel("Show contacts in").selectOption({ label: "Made-up Crew" });
  check("contacts can be filtered by a custom group", (await realDialog.locator(".person").allInnerTexts()).every((t) => t.includes("Bob Plumber")) && (await realDialog.locator(".person").count()) === 1);
  await realDialog.locator(".person", { hasText: "Bob Plumber" }).locator("input").check();
  await realDialog.getByLabel("Show contacts in").selectOption({ label: "All contacts" });
  check("choices are kept while filtering", (await realDialog.locator(".person input:checked").count()) === 2);
  check("only known contacts are offered, with no way to type a number in", (await realDialog.getByRole("textbox").count()) <= 1 && (await realDialog.getByRole("group", { name: "People to add" }).locator("input:not([type=checkbox])").count()) === 0);
  await page.screenshot({ path: path.join(shotDir, "35-new-pick.png") });
  await realDialog.getByLabel("Also put it in one of your groups").selectOption({ label: "Made-up Crew" });
  await next.click();
  const finalText = await realDialog.innerText();
  check("before creating, it shows the name and members and says they're notified", finalText.includes(jobName) && finalText.includes("These 2 people will be added and notified") && finalText.includes("Carol NoChat") && finalText.includes("Bob Plumber"), finalText);
  check("nothing is created before the confirmation", (await creates()).length === 0);
  await page.screenshot({ path: path.join(shotDir, "36-new-confirm.png") });
  await realDialog.getByRole("button", { name: "Back" }).click();
  check("Back keeps the choices and creates nothing", (await realDialog.locator(".person input:checked").count()) === 2 && (await creates()).length === 0);
  await next.click();
  await control("group-facts", { createError: { status: 422, error: "WhatsApp refused that, and no group was created. The name may be too long." } });
  await realDialog.getByRole("button", { name: "Create group and add them" }).click();
  check("a refusal is shown, in the dialog", !!(await until(() => realDialog.getByRole("alert").getByText("WhatsApp refused that").isVisible())));
  check("a refusal looks like a refusal, not a lapsed login", (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("conn_state"))).need_login === false);
  check("it isn't retried by itself", (await creates()).length === 1);
  await control("group-facts", { createError: null, notAdded: [{ jid: "15550000003@s.whatsapp.net", code: 403 }] });
  const createButton = realDialog.getByRole("button", { name: "Create group and add them" });
  await createButton.dblclick();
  check("it's sent once, with exactly those people and the full name", !!(await until(async () => (await creates()).length === 2)) && (await sleep(300), (await creates()).length === 2) && (await creates())[1].name === jobName && (await creates())[1].members.slice().sort().join() === ["15550000002@s.whatsapp.net", "15550000003@s.whatsapp.net"].join(), JSON.stringify(await creates()));
  const resultView = await until(async () => ((await realDialog.innerText()).includes("not everyone was added") ? realDialog.innerText() : null));
  check("it says exactly who was and wasn't added", !!resultView && (await realDialog.getByRole("list", { name: "Added", exact: true }).innerText()).includes("Bob Plumber") && (await realDialog.getByRole("list", { name: "Not added" }).innerText()).includes("Carol NoChat. Their privacy settings don't allow it."), String(resultView));
  await page.screenshot({ path: path.join(shotDir, "37-new-partial.png") });
  await realDialog.getByRole("button", { name: "Done" }).click();
  check("the new chat opens by itself", !!(await until(async () => (await page.locator(".convo header .name").innerText()) === jobName)), await page.locator(".convo header .name").innerText());
  const newJid = (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("chat_list"))).find((c) => c.name === jobName)?.jid;
  check("it's in the list straight away", !!newJid);
  check("the optional group picker worked", (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("groups_get"))).assign[newJid] === groupOfBob.id);
  await control("group-facts", { notAdded: [] });
  await newButton.click();
  await realOption.click();
  await until(() => realDialog.locator(".person").first().isVisible());
  await realDialog.getByLabel("WhatsApp group name").fill("Made-up small group");
  await realDialog.locator(".person", { hasText: "Bob Plumber" }).locator("input").check();
  await next.click();
  check("one person reads right", (await realDialog.innerText()).includes("This person will be added and notified"));
  await realDialog.getByRole("button", { name: "Create group and add them" }).click();
  check("when everyone's added, it just opens the chat", !!(await until(async () => (await page.locator(".convo header .name").innerText()) === "Made-up small group")) && !(await realDialog.isVisible()));
  await page.setViewportSize({ width: 420, height: 480 });
  if (await back.count()) await back.click();
  await sleep(200);
  check("at the minimum window size, + New group still shows", await newButton.isVisible());
  await newButton.click();
  await realOption.click();
  await sleep(300);
  const fits = await page.evaluate(() => {
    const box = document.querySelector(".new-group.modal, .modal.new-group").getBoundingClientRect();
    return box.left >= 0 && box.right <= innerWidth && box.top >= 0 && box.bottom <= innerHeight;
  });
  check("the dialog fits at the minimum window size", fits);
  await page.screenshot({ path: path.join(shotDir, "38-new-narrow.png") });
  await realDialog.getByRole("button", { name: "Cancel" }).click();
  await page.setViewportSize({ width: 1180, height: 780 });

  // Groups in their own column. All made up.
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  const toggle = page.getByRole("button", { name: "Groups column" });
  check("off by default: one list, as before", (await toggle.getAttribute("aria-pressed")) === "false" && (await page.locator(".column").count()) === 1 && (await page.locator(".side .resize").count()) === 1);
  await toggle.click();
  const peopleCol = page.locator(".column.people");
  const groupsCol = page.locator(".column.groups");
  await until(() => peopleCol.isVisible());
  const columnTabs = (col) => col.locator('[role="tab"]').allInnerTexts();
  const rowNames = (col) => col.locator(".row.abs .name").allInnerTexts();
  const places = () =>
    page.evaluate(() => {
      const box = (s) => document.querySelector(s).getBoundingClientRect();
      const people = box(".column.people");
      const groups = box(".column.groups");
      const chat = box(".convo");
      return { people: Math.round(people.width), groups: Math.round(groups.width), chat: Math.round(chat.width), inOrder: people.right <= groups.left + 1 && groups.right <= chat.left + 1, sideways: [...document.querySelectorAll(".tabs")].some((t) => t.scrollWidth > t.clientWidth + 1) };
    });
  check("on: people on the left, then groups, then the chat", (await places()).inOrder, JSON.stringify(await places()));
  check("each column lists only its kind", (await rowNames(peopleCol)).length > 0 && (await rowNames(peopleCol)).every((n) => !n.startsWith("Made-up Group") && n !== "Bingo Committee") && (await rowNames(groupsCol)).length > 0 && (await rowNames(groupsCol)).every((n) => !n.startsWith("Made-up Person") && !["Bob Plumber", "Sample Person"].includes(n)));
  const firstOfTab = (texts) => texts.map((t) => t.trim().split(/\s+/)[0]);
  check("each column has its own tabs, with no People or Groups tab", firstOfTab(await columnTabs(peopleCol)).slice(0, 3).join() === "All,Unread,Unsorted" && firstOfTab(await columnTabs(groupsCol)).slice(0, 3).join() === "All,Unread,Unsorted", JSON.stringify(await columnTabs(peopleCol)));
  const numberOn = (col, name) => col.getByRole("tab", { name }).locator(".chat-count").innerText().then(Number);
  const allChats = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("chat_list"));
  const groupsTotal = allChats.filter((c) => c.jid.endsWith("@g.us")).length;
  check("tab counts are for that column's kind of chat", (await numberOn(peopleCol, /^All/)) === allChats.length - groupsTotal && (await numberOn(groupsCol, /^All/)) === groupsTotal, `${await numberOn(peopleCol, /^All/)} ${await numberOn(groupsCol, /^All/)} ${allChats.length}`);
  await peopleCol.getByRole("tab", { name: /^Big/ }).click();
  await sleep(100);
  check("each column's selection is its own", (await peopleCol.getByRole("tab", { name: /^Big/ }).getAttribute("aria-selected")) === "true" && (await groupsCol.getByRole("tab", { name: /^All/ }).getAttribute("aria-selected")) === "true");
  check("no headings inside a custom group here", (await page.locator(".list-head").count()) === 0);
  await page.screenshot({ path: path.join(shotDir, "39-two-light.png") });
  await page.emulateMedia({ colorScheme: "dark" });
  await sleep(150);
  await page.screenshot({ path: path.join(shotDir, "40-two-dark.png") });
  await page.emulateMedia({ colorScheme: "light" });
  await peopleCol.getByRole("tab", { name: /^All/ }).click();
  await control("incoming", { id: "two-person", chat: people[21].jid, text: "A made-up message", ts: Date.now() + 200000 });
  await control("incoming", { id: "two-group", chat: chatGroups[21].jid, text: "A made-up message", ts: Date.now() + 200000 });
  await sleep(200);
  const bothColumns = await page.evaluate(() => {
    const beat = (s) => [...document.querySelector(s).getAnimations({ subtree: true })].filter((a) => a.animationName === "unread-pulse").map((a) => Math.round((a.currentTime % 1600) / 40));
    return { people: beat(".column.people"), groups: beat(".column.groups") };
  });
  check("in two columns, both pulse, on the same beat", bothColumns.people.length > 0 && bothColumns.groups.length > 0 && new Set([...bothColumns.people, ...bothColumns.groups]).size === 1, JSON.stringify(bothColumns));
  check("a new message puts its chat at the top, amber, in both columns", !!(await until(async () => (await peopleCol.locator(".row.abs .name").first().innerText()) === people[21].name && (await groupsCol.locator(".row.abs .name").first().innerText()) === chatGroups[21].name)) && (await peopleCol.locator(".row.abs").first().evaluate((r) => r.classList.contains("unread"))));
  await groupsCol.locator(".row.abs").first().click();
  check("opening a group selects it only in its column", !!(await until(async () => (await groupsCol.locator(".row.on").count()) === 1 && (await peopleCol.locator(".row.on").count()) === 0)));
  await page.locator("#search").fill("Made-up");
  check("search is one view over both columns", !!(await until(() => page.locator(".results .row").first().isVisible())) && (await page.locator(".column").count()) === 0 && (await page.locator(".results .row .avatar").evaluateAll((all) => new Set(all.map((a) => getComputedStyle(a).borderRadius)).size)) >= 1);
  await page.locator("#search").fill("");
  await until(() => peopleCol.isVisible());
  const dragHandle = async (col, x) => {
    const box = await col.locator(".resize").boundingBox();
    await page.mouse.move(box.x + box.width / 2, 400);
    await page.mouse.down();
    await page.mouse.move(x, 400, { steps: 20 });
    await page.mouse.up();
    await sleep(120);
  };
  const peopleLeft = (await peopleCol.boundingBox()).x;
  await dragHandle(peopleCol, peopleLeft + 360);
  check("the people column can be dragged", Math.abs((await places()).people - 360) <= 2, JSON.stringify(await places()));
  await dragHandle(peopleCol, peopleLeft + 50);
  check("it stops at its minimum", (await places()).people === 220, JSON.stringify(await places()));
  const groupsLeft = (await groupsCol.boundingBox()).x;
  await dragHandle(groupsCol, groupsLeft + 300);
  check("the groups column can be dragged", Math.abs((await places()).groups - 300) <= 2, JSON.stringify(await places()));
  await dragHandle(groupsCol, 1170);
  const widestPair = await places();
  check("dragging can't squeeze the chat", widestPair.chat >= 380 && !widestPair.sideways, JSON.stringify(widestPair));
  // The groups column first, since a very wide one leaves the people column no room to go back into.
  await groupsCol.locator(".resize").dblclick();
  await peopleCol.locator(".resize").dblclick();
  await sleep(100);
  check("double-click goes back to the usual widths", (await places()).people === 300 && (await places()).groups === 300, JSON.stringify(await places()));
  await peopleCol.locator(".resize").focus();
  await page.keyboard.press("ArrowRight");
  check("the arrow keys work on a column", (await places()).people === 320, JSON.stringify(await places()));
  check("the widths and the setting are saved on this PC", (await page.evaluate(() => [localStorage.getItem("wa-people-width"), localStorage.getItem("wa-two-columns")].join())) === "320,1");
  await peopleCol.locator(".resize").dblclick();
  await page.setViewportSize({ width: 900, height: 600 });
  await sleep(250);
  const atNarrowest = await places();
  check("at the narrowest two-column window, the chat stays usable", atNarrowest.chat >= 380 && atNarrowest.people >= 220 && atNarrowest.groups >= 220 && !atNarrowest.sideways && atNarrowest.inOrder, JSON.stringify(atNarrowest));
  await page.screenshot({ path: path.join(shotDir, "41-two-900.png") });
  await page.setViewportSize({ width: 899, height: 600 });
  await sleep(250);
  const fallback = await page.evaluate(() => ({ columns: document.querySelectorAll(".column").length, all: document.querySelectorAll(".column.all").length, width: innerWidth, toggle: getComputedStyle(document.querySelector(".layout-toggle")).display }));
  check("below that, it falls back to the single list", fallback.columns === 1 && fallback.all === 1 && fallback.toggle === "none", JSON.stringify(fallback));
  await page.setViewportSize({ width: 420, height: 480 });
  await sleep(250);
  if (await back.count()) await back.click();
  await sleep(150);
  check("at the minimum window size, it's the single list", (await page.locator(".column.all").count()) === 1 && (await page.locator(".side").evaluate((s) => Math.round(s.getBoundingClientRect().width))) === 364);
  await page.screenshot({ path: path.join(shotDir, "42-two-min.png") });
  await page.setViewportSize({ width: 1600, height: 900 });
  await sleep(250);
  check("widening brings the two columns back", (await page.locator(".column.people").count()) === 1 && (await page.locator(".column.groups").count()) === 1);
  await page.screenshot({ path: path.join(shotDir, "43-two-1600.png") });
  const drawn = await page.evaluate(() => document.querySelectorAll(".row.abs").length);
  check("both lists only draw the rows near the view", drawn < 60, String(drawn));
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const setting = page.getByLabel(/Show groups in their own column/);
  check("Settings shows the same setting", await setting.isChecked());
  await setting.uncheck();
  await page.getByRole("button", { name: "Chats", exact: true }).click();
  await until(() => page.locator(".column.all").isVisible());
  check("off again: one list, with its own tab kept", (await page.locator(".column").count()) === 1 && (await page.getByRole("tab", { name: /^People/ }).count()) === 1);
  await page.setViewportSize({ width: 1180, height: 780 });

  // Who said what: a colour for each person, a small gap when the person changes, and pictures. All made up.
  {
  const table = JSON.parse(readFileSync(new URL("../src/lib/palettes.json", import.meta.url), "utf8"));
  const setOf = (theme, n) => new Set(table[theme][n - 1].match(/.{6}/g).map((h) => `#${h}`));
  const parseRgb = (css) => css.match(/\d+(\.\d+)?/g).slice(0, 3).map(Number);
  const closestPair = (colours) => {
    let de = Infinity;
    let ok = Infinity;
    const points = colours.map((c) => rgbOf(c));
    for (let i = 0; i < points.length; i++)
      for (let j = i + 1; j < points.length; j++) {
        de = Math.min(de, ciede2000(labOf(points[i]), labOf(points[j])));
        ok = Math.min(ok, oklabDistance(oklabOf(points[i]), oklabOf(points[j])) * 100);
      }
    return { de, ok };
  };
  const TALK = "120363000000000777@g.us";
  const talker = (n) => `1555700${String(n).padStart(4, "0")}@s.whatsapp.net`;
  const from = (n) => ({ sender: talker(n).split("@")[0], sender_jid: talker(n), push_name: `Made-up ${n}` });
  const meInGroup = { jid: "11111@lid", phone: "15559990000@s.whatsapp.net", lid: "11111@lid" };
  // Eight people besides the user. Number 6 never says anything, and number 7's hidden ID is only linked to their
  // number by this list.
  const talkMembers = [meInGroup, { jid: "777777777@lid" }, ...[1, 2, 3, 4, 5, 6].map((n) => ({ jid: talker(n) })), { jid: "55555@lid", phone: talker(7), lid: "55555@lid" }];
  await control("group-facts", { members: talkMembers, membersFail: false });
  const talkStart = Date.now() + 200000;
  await control("add-chats", { chats: [{ jid: TALK, name: "Who Said What", ts: talkStart, read: true, text: "An old line with no sender" }] });
  let talkAt = talkStart;
  const talkLine = (text, who, extra = {}) => ({ id: `talk-${++talkAt}`, chat: TALK, ts: talkAt, text, ...who, ...extra });
  const mine = { from_me: true, sender: "15559990000", status: "read" };
  const dana = { sender: "777777777", sender_jid: "777777777@lid", push_name: "Dana" };
  const danaByNumber = { sender: "15550000004", sender_jid: "15550000004@s.whatsapp.net", push_name: "Dana" };
  await control("add-messages", {
    messages: [
      talkLine("First from one", from(1)),
      talkLine("Second from one", from(1)),
      talkLine("A line from two", from(2)),
      talkLine("A line from me", mine),
      talkLine("Another line from me", mine),
      talkLine("Dana by hidden ID", dana),
      talkLine("Dana by number", danaByNumber),
      talkLine("Three has a lot to say\nover several lines\nof made-up text", from(3)),
      talkLine("", from(4), { media: "sticker", mime: "image/webp", size: 2000, width: 160, height: 160, filename: "s.webp", thumb: "iVBORw0KGgoAAAANSUhEUgAAAAoAAAAKCAYAAACNMs+9AAAAFUlEQVR42mNkYPj/n4EIwMRIJBhVSAwAVhQBEhQmF1wAAAAASUVORK5CYII=" }),
      talkLine("A photo from five", from(5), { media: "image", mime: "image/png", size: 100, width: 300, height: 200, filename: "p.png" }),
      talkLine("Seven by hidden ID", { sender: "55555", sender_jid: "55555@lid", push_name: "Made-up 7" }),
      talkLine("Seven by number", from(7)),
    ],
  });
  await page.getByRole("tab", { name: /^All/ }).click();
  await page.evaluate(() => (document.querySelector(".side .list").scrollTop = 0));
  await until(() => rowNamed("Who Said What").isVisible());
  await rowNamed("Who Said What").click();
  await until(async () => (await page.locator(".convo .bubble").count()) === 13);
  const talkRows = () =>
    page.evaluate(() =>
      [...document.querySelectorAll(".convo .scroller .row")].reverse().map((r) => {
        const box = r.getBoundingClientRect();
        const bubbleEl = r.querySelector(".bubble");
        const bubble = bubbleEl.getBoundingClientRect();
        const avatar = r.querySelector(".face .avatar");
        const name = r.querySelector(".sender");
        let surface = null;
        for (let el = name; el && !surface; el = el.parentElement) {
          const bg = getComputedStyle(el).backgroundColor;
          if (bg !== "rgba(0, 0, 0, 0)" && bg !== "transparent") surface = bg;
        }
        return {
          id: r.id,
          text: r.querySelector(".text")?.innerText.trim() ?? "",
          top: box.top,
          bottom: box.bottom,
          bubbleLeft: bubble.left,
          bubbleRight: bubble.right,
          mine: r.classList.contains("mine"),
          hasFace: !!r.querySelector(".face"),
          faceTitle: r.querySelector(".face")?.title ?? "",
          avatar: avatar && { left: avatar.getBoundingClientRect().left, right: avatar.getBoundingClientRect().right, top: avatar.getBoundingClientRect().top, round: getComputedStyle(avatar).borderRadius, background: getComputedStyle(avatar).backgroundColor, color: getComputedStyle(avatar).color, text: avatar.textContent.trim(), img: avatar.querySelector("img")?.getAttribute("src") ?? "", naturalWidth: avatar.querySelector("img")?.naturalWidth ?? 0 },
          name: name?.innerText ?? "",
          nameColour: name ? getComputedStyle(name).color : "",
          nameSurface: surface,
          who: getComputedStyle(r).getPropertyValue("--who").trim(),
          coded: bubbleEl.classList.contains("coded"),
          shadow: getComputedStyle(bubbleEl).boxShadow,
          faceSelect: r.querySelector(".face") ? getComputedStyle(r.querySelector(".face")).userSelect : "",
        };
      }),
    );
  await sleep(300);
  let talk = await talkRows();
  const line = (text) => talk.find((r) => r.text === text);
  const gapAbove = (text) => {
    const at = talk.findIndex((r) => r.text === text);
    return Math.round((talk[at].top - talk[at - 1].bottom) * 10) / 10;
  };
  check("the same person's messages stay tight", gapAbove("Second from one") <= 1, String(gapAbove("Second from one")));
  check("a different person gets a small gap", gapAbove("A line from two") >= 7 && gapAbove("A line from two") <= 9, String(gapAbove("A line from two")));
  check("so does going from someone else to me", gapAbove("A line from me") >= 7 && gapAbove("A line from me") <= 9, String(gapAbove("A line from me")));
  check("my own messages stay tight", gapAbove("Another line from me") <= 1, String(gapAbove("Another line from me")));
  check("so does going from me to someone else", gapAbove("Dana by hidden ID") >= 7 && gapAbove("Dana by hidden ID") <= 9, String(gapAbove("Dana by hidden ID")));
  check("one person on two addresses stays one run", gapAbove("Dana by number") <= 1 && !line("Dana by number").avatar && !line("Dana by number").name, String(gapAbove("Dana by number")));
  check("and keeps one colour", line("Dana by hidden ID").who === line("Dana by number").who && !!line("Dana by number").who);
  check("a hidden ID only the member list knows is still the same person", gapAbove("Seven by number") <= 1 && line("Seven by hidden ID").who === line("Seven by number").who && !!line("Seven by number").who);
  check("a person's colour is on every one of their messages", line("First from one").who === line("Second from one").who && line("Second from one").coded && line("Second from one").shadow.includes("3px 0px 0px"));
  const seen = (rows) => {
    const map = new Map();
    for (const r of rows) if (r.who) map.set(r.who, true);
    return [...map.keys()];
  };
  check("everyone who has spoken has their own colour", seen(talk).length === 7, seen(talk).join(" "));
  check("the set is the one for everyone in the group, not just who has spoken", seen(talk).every((c) => setOf("light", 8).has(c)), seen(talk).join(" "));
  check("a person's picture is on the left of their first message", line("First from one").avatar?.right <= line("First from one").bubbleLeft && line("First from one").avatar?.round === "50%" && line("First from one").avatar?.top >= line("First from one").top);
  check("later messages have no picture, and line up under the first", !line("Second from one").avatar && line("Second from one").hasFace && line("Second from one").bubbleLeft === line("First from one").bubbleLeft);
  check("everyone's messages start at the same place", new Set(talk.filter((r) => !r.mine).map((r) => r.bubbleLeft)).size === 1, JSON.stringify(talk.filter((r) => !r.mine).map((r) => r.bubbleLeft)));
  check("my own messages have no picture, no colour, and sit on the right", !line("A line from me").hasFace && !line("A line from me").coded && !line("A line from me").who && line("A line from me").bubbleRight > line("A line from two").bubbleRight);
  const unknownRow = talk[0];
  check("an old message with no sender gets a grey stand-in, no colour and no name", unknownRow.text === "An old line with no sender" && !unknownRow.who && !unknownRow.coded && !unknownRow.name && !unknownRow.avatar?.img && unknownRow.faceTitle.includes("doesn't know who"), JSON.stringify(unknownRow));
  check("a picture can't get mixed into copied text", line("First from one").faceSelect === "none");

  // The same picture source as everywhere else, under the address the rest of the app files it by.
  await control("picture", { jid: talker(1), v: 501 });
  await control("picture", { jid: talker(7), v: 502 });
  // Pictures load as they come into view.
  const pictureOn = async (text) => {
    await page.locator(".convo .row", { hasText: text }).scrollIntoViewIfNeeded();
    return until(async () => {
      talk = await talkRows();
      return line(text).avatar?.naturalWidth > 0;
    });
  };
  const firstPicture = await pictureOn("First from one");
  check("a person's picture shows", !!firstPicture && line("First from one").avatar.img.includes(`/avatar/${talker(1)}?v=501`), line("First from one").avatar?.img);
  const hiddenPicture = await pictureOn("Seven by hidden ID");
  check("it's found through a hidden ID too", !!hiddenPicture && line("Seven by hidden ID").avatar.img.includes(`/avatar/${talker(7)}?v=502`), line("Seven by hidden ID").avatar?.img);
  await page.locator(".convo .row", { hasText: "A line from two" }).scrollIntoViewIfNeeded();
  await until(async () => {
    talk = await talkRows();
    return line("A line from two").avatar && !line("A line from two").avatar.img;
  });
  const hex = (css) => `#${parseRgb(css).map((v) => v.toString(16).padStart(2, "0")).join("")}`;
  check("with no picture, it's their initials in their colour", line("A line from two").avatar.text === "M2" && hex(line("A line from two").avatar.background) === line("A line from two").who, JSON.stringify(line("A line from two").avatar));

  // Every theme, measured on what's really behind each name.
  const before = new Map(talk.map((r) => [r.id, r.who]));
  for (const scheme of ["light", "dark"]) {
    await page.emulateMedia({ colorScheme: scheme });
    await sleep(150);
    const rows = await talkRows();
    const named = rows.filter((r) => r.name);
    const worst = Math.min(...named.map((r) => contrastOf(parseRgb(r.nameColour), parseRgb(r.nameSurface))));
    check(`${scheme}: every name reads at 4.5:1 or better on what's behind it, sticker included`, named.length >= 7 && worst >= 4.5, worst.toFixed(2));
    check(`${scheme}: the names really are in each person's colour`, named.filter((r) => r.who).every((r) => hex(r.nameColour) === r.who));
    check(`${scheme}: the colours are that theme's set`, seen(rows).every((c) => setOf(scheme, 8).has(c)), seen(rows).join(" "));
    const initialsRow = rows.find((r) => r.text === "A line from two");
    const onFill = contrastOf(parseRgb(initialsRow.avatar.color), parseRgb(initialsRow.avatar.background));
    check(`${scheme}: initials read on their colour`, onFill >= 4.5, onFill.toFixed(2));
    const measured = closestPair(seen(rows));
    console.log(`      ${scheme}, 7 of 8 people on screen: closest pair CIEDE2000 ${measured.de.toFixed(1)}, OKLab ${measured.ok.toFixed(1)}, worst name contrast ${worst.toFixed(2)}:1`);
    check(`${scheme}: measured on screen, the people are well apart`, measured.de >= 19 && measured.ok >= 14, `${measured.de.toFixed(1)} / ${measured.ok.toFixed(1)}`);
    await page.locator(".convo .scroller").evaluate((el) => (el.scrollTop = 0));
    await page.screenshot({ path: path.join(shotDir, `46-who-said-what-${scheme}.png`) });
  }
  await page.emulateMedia({ colorScheme: "light" });
  await sleep(150);

  // Colours hold still until the people change.
  await control("incoming", talkLine("One more from three", from(3)));
  await until(async () => (await page.locator(".convo .bubble").count()) === 14);
  await sleep(150);
  talk = await talkRows();
  check("a new message from someone known changes nobody's colour", talk.filter((r) => before.has(r.id)).every((r) => r.who === before.get(r.id)));
  await control("incoming", talkLine("Six finally speaks", from(6)));
  await until(async () => (await page.locator(".convo .bubble").count()) === 15);
  await sleep(150);
  talk = await talkRows();
  check("a member speaking for the first time changes nobody's colour", talk.filter((r) => before.has(r.id)).every((r) => r.who === before.get(r.id)) && seen(talk).length === 8 && !!line("Six finally speaks").who);
  const settled = new Map(talk.map((r) => [r.id, r.who]));
  await rowNamed("Bingo Committee").click();
  await until(() => page.locator(".convo .title .name", { hasText: "Bingo Committee" }).isVisible());
  await rowNamed("Who Said What").click();
  await until(async () => (await page.locator(".convo .bubble").count()) === 15);
  await sleep(250);
  talk = await talkRows();
  check("leaving and coming back keeps everyone's colour", talk.every((r) => r.who === settled.get(r.id)));
  await control("group-facts", { members: [...talkMembers, { jid: talker(8) }] });
  await control("group-changed", { jid: TALK });
  const grew = await until(async () => {
    talk = await talkRows();
    return seen(talk).every((c) => setOf("light", 9).has(c)) ? talk : null;
  });
  check("when someone joins, the set changes to the one for that many people", !!grew && seen(talk).length === 8, seen(talk).join(" "));
  await control("incoming", talkLine("Nobody listed me", from(9)));
  await until(async () => (await page.locator(".convo .bubble").count()) === 16);
  await sleep(150);
  talk = await talkRows();
  check("someone who isn't on the list still gets their own colour", seen(talk).length === 9 && seen(talk).every((c) => setOf("light", 10).has(c)), seen(talk).join(" "));

  // One-to-one: the picture still shows, and there's nothing to tell apart.
  await page.locator("#search").fill("Bob Plumber");
  await until(() => page.locator(".results .row", { hasText: "Bob Plumber" }).first().isVisible());
  await page.locator(".results .row", { hasText: "Bob Plumber" }).first().click();
  await page.locator("#search").fill("");
  await until(() => page.locator(".convo .title .name", { hasText: "Bob Plumber" }).isVisible());
  await until(async () => (await page.locator(".convo .bubble").count()) >= 40);
  await sleep(250);
  let oneToOne = await talkRows();
  const theirs = oneToOne.filter((r) => !r.mine);
  const withFace = theirs.filter((r) => r.avatar);
  check("one-to-one: their picture is on the left of their first message", withFace.length >= 1 && withFace.length < theirs.length / 2 && withFace.every((r) => r.avatar.right <= r.bubbleLeft && r.avatar.round === "50%"), `${withFace.length} of ${theirs.length}`);
  check("one-to-one: their initials, not a stand-in", withFace.every((r) => r.avatar.text === "BP"), JSON.stringify(withFace[0]?.avatar));
  check("one-to-one: no names and no colour stripe", oneToOne.every((r) => !r.name && !r.coded));
  const listFill = await page.locator(".side .row.abs").filter({ has: page.locator(".name", { hasText: /^Bob Plumber$/ }) }).locator(".avatar").evaluate((a) => getComputedStyle(a).backgroundColor).catch(() => "");
  check("one-to-one: the same colour as in the chat list", !listFill || listFill === withFace[0]?.avatar.background, `${listFill} ${withFace[0]?.avatar.background}`);
  await control("add-messages", { messages: [{ id: "bob-mine", chat: DM2, ts: Date.now() + 400000, text: "Made-up reply", from_me: true, sender: "15559990000", status: "read" }, { id: "bob-back", chat: DM2, ts: Date.now() + 401000, text: "Made-up answer", sender: DM2.split("@")[0] }] });
  await until(() => page.locator(".convo .row", { hasText: "Made-up answer" }).isVisible());
  await sleep(150);
  oneToOne = await talkRows();
  const oneGap = (text) => {
    const at = oneToOne.findIndex((r) => r.text === text);
    return oneToOne[at].top - oneToOne[at - 1].bottom;
  };
  check("one-to-one: the gap works there too", oneGap("Made-up reply") >= 7 && oneGap("Made-up answer") >= 7 && oneToOne.find((r) => r.text === "Made-up answer").avatar?.right <= oneToOne.find((r) => r.text === "Made-up answer").bubbleLeft);
  await page.screenshot({ path: path.join(shotDir, "47-one-to-one.png") });

  // A long chat at the largest group size seen: 1,500 messages from 64 people, of very different heights.
  const BIG = "120363000000000888@g.us";
  const bigPeople = Array.from({ length: 64 }, (_, i) => talker(100 + i));
  await control("group-facts", { members: [meInGroup, ...bigPeople.map((jid) => ({ jid }))] });
  await control("add-chats", { chats: [{ jid: BIG, name: "Big Made-up Group", ts: Date.now() + 500000, read: true, text: "The newest line" }] });
  const longText = "All of this is made-up filler so that this row is a good deal taller than the ones around it. ".repeat(6);
  const bigStart = Date.now() - 86400000 * 4;
  const bigRows = [];
  let speakerAt = 0;
  for (let i = 0; i < 1500; i++) {
    if (i % 3 !== 1) speakerAt = (speakerAt + 23) % 64;
    const jid = bigPeople[speakerAt];
    const row = { id: `big-${i}`, chat: BIG, ts: bigStart + i * 1000, sender: jid.split("@")[0], sender_jid: jid, push_name: `Made-up ${speakerAt}`, text: i % 9 === 0 ? longText : `Made-up line ${i}` };
    if (i % 17 === 0) Object.assign(row, { media: "image", mime: "image/png", size: 100, width: 300 + (i % 200), height: 160 + (i % 180), filename: `b${i}.png` });
    if (i % 23 === 0) Object.assign(row, { media: "document", mime: "application/pdf", size: 5000, filename: `made-up-${i}.pdf` });
    if (i === 40) row.text = "needle-from-long-ago in a made-up line";
    if (i % 2 === 1 && i % 11 === 0) Object.assign(row, { from_me: true, sender: "15559990000", sender_jid: "", status: "read" });
    bigRows.push(row);
  }
  await control("add-messages", { messages: bigRows });
  check("the whole long chat has arrived", !!(await until(async () => (await page.evaluate((chat) => window.__TAURI_INTERNALS__.invoke("messages_page", { chat, beforeTs: null, beforeRowid: null, limit: 60 }), BIG)).some((m) => m.id === "big-1499"), 30000)));
  const picturesBefore = (await relayLog()).pictureGets ?? 0;
  await page.evaluate(() => (document.querySelector(".side .list").scrollTop = 0));
  await until(() => rowNamed("Big Made-up Group").isVisible());
  const opening = Date.now();
  await rowNamed("Big Made-up Group").click();
  await until(async () => (await page.locator(".convo .bubble").count()) >= 60);
  const openMs = Date.now() - opening;
  console.log(`      opening the long chat, debug build: ${openMs} ms`);
  check("a long chat opens promptly", openMs < 1500, `${openMs} ms`);
  const loading = Date.now();
  const allLoaded = await until(async () => {
    await page.locator(".convo .scroller").evaluate((el) => (el.scrollTop = -el.scrollHeight));
    return (await page.locator(".convo .bubble").count()) >= 1501;
  }, 90000);
  console.log(`      scrolling back through all 1,500, debug build: ${Date.now() - loading} ms`);
  check("scrolling back loads the whole chat", !!allLoaded, String(await page.locator(".convo .bubble").count()));
  const bigWho = await page.evaluate(() => {
    const out = new Map();
    for (const r of document.querySelectorAll(".convo .scroller .row:not(.mine)")) {
      const name = r.querySelector(".sender")?.textContent;
      if (name?.startsWith("Made-up")) out.set(name, getComputedStyle(r).getPropertyValue("--who").trim());
    }
    return [...out];
  });
  const bigColours = new Set(bigWho.map(([, c]) => c));
  check("64 people get 64 different colours", bigWho.length === 64 && bigColours.size === 64, `${bigWho.length} people, ${bigColours.size} colours`);
  check("from the set for 64", [...bigColours].every((c) => setOf("light", 64).has(c)));
  const bigMeasure = closestPair([...bigColours]);
  console.log(`      64 people on screen: closest pair CIEDE2000 ${bigMeasure.de.toFixed(1)}, OKLab ${bigMeasure.ok.toFixed(1)}`);
  const stacked = await page.evaluate(() => {
    const rows = [...document.querySelectorAll(".convo .scroller .row")].map((r) => r.getBoundingClientRect());
    let overlaps = 0;
    for (let i = 1; i < rows.length; i++) if (rows[i].bottom > rows[i - 1].top + 1) overlaps++;
    return { rows: rows.length, overlaps, heights: new Set(rows.map((r) => Math.round(r.height))).size };
  });
  check("rows of very different heights don't overlap", stacked.overlaps === 0 && stacked.rows >= 1500, JSON.stringify(stacked));
  const scrollFrames = () =>
    page.evaluate(async () => {
      const el = document.querySelector(".convo .scroller");
      const gaps = [];
      let last = performance.now();
      const from = el.scrollTop;
      const steps = 240;
      for (let i = 0; i <= steps; i++) {
        el.scrollTop = from * (1 - i / steps);
        await new Promise((r) => requestAnimationFrame(r));
        const now = performance.now();
        gaps.push(now - last);
        last = now;
      }
      gaps.sort((a, b) => a - b);
      return { median: Math.round(gaps[gaps.length >> 1]), worst: Math.round(gaps[gaps.length - 1]) };
    });
  const withPictures = await scrollFrames();
  await page.locator(".convo .scroller").evaluate((el) => (el.scrollTop = -el.scrollHeight));
  await page.addStyleTag({ content: ".face { display: none !important; } .row.gap { margin-top: 0 !important; }" });
  await sleep(300);
  const withoutPictures = await scrollFrames();
  await page.evaluate(() => [...document.querySelectorAll("style")].find((s) => s.textContent.startsWith(".face { display: none"))?.remove());
  console.log(`      scrolling through all of it, debug build: median ${withPictures.median} ms, worst ${withPictures.worst} ms with pictures and gaps, median ${withoutPictures.median} ms, worst ${withoutPictures.worst} ms without`);
  check("pictures and gaps don't slow scrolling much", withPictures.median <= withoutPictures.median * 1.5 + 10, JSON.stringify({ withPictures, withoutPictures }));
  const picturesAsked = ((await relayLog()).pictureGets ?? 0) - picturesBefore;
  console.log(`      asked the relay for pictures ${picturesAsked} times, for 64 people`);
  check("a long chat doesn't keep asking for the same pictures", picturesAsked <= 64 * 2, String(picturesAsked));
  await page.locator(".convo .scroller").evaluate((el) => (el.scrollTop = 0));
  await sleep(250);
  await page.screenshot({ path: path.join(shotDir, "48-sixty-four-people-light.png") });
  const needleColour = () => page.locator(".convo .row", { hasText: "needle-from-long-ago" }).evaluate((r) => getComputedStyle(r).getPropertyValue("--who").trim());
  const needleBefore = await needleColour();
  const jumping = Date.now();
  await page.locator("#search").fill("needle-from-long-ago");
  const needleHit = page.locator(".results .hit").first();
  await until(() => needleHit.isVisible());
  await needleHit.click();
  await until(() => page.locator(".row.hl", { hasText: "needle-from-long-ago" }).isVisible());
  console.log(`      searching and jumping to an old message, debug build: ${Date.now() - jumping} ms`);
  const inView = () =>
    page.evaluate(() => {
      const row = document.querySelector(".row.hl");
      const box = document.querySelector(".convo .scroller").getBoundingClientRect();
      const r = row?.getBoundingClientRect();
      return r ? { top: Math.round(r.top), inside: r.top >= box.top && r.bottom <= box.bottom } : null;
    });
  const jumpedTo = await inView();
  check("a search result opens in view", !!jumpedTo?.inside, JSON.stringify(jumpedTo));
  await sleep(600);
  const jumpedLater = await inView();
  check("and stays put as the rows around it settle", !!jumpedLater?.inside && Math.abs(jumpedLater.top - jumpedTo.top) <= 2, JSON.stringify({ jumpedTo, jumpedLater }));
  check("a jump to old messages keeps that person's colour", (await needleColour()) === needleBefore, `${needleBefore} ${await needleColour()}`);
  await page.locator("#search").fill("");

  await page.emulateMedia({ colorScheme: "dark" });
  await sleep(200);
  await page.screenshot({ path: path.join(shotDir, "48-sixty-four-people-dark.png") });
  await page.emulateMedia({ colorScheme: "light" });
  await page.setViewportSize({ width: 420, height: 480 });
  await sleep(250);
  await page.screenshot({ path: path.join(shotDir, "49-people-narrow.png") });
  const sideways = await page.locator(".convo .scroller").evaluate((el) => el.scrollWidth - el.clientWidth);
  check("at the minimum window size, pictures don't push anything sideways", sideways <= 1, String(sideways));
  await page.setViewportSize({ width: 1180, height: 780 });
  }

  // The group's colour is on the left of a row. Nothing on the right may look like a new message.
  {
    const current = await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("groups_get"));
    const READ_ONE = "15557770001@s.whatsapp.net";
    const UNREAD_ONE = "15557770002@s.whatsapp.net";
    const dotGroup = current.groups[0];
    const soon = Date.now() + 600000;
    await control("add-chats", {
      chats: [
        { jid: READ_ONE, name: "Read In A Group", ts: soon, read: true },
        { jid: UNREAD_ONE, name: "Unread In A Group", ts: soon + 1000, read: false },
      ],
    });
    await control("groups", { groups: current.groups, assign: { ...current.assign, [READ_ONE]: dotGroup.id, [UNREAD_ONE]: dotGroup.id } });
    await page.getByRole("tab", { name: /^All/ }).click();
    await page.evaluate(() => (document.querySelector(".side .list").scrollTop = 0));
    const inList = (name) => page.locator(".side .row.abs").filter({ has: page.locator(".name", { hasText: new RegExp(`^${name}$`) }) }).first();
    await until(() => inList("Read In A Group").isVisible());
    await until(async () => (await inList("Read In A Group").evaluate((r) => getComputedStyle(r).boxShadow)).includes("4px 0px 0px"));
    const looks = (name) =>
      inList(name).evaluate((r) => {
        const line = r.querySelectorAll(".main .line")[1];
        return {
          shadow: getComputedStyle(r).boxShadow,
          colour: r.style.getPropertyValue("--group-color"),
          unread: r.classList.contains("unread"),
          dots: r.querySelectorAll(".dot").length,
          count: r.querySelector(".count")?.textContent.trim() ?? "",
          // Everything in the second line after the preview text is a mark on the right.
          rightMarks: [...line.children].filter((c) => !c.classList.contains("preview")).map((c) => c.className),
          topRight: [...r.querySelectorAll(".main .line")[0].children].filter((c) => !c.classList.contains("name")).map((c) => c.className),
        };
      });
    const readLook = await looks("Read In A Group");
    check("a read chat in a group has its colour on the left", readLook.colour === dotGroup.color && readLook.shadow.includes("4px 0px 0px"), JSON.stringify(readLook));
    check("a read chat in a group has no dot or other mark on the right", readLook.dots === 0 && readLook.rightMarks.length === 0 && !readLook.unread && !readLook.count, JSON.stringify(readLook));
    check("the only other thing on the right of a read chat is its time", readLook.topRight.join() === "time", readLook.topRight.join());
    const unreadLook = await looks("Unread In A Group");
    check("an unread chat in a group still shows as unread, with its count and no dot", unreadLook.unread && unreadLook.count === "1" && unreadLook.dots === 0 && unreadLook.rightMarks.join() === "count" && unreadLook.topRight.join().includes("fresh"), JSON.stringify(unreadLook));
    check("no chat in the list has a dot, on any row", (await page.locator(".side .row.abs .dot").count()) === 0);
    await page.screenshot({ path: path.join(shotDir, "50-no-dots.png") });
    await page.locator("#search").fill("In A Group");
    await until(() => page.locator(".results .row").first().isVisible());
    check("search results have no dot", (await page.locator(".results .dot").count()) === 0);
    await page.locator("#search").fill("");
    await page.getByRole("button", { name: "Settings", exact: true }).click();
    const twoColumns = page.getByLabel(/Show groups in their own column/);
    await twoColumns.check();
    await page.getByRole("button", { name: "Chats", exact: true }).click();
    await until(() => page.locator(".column.people").isVisible());
    await sleep(200);
    check("in two columns, no chat has a dot either", (await page.locator(".side .row.abs").count()) > 0 && (await page.locator(".side .row.abs .dot").count()) === 0);
    await page.getByRole("button", { name: "Settings", exact: true }).click();
    await twoColumns.uncheck();
    await page.getByRole("button", { name: "Chats", exact: true }).click();
    await until(() => page.locator(".column.all").isVisible());
    check("the tabs keep theirs, since those name a group", (await page.locator(".tabs.custom .dot").count()) === current.groups.length);
  }

  // Renaming and exiting. All made up, against the fake relay. No test reaches a real group.
  {
    const ACT = "120363000000000999@g.us";
    const soon = Date.now() + 700000;
    await control("group-facts", { access: { can_change: true, why: "members_can_edit" }, members: [{ jid: "11111@lid", phone: "15559990000@s.whatsapp.net", lid: "11111@lid" }, { jid: "15557770009@s.whatsapp.net" }], membersFail: false, actionError: null });
    await control("add-chats", {
      chats: [
        { jid: ACT, name: "Made-up Action Group", ts: soon, read: true, text: "needleinactiongroup" },
      ],
    });
    await page.getByRole("tab", { name: /^All/ }).click();
    await page.evaluate(() => (document.querySelector(".side .list").scrollTop = 0));
    const inList = (name) => page.locator(".side .row.abs").filter({ has: page.locator(".name", { hasText: new RegExp(`^${name}$`) }) }).first();
    await until(() => inList("Made-up Action Group").isVisible());
    await inList("Made-up Action Group").click();
    await until(() => page.locator(".convo .title .name", { hasText: "Made-up Action Group" }).isVisible());
    const more = page.getByRole("button", { name: /^Chat options/ });
    const actions = async () => (await relayLog()).groupActions ?? [];
    const dialogBox = page.getByRole("alertdialog");

    // Rename.
    await more.click();
    const renameItem = page.getByRole("menuitem", { name: "Rename group…" });
    check("a group you can edit offers a rename", !!(await until(() => renameItem.isVisible())));
    check("a member can exit, and nothing offers to delete a group", (await page.getByRole("menuitem", { name: /Delete/ }).count()) === 0 && (await page.getByRole("menuitem", { name: "Exit group…" }).isVisible()));
    await page.screenshot({ path: path.join(shotDir, "51-chat-options.png") });
    await renameItem.click();
    check("renaming says everyone will see it, before anything happens", (await dialogBox.innerText()).includes("Everyone in Made-up Action Group will see the new name") && (await actions()).length === 0);
    await page.screenshot({ path: path.join(shotDir, "52-rename-group.png") });
    await page.getByRole("button", { name: "Cancel" }).click();
    check("cancelling sends nothing", (await actions()).length === 0 && (await dialogBox.count()) === 0);
    await more.click();
    await renameItem.click();
    await page.getByLabel("New group name").fill("");
    check("an empty name can't be sent", await page.getByRole("button", { name: "Rename it for everyone" }).isDisabled());
    await control("group-facts", { actionError: { status: 409, error: "Only this group's admins can rename it, so nothing changed." } });
    await page.getByLabel("New group name").fill("Refused Made-up Name");
    await page.getByRole("button", { name: "Rename it for everyone" }).click();
    check("a refusal stays on screen, and doesn't look like a lapsed login", !!(await until(() => dialogBox.getByRole("alert").isVisible())) && (await dialogBox.getByRole("alert").innerText()).includes("admins") && (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("conn_state"))).need_login === false);
    await control("group-facts", { actionError: null });
    await page.getByLabel("New group name").fill("Renamed Made-up Group");
    await page.getByRole("button", { name: "Rename it for everyone" }).click();
    check("the new name shows in the header and the list", !!(await until(async () => (await page.locator(".convo .title .name").innerText()) === "Renamed Made-up Group" && (await inList("Renamed Made-up Group").isVisible()))));
    check("the dialog closes", (await dialogBox.count()) === 0);
    const renames = (await actions()).filter((a) => a.action === "rename");
    check("what reached the relay is that group and that name", renames[renames.length - 1].jid === ACT && renames[renames.length - 1].name === "Renamed Made-up Group", JSON.stringify(renames));

    // Who can't rename.
    await control("group-facts", { access: { can_change: false, why: "admins_only" } });
    await sleep(100);
    await more.click();
    check("where only admins can rename, it says so, and exit stays", !!(await until(() => page.getByText("Only this group's admins can rename it.").isVisible())) && (await renameItem.count()) === 0 && (await page.getByRole("menuitem", { name: "Exit group…" }).isVisible()));
    await page.locator(".convo .title").click();
    await control("group-facts", { access: { can_change: false, why: "unknown" } });
    await more.click();
    check("if WhatsApp can't say, nothing is offered", !!(await until(() => page.getByText("nothing's offered").isVisible())) && (await page.getByRole("menuitem").filter({ hasText: /Rename|Exit|Delete/ }).count()) === 0);
    await page.locator(".convo .title").click();
    await control("group-facts", { access: { can_change: true, why: "admin" } });

    // Exit.
    await more.click();
    await page.getByRole("menuitem", { name: "Exit group…" }).click();
    check("exiting warns first", (await dialogBox.innerText()).includes("you can only come back if someone adds you") && !(await actions()).some((a) => a.action === "leave"));
    await page.screenshot({ path: path.join(shotDir, "53-exit-group.png") });
    await page.getByRole("button", { name: "Exit group" }).click();
    check("exit goes through", !!(await until(async () => (await actions()).some((a) => a.action === "leave" && a.jid === ACT))));
    check("the chat stays after exiting", !!(await until(() => dialogBox.count().then((n) => n === 0))) && (await inList("Renamed Made-up Group").isVisible()) && (await page.locator(".convo .bubble").count()) === 1);

    // After exiting.
    await more.click();
    check("after exiting, it says so, and offers nothing that would fail", !!(await until(() => page.getByText("You're no longer in this group.").isVisible())) && (await page.getByRole("menuitem").filter({ hasText: /Rename|Exit|Delete/ }).count()) === 0);
    await page.screenshot({ path: path.join(shotDir, "54-after-exit.png") });
    await page.locator(".convo .title").click();

    // Renaming a person.
    await page.locator("#search").fill("Bob Plumber");
    await until(() => page.locator(".results .row", { hasText: "Bob Plumber" }).first().isVisible());
    await page.locator(".results .row", { hasText: "Bob Plumber" }).first().click();
    await page.locator("#search").fill("");
    await until(() => page.locator(".convo .title .name", { hasText: "Bob Plumber" }).isVisible());
    await more.click();
    await page.getByRole("menuitem", { name: "Rename contact…" }).click();
    check("renaming a person says nothing changes on WhatsApp", (await dialogBox.innerText()).includes("Nothing changes on WhatsApp"));
    await page.screenshot({ path: path.join(shotDir, "55-rename-contact.png") });
    await page.getByLabel("New name").fill("Robert Made-up");
    await dialogBox.getByRole("button", { name: "Save" }).click();
    check("their new name shows", !!(await until(async () => (await page.locator(".convo .title .name").innerText()) === "Robert Made-up")));
    const namesLog = (await relayLog()).names ?? [];
    check("it went to the relay, for that person", namesLog.some((n) => n.set === DM2 && n.name === "Robert Made-up"), JSON.stringify(namesLog));
    await more.click();
    await page.getByRole("menuitem", { name: "Rename contact…" }).click();
    await page.getByRole("button", { name: "Use their own name" }).click();
    check("it can go back to their own name", !!(await until(async () => (await page.locator(".convo .title .name").innerText()) === "Bob Plumber")));

    // Renaming someone from a group's member list.
    await control("group-facts", { access: { can_change: true, why: "admin" }, members: [{ jid: "11111@lid", phone: "15559990000@s.whatsapp.net", lid: "11111@lid" }, { jid: "15550000099@s.whatsapp.net" }] });
    await page.locator("#search").fill("Bingo Committee");
    await until(() => page.locator(".results .row", { hasText: "Bingo Committee" }).first().isVisible());
    await page.locator(".results .row", { hasText: "Bingo Committee" }).first().click();
    await page.locator("#search").fill("");
    const unnamed = page.locator(".convo header .member", { hasText: "+15550000099" });
    check("a group member can be right-clicked", !!(await until(() => unnamed.isVisible())));
    await unnamed.click({ button: "right" });
    await page.getByLabel("New name").fill("Member Made-up");
    await dialogBox.getByRole("button", { name: "Save" }).click();
    check("they then show by that name in the member list", !!(await until(() => page.locator(".convo header .member", { hasText: "Member Made-up" }).isVisible())));
    const you = page.locator(".convo header .member", { hasText: /^You/ });
    await you.click({ button: "right" });
    await sleep(100);
    check("you can't rename yourself from there", (await dialogBox.count()) === 0);
  }

  // A group with no name comes from the home PC named by its members, the way WhatsApp shows it, "&" and all.
  {
    const label = "Made-up Ann & Made-up Bo";
    await control("add-chats", { chats: [{ jid: "120363299999999901@g.us", name: label, ts: Date.now() - 3600000, read: true }] });
    await page.locator("#search").fill("Made-up Bo");
    const hit = page.locator(".results .row", { hasText: label });
    check("a group with no name is found by its members' names", !!(await until(() => hit.first().isVisible())));
    await hit.first().click();
    await page.locator("#search").fill("");
    check("its header shows them", !!(await until(async () => (await page.locator(".convo .title .name").innerText()) === label)));
    await page.evaluate(async (n) => {
      // In two columns, groups have their own list.
      const groupsList = document.querySelector(".side .column.groups .list");
      const all = await window.__TAURI_INTERNALS__.invoke("chat_list");
      const at = (groupsList ? all.filter((c) => c.jid.endsWith("@g.us")) : all).findIndex((c) => c.name === n);
      const list = groupsList ?? document.querySelector(".side .list");
      if (list) list.scrollTop = Math.max(0, at * 70 - 140);
    }, label);
    check("and so does its row in the list", !!(await until(() => page.locator(".side .row.abs .name", { hasText: label }).first().isVisible())));
  }

  // Settings shows which relay this copy uses, and won't take a bad address in its place.
  {
    await page.getByRole("button", { name: "Settings", exact: true }).click();
    const relayRow = page.locator(".relay-row");
    check("Settings shows the relay this copy uses", !!(await until(async () => (await relayRow.innerText()).includes(base))), await relayRow.innerText().catch(() => ""));
    await relayRow.getByRole("button", { name: "Change…" }).click();
    await page.getByLabel("New relay address").fill("ftp://relay.example.com");
    await relayRow.getByRole("button", { name: "Save and restart" }).click();
    check("and refuses a bad replacement, staying on the same relay", !!(await until(async () => /https:\/\//.test(await relayRow.getByRole("alert").innerText()))) && (await page.evaluate(() => window.__TAURI_INTERNALS__.invoke("relay_get"))).address === base);
    await relayRow.getByRole("button", { name: "Cancel" }).click();
    await page.getByRole("button", { name: "Chats", exact: true }).click();
  }

  check("no script errors during the run", pageErrors.length === 0, pageErrors.join(" | "));
} catch (e) {
  check("run completed", false, e.stack ?? String(e));
} finally {
  await browser?.close().catch(() => {});
  app.kill();
  server.closeAllConnections?.();
  server.close();
  await sleep(800);
  try {
    rmSync(data, { recursive: true, force: true });
  } catch {}
  const failed = results.filter((r) => !r.ok);
  console.log(`\n${results.length - failed.length} of ${results.length} passed`);
  process.exit(failed.length ? 1 : 0);
}
