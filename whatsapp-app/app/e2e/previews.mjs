// Document previews in the real app against the fake relay, with made-up and hostile files.
//   node e2e/previews.mjs <playwright-core dir> <screenshot dir>
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeRelay } from "./fake-relay.mjs";
import { docx, pdf, pptx, xlsx } from "./make-docs.mjs";

const [pwDir, shotDir] = process.argv.slice(2);
const require = createRequire(path.join(pwDir, "x.js"));
const { chromium } = require("playwright-core");
const exe = path.resolve("src-tauri/target/debug/wa-relay-app.exe");
const data = mkdtempSync(path.join(os.tmpdir(), "wa-preview-"));
const CDP = 9343;
const PORT = 18793;
const base = `http://127.0.0.1:${PORT}`;
mkdirSync(shotDir, { recursive: true });

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
    await sleep(50);
  }
  return null;
}
const control = (p, body) =>
  fetch(`${base}/control/${p}`, { method: "POST", body: body ? JSON.stringify(body) : undefined }).then((r) => r.json());

const { server, GROUP } = makeRelay();
await new Promise((r) => server.listen(PORT, "127.0.0.1", r));

const PNG = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAoAAAAKCAYAAACNMs+9AAAAFUlEQVR42mNkYPj/n4EIwMRIJBhVSAwAVhQBEhQmF1wAAAAASUVORK5CYII=", "base64");
const big = pdf(40, { imageSide: 500 });
const docs = [
  { id: "d-pdf", filename: "Made-up report.pdf", mime: "application/pdf", bytes: pdf(3) },
  { id: "d-bigpdf", filename: "Made-up big report.pdf", mime: "application/pdf", bytes: big },
  { id: "d-hostile-pdf", filename: "Made-up hostile.pdf", mime: "application/pdf", bytes: pdf(1, { hostile: true }) },
  { id: "d-docx", filename: "Made-up letter.docx", mime: "", bytes: await docx(20, { hostile: true }) },
  { id: "d-bigdocx", filename: "Made-up long letter.docx", mime: "", bytes: await docx(12000) },
  { id: "d-xlsx", filename: "Made-up hours.xlsx", mime: "", bytes: xlsx(200) },
  { id: "d-bigxlsx", filename: "Made-up big hours.xlsx", mime: "", bytes: xlsx(50000) },
  { id: "d-pptx", filename: "Made-up deck.pptx", mime: "", bytes: await pptx(PNG) },
  { id: "d-html", filename: "Made-up page.html", mime: "text/html", bytes: Buffer.from("<script>parent.postMessage({type:'link',url:'https://example.invalid/'},'*');document.title='ran'</script><h1>Made-up</h1>") },
  { id: "d-svg", filename: "Made-up drawing.svg", mime: "image/svg+xml", bytes: Buffer.from('<svg xmlns="http://www.w3.org/2000/svg" width="40" height="40"><script>parent.postMessage({type:"link",url:"https://example.invalid/svg"},"*")</script><rect width="40" height="40" fill="teal"/></svg>') },
  { id: "d-json", filename: "made-up.json", mime: "application/json", bytes: Buffer.from('{"made":"up","n":[1,2]}') },
  { id: "d-broken", filename: "Made-up broken.pdf", mime: "application/pdf", bytes: Buffer.from("%PDF-1.4 this is not really a pdf") },
  { id: "d-doc", filename: "Made-up old.doc", mime: "application/msword", bytes: Buffer.from("old word file") },
  { id: "d-huge", filename: "Made-up huge.pdf", mime: "application/pdf", bytes: pdf(1), size: 200 * 1024 * 1024 },
];
console.log(`      sizes: big PDF ${(big.length / 1048576).toFixed(1)} MB, long DOCX ${(docs[4].bytes.length / 1048576).toFixed(1)} MB, big XLSX ${(docs[6].bytes.length / 1048576).toFixed(1)} MB`);

const app = spawn(exe, [], {
  env: { ...process.env, WA_RELAY_URL: base, WA_DATA_DIR: data, WA_TEST_OFFSCREEN: "1", WA_SECOND_INSTANCE: "1", WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: `--remote-debugging-port=${CDP}` },
  stdio: "ignore",
});

let browser;
const pageErrors = [];
try {
  browser = await until(() => chromium.connectOverCDP(`http://127.0.0.1:${CDP}`), 30000);
  const findPage = (pred) =>
    until(async () => {
      for (const ctx of browser.contexts()) for (const p of ctx.pages()) if (await pred(p)) return p;
    }, 15000);
  const page = await findPage((p) => p.url().includes("tauri.localhost") || p.url().includes("index.html"));
  page.on("pageerror", (e) => pageErrors.push(process.env.PREVIEW_DEBUG ? (e.stack ?? e.message).slice(0, 400) : e.message));
  if (process.env.PREVIEW_DEBUG) {
    page.on("console", (m) => m.type() === "error" && m.text().includes("plugins") && console.log("      console:", m.type(), m.text().slice(0, 120), JSON.stringify(m.location())));
    page.on("pageerror", () => console.log("      pageerror; frames now:", page.frames().map((f) => f.url()).join(" ")));
    page.on("crash", () => console.log("      PAGE CRASHED"));
    page.on("frameattached", (f) => console.log("      frame attached", f.url()));
    page.on("framenavigated", (f) => console.log("      frame navigated", f.url()));
  }
  await page.setViewportSize({ width: 1180, height: 780 }).catch(() => {});
  const signIn = page.getByRole("button", { name: "Sign in", exact: true });
  await until(() => signIn.isVisible());
  await signIn.click();
  const login = await findPage(async (p) => (await p.title().catch(() => "")) === "PIN page");
  await control("finish-login");
  await login.evaluate(() => (location.href = "/login-ok?done=1")).catch(() => {});
  await until(() => page.locator(".side .row", { hasText: "Bingo Committee" }).first().isVisible(), 20000);

  check("nothing of the viewer loads at startup", !(await page.evaluate(() => performance.getEntriesByType("resource").some((r) => r.name.includes("/viewer") || /assets\/(pdf|docx|sheet|pptx)-/.test(r.name)))));
  let ts = Date.now() - 60000;
  for (const d of docs) await control("doc", { id: d.id, chat: GROUP, filename: d.filename, mime: d.mime, base64: d.bytes.toString("base64"), size: d.size, ts: (ts += 10) });
  await page.locator(".side .row", { hasText: "Bingo Committee" }).first().click();
  await until(() => page.locator(".doc", { hasText: "Made-up report.pdf" }).isVisible(), 15000);

  const viewer = () => page.frames().find((f) => f.url().includes("wamedia.localhost/viewer"));
  const dialog = page.getByRole("dialog", { name: /^Preview of/ });
  const open = async (name) => {
    const started = Date.now();
    const row = page.locator(".doc", { hasText: name });
    await row.scrollIntoViewIfNeeded();
    await row.getByRole("button", { name: "Preview file" }).click();
    await until(() => dialog.isVisible());
    return started;
  };
  const close = async () => {
    await page.keyboard.press("Escape");
    return !!(await until(async () => !(await dialog.count())));
  };
  const inFrame = (fn, arg) => until(async () => viewer() && (await viewer().evaluate(fn, arg)), 30000);
  const times = {};

  // PDF.
  let t0 = await open("Made-up report.pdf");
  check("a PDF shows its pages", !!(await inFrame(() => document.querySelectorAll(".page").length === 3 && document.querySelectorAll("canvas").length >= 1)));
  check("the preview offers Open and Save", (await dialog.getByRole("button", { name: "Open in default app" }).isVisible()) && (await dialog.getByRole("button", { name: "Save…" }).isVisible()));
  await page.screenshot({ path: path.join(shotDir, "50-preview-pdf.png") });

  // The sandbox: nothing in the viewer reaches the app.
  const probe = await viewer().evaluate(async () => {
    const out = {};
    out.tauri = typeof window.__TAURI_INTERNALS__;
    out.bridge = await Promise.race([
      Promise.resolve()
        .then(() => window.__TAURI_INTERNALS__.invoke("chat_list"))
        .then((x) => "ANSWERED " + JSON.stringify(x).length, (e) => "refused " + String(e).slice(0, 60)),
      new Promise((r) => setTimeout(() => r("no answer"), 4000)),
    ]);
    // Sent straight to the host's message channel too.
    out.invoke = await new Promise((resolve) => {
      const send = window.chrome?.webview?.postMessage;
      if (!send) return resolve("no channel");
      window.chrome.webview.addEventListener?.("message", (e) => resolve("answered: " + JSON.stringify(e.data).slice(0, 80)));
      send.call(window.chrome.webview, JSON.stringify({ cmd: "chat_list", callback: 1, error: 2, payload: {} }));
      setTimeout(() => resolve("no answer"), 3000);
    });
    for (const [k, u] of [["ipc", "http://ipc.localhost/chat_list"], ["file", "http://wamedia.localhost/file/1"], ["web", "https://example.com/"]]) {
      try {
        await fetch(u, { method: k === "ipc" ? "POST" : "GET" });
        out[k] = "reached";
      } catch (e) {
        out[k] = "blocked";
      }
    }
    try {
      out.parentDoc = String(parent.document.title);
    } catch {
      out.parentDoc = "blocked";
    }
    try {
      out.storage = String(localStorage.length);
    } catch {
      out.storage = "blocked";
    }
    out.origin = self.origin;
    out.popup = String(window.open("https://example.com/") === null ? "blocked" : "opened");
    try {
      top.location.href = "https://example.com/";
      out.topNav = "tried";
    } catch {
      out.topNav = "blocked";
    }
    return out;
  });
  check("the viewer has an origin of its own", probe.origin === "null", JSON.stringify(probe));
  check("a command from the viewer gets no answer, through the bridge or the host's channel", !String(probe.bridge).startsWith("ANSWERED") && ["no answer", "no channel"].includes(probe.invoke), JSON.stringify(probe));
  check("it can't reach the command channel, other files or the web", probe.ipc === "blocked" && probe.file === "blocked" && probe.web === "blocked", JSON.stringify(probe));
  check("it can't read the app, storage or open windows", probe.parentDoc === "blocked" && probe.storage === "blocked" && probe.popup === "blocked", JSON.stringify(probe));
  await sleep(300);
  check("and it can't move the app away", page.url().includes("tauri.localhost") || page.url().includes("index.html"), page.url());
  await close();

  t0 = await open("Made-up big report.pdf");
  await inFrame(() => document.querySelector("canvas") !== null);
  times.pdf = Date.now() - t0;
  check("a 10 MB PDF shows its first page quickly", times.pdf < 8000 && (await viewer().evaluate(() => document.querySelectorAll(".page").length)) === 40, `${times.pdf} ms`);
  await close();

  await open("Made-up hostile.pdf");
  check("a PDF with an open script just shows, and its script doesn't run", !!(await inFrame(() => document.querySelector("canvas") !== null)) && !(await page.getByRole("alertdialog", { name: "Open this link?" }).count()));
  await close();

  // Word.
  await open("Made-up letter.docx");
  const word = await inFrame(() => document.body.innerText.includes("Made-up heading") && document.body.innerText);
  check("a Word file shows its text", !!word && word.includes("Made-up paragraph 20"));
  check("text that looks like code stays text", word?.includes("<script>parent.hacked=1</script>") && (await viewer().evaluate(() => document.querySelectorAll("script:not([src]), img[onerror]").length)) === 0);
  check("a picture from the web isn't fetched", (await viewer().evaluate(() => [...document.images].every((i) => !i.src.startsWith("http")))));
  check("it says the layout isn't exact", word?.includes("Layout is close to Word's"));
  await viewer().locator("a", { hasText: "A made-up link" }).click();
  const ask = page.getByRole("alertdialog", { name: "Open this link?" });
  check("a link asks first, showing where it goes", !!(await until(() => ask.isVisible())) && (await ask.innerText()).includes("https://example.com/made-up-link"));
  await page.screenshot({ path: path.join(shotDir, "51-preview-docx-link.png") });
  await ask.getByRole("button", { name: "Cancel" }).click();
  await close();
  t0 = await open("Made-up long letter.docx");
  await inFrame(() => document.body.innerText.includes("Made-up paragraph 12000"));
  times.docx = Date.now() - t0;
  check("a long Word file shows", times.docx < 15000, `${times.docx} ms`);
  await close();

  // Spreadsheets.
  await open("Made-up hours.xlsx");
  check("a spreadsheet shows a table with sheet tabs", !!(await inFrame(() => document.querySelectorAll(".tabs button").length === 2 && document.body.innerText.includes("J-1001"))));
  await viewer().getByRole("tab", { name: "Second sheet" }).click();
  // The keyboard is now in the preview, and Escape must still close it: checked at the end of this file's next close.
  check("tabs switch sheets, and a formula shows its saved value", !!(await inFrame(() => [...document.querySelectorAll("td")].some((td) => td.textContent === "3"))));
  await page.screenshot({ path: path.join(shotDir, "52-preview-xlsx.png") });
  check("Escape closes it with the keyboard in the preview", await close());
  t0 = await open("Made-up big hours.xlsx");
  await inFrame(() => document.body.innerText.includes("J-1001"));
  times.xlsx = Date.now() - t0;
  const capped = await viewer().evaluate(() => ({ note: document.querySelector(".tabs + .note")?.textContent ?? "", rows: document.querySelectorAll("tbody tr").length }));
  check("a 50,000-row sheet shows quickly, capped, and says so", times.xlsx < 15000 && capped.note.includes("first 2,000 rows") && capped.rows < 200, `${times.xlsx} ms ${JSON.stringify(capped)}`);
  await close();

  // PowerPoint, text, SVG and HTML.
  await open("Made-up deck.pptx");
  check("a deck shows each slide's text and pictures, and says it's not the layout", !!(await inFrame(() => document.body.innerText.includes("Made-up deck title") && document.body.innerText.includes("Second slide") && document.querySelectorAll(".slide img").length === 1 && document.body.innerText.includes("not its layout"))));
  await close();
  await open("made-up.json");
  check("JSON shows as text", !!(await inFrame(() => document.querySelector("pre")?.textContent.includes('"made": "up"'))));
  await close();
  await open("Made-up page.html");
  check("a web page shows as its source, and nothing in it runs", !!(await inFrame(() => document.querySelector("pre")?.textContent.includes("<script>") && document.title === "Preview")) && !(await page.getByRole("alertdialog", { name: "Open this link?" }).count()));
  await close();
  await open("Made-up drawing.svg");
  check("an SVG shows only as a picture, and its script doesn't run", !!(await inFrame(() => document.querySelector("img.svg")?.complete)) && (await sleep(400), !(await page.getByRole("alertdialog", { name: "Open this link?" }).count())));
  await close();

  // What can't be previewed.
  await open("Made-up broken.pdf");
  check("a broken file says so and offers Open", !!(await until(async () => (await dialog.innerText()).includes("Couldn't preview this file"), 20000)));
  await close();
  await open("Made-up huge.pdf");
  check("an oversized file isn't loaded, and says why", !!(await until(async () => (await dialog.innerText()).includes("more than this preview takes"))));
  await close();
  const old = page.locator(".doc", { hasText: "Made-up old.doc" });
  await old.scrollIntoViewIfNeeded();
  check("an old Word file keeps Open and Save, with no preview", (await old.getByRole("button", { name: "Preview file" }).count()) === 0 && (await old.getByRole("button", { name: "Open file" }).count()) === 1);
  // Tauri puts its set-up scripts into every frame. In the sandboxed viewer one of them can't find what it sets up and
  // stops, once per preview. That's expected and harmless; anything else isn't.
  const unexpected = pageErrors.filter((m) => !m.includes("reading 'plugins'"));
  check("no other page errors", unexpected.length === 0, unexpected.slice(0, 3).join(" | "));
  console.log(`      open times: 10 MB PDF first page ${times.pdf} ms, long DOCX ${times.docx} ms, 50,000-row XLSX ${times.xlsx} ms`);
} catch (e) {
  check("the run finished", false, e.stack ?? String(e));
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
