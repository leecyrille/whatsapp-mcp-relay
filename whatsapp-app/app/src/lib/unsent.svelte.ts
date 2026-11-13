// Everything the user wrote that hasn't reached the home PC yet: drafts, messages still sending, and ones that
// didn't send. Kept in one place, outside any view, so a result that arrives while another tab is showing still
// lands, Settings can list them all, and drafts and unsent text survive a restart.
import { api, errorText, newId } from "./api";
import { toWire, type Pick } from "./mentions";
import { on, setBusy, ui } from "./state.svelte";

export interface Outgoing {
  clientId: string;
  chat: string;
  text: string;
  state: "sending" | "failed";
  kind: "text" | "file";
  error?: string;
  name?: string;
  size?: number;
  done?: number;
  token?: string;
  fileKind?: string;
  thumb?: string | null;
  width?: number;
  height?: number;
  seconds?: number;
  quote: unknown;
  /** Addresses of the people mentioned, sent with the text. */
  mentions?: string[];
  /** The text as the user typed it, with names, for showing while it sends. */
  shown?: string;
  /** When it was last sent or tried. */
  at: number;
  /** A file the home PC has in full: it sends it to WhatsApp by itself, and a restart here can't lose it. */
  handedOver?: boolean;
}

export interface Draft {
  text: string;
  picks: Pick[];
  at: number;
}

export const unsent = $state({ outbox: [] as Outgoing[], drafts: {} as Record<string, Draft> });

/** Past this, something unsent stops holding back an automatic update. It's still listed in Settings, and
 *  drafts and unsent text are kept through the restart anyway. */
export const STALE_MS = 24 * 3600 * 1000;
/** A handed-over file whose result never came (the connection dropped just then) stops showing after this. */
const HANDED_OVER_MS = 10 * 60 * 1000;
const KEY = "wa-unsent";

/** Text that didn't send can be tried again after a restart. A file can't: its pick lives in memory. */
const keptThroughRestart = (i: Outgoing) => i.kind === "text" && i.state === "failed";

function load() {
  try {
    const saved = JSON.parse(localStorage.getItem(KEY) ?? "null");
    if (!saved) return;
    for (const [chat, d] of Object.entries<Draft>(saved.drafts ?? {})) {
      if (typeof d?.text === "string" && d.text.trim()) unsent.drafts[chat] = { text: d.text, picks: Array.isArray(d.picks) ? d.picks : [], at: Number(d.at) || Date.now() };
    }
    for (const i of saved.outbox ?? []) {
      if (i?.kind === "text" && typeof i.chat === "string" && typeof i.text === "string" && typeof i.clientId === "string") {
        unsent.outbox.push({ ...i, state: "failed", handedOver: false, at: Number(i.at) || Date.now() });
      }
    }
  } catch {}
}

function save() {
  try {
    const outbox = unsent.outbox.filter(keptThroughRestart);
    if (!outbox.length && !Object.keys(unsent.drafts).length) localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, JSON.stringify({ drafts: unsent.drafts, outbox }));
  } catch {}
}

/** Signing out with the saved chats removed takes these too. */
export function forgetAllUnsent() {
  unsent.outbox = [];
  unsent.drafts = {};
  try {
    localStorage.removeItem(KEY);
  } catch {}
}

export function setDraft(chat: string, text: string, picks: Pick[]) {
  if (text.trim()) unsent.drafts[chat] = { text, picks, at: Date.now() };
  else delete unsent.drafts[chat];
}

const sentListeners = new Set<(chat: string) => void>();
/** Called when something reaches the home PC, so the open chat can show it. */
export function onSent(fn: (chat: string) => void): () => void {
  sentListeners.add(fn);
  return () => sentListeners.delete(fn);
}

const find = (clientId: string) => unsent.outbox.find((p) => p.clientId === clientId);
const drop = (clientId: string) => (unsent.outbox = unsent.outbox.filter((p) => p.clientId !== clientId));

export function queueText(chat: string, text: string, mentions: string[], shown: string, quote: unknown) {
  const item: Outgoing = { clientId: newId(), chat, text, mentions, shown, state: "sending", kind: "text", quote, at: Date.now() };
  unsent.outbox.push(item);
  void deliverText(item);
}

export function queueFile(item: Omit<Outgoing, "clientId" | "state" | "kind" | "at" | "done">) {
  const full: Outgoing = { ...item, clientId: newId(), state: "sending", kind: "file", done: 0, at: Date.now() };
  unsent.outbox.push(full);
  void deliverFile(full);
}

async function deliverText(item: Outgoing) {
  const current = find(item.clientId);
  if (current) {
    current.state = "sending";
    current.error = undefined;
    current.at = Date.now();
  }
  try {
    await api.sendText(item.chat, item.text, item.quote, item.clientId, item.mentions ?? []);
    drop(item.clientId);
    sentListeners.forEach((fn) => fn(item.chat));
  } catch (e) {
    const failed = find(item.clientId);
    if (failed) {
      failed.state = "failed";
      failed.error = errorText(e);
    }
  }
}

async function deliverFile(item: Outgoing) {
  const current = find(item.clientId);
  if (current) {
    current.state = "sending";
    current.error = undefined;
    current.handedOver = false;
    current.at = Date.now();
  }
  try {
    await api.sendFile({
      chat: item.chat,
      token: item.token!,
      kind: item.fileKind!,
      caption: item.text,
      thumb: item.thumb ?? null,
      width: item.width ?? 0,
      height: item.height ?? 0,
      seconds: item.seconds ?? 0,
      quote: item.quote,
      clientId: item.clientId,
      mentions: item.mentions ?? [],
    });
    // Uploaded. The home PC now sends it to WhatsApp, and a send-result event reports the outcome.
    const sent = find(item.clientId);
    if (sent && sent.state === "sending") {
      sent.handedOver = true;
      sent.at = Date.now();
    }
  } catch (e) {
    const failed = find(item.clientId);
    if (failed) {
      failed.state = "failed";
      failed.handedOver = false;
      failed.error = errorText(e);
    }
  }
}

export function retry(item: Outgoing) {
  if (item.kind === "text") void deliverText(item);
  else void deliverFile(item);
}

export function discard(item: Outgoing) {
  drop(item.clientId);
  if (item.token) void api.forgetToken(item.token);
}

/** A draft sent from the list in Settings, as the chat's own Send button would. */
export function sendDraft(chat: string) {
  const d = unsent.drafts[chat];
  if (!d) return;
  const shown = d.text.trim();
  const wire = toWire(shown, chat.endsWith("@g.us") ? d.picks : []);
  delete unsent.drafts[chat];
  queueText(chat, wire.text, wire.mentions, shown, null);
}

export function discardDraft(chat: string) {
  delete unsent.drafts[chat];
}

export interface Held {
  key: string;
  chat: string;
  kind: "draft" | "failed" | "sending" | "handed";
  text: string;
  at: number;
  item?: Outgoing;
  /** The chat isn't in the list any more, so it can't be opened or sent to from there. */
  orphan: boolean;
  /** Whether it holds back an automatic update. */
  blocks: boolean;
  /** Whether it's still here after a restart. */
  kept: boolean;
}

const clock = $state({ now: Date.now() });
setInterval(() => (clock.now = Date.now()), 30_000);

/** Everything unsent, newest first. Read inside a component or effect to follow changes. */
export function held(): Held[] {
  const now = clock.now;
  // Until the chat list has loaded, nothing counts as missing from it.
  const listed = ui.loaded && ui.chats.length > 0 ? new Set(ui.chats.map((c) => c.jid)) : null;
  const orphan = (chat: string) => !!listed && !listed.has(chat);
  const out: Held[] = [];
  for (const [chat, d] of Object.entries(unsent.drafts)) {
    const lost = orphan(chat);
    out.push({ key: `draft:${chat}`, chat, kind: "draft", text: d.text.trim(), at: d.at, orphan: lost, blocks: !lost && now - d.at < STALE_MS, kept: true });
  }
  for (const item of unsent.outbox) {
    const kind = item.state === "failed" ? "failed" : item.handedOver ? "handed" : "sending";
    if (kind === "handed" && now - item.at > HANDED_OVER_MS) continue;
    const lost = orphan(item.chat);
    const text = item.shown || item.text || (item.kind === "file" ? (item.name ?? "A file") : "");
    out.push({
      key: item.clientId,
      chat: item.chat,
      kind,
      text,
      at: item.at,
      item,
      orphan: lost,
      blocks: kind !== "handed" && !lost && now - item.at < STALE_MS,
      kept: keptThroughRestart(item),
    });
  }
  return out.sort((a, b) => b.at - a.at);
}

load();

// The home PC's answer for a file, whichever view is showing when it comes.
on("send-progress", (p) => {
  const item = find(p.client_id);
  if (item) item.done = p.done;
});
on("send-result", (r) => {
  const item = find(r.client_id);
  if (!item) return;
  if (r.ok) {
    drop(r.client_id);
    sentListeners.forEach((fn) => fn(item.chat));
  } else {
    item.state = "failed";
    item.handedOver = false;
    item.error = r.error ?? "Couldn't send that file.";
  }
});

$effect.root(() => {
  $effect(() => setBusy("unsent", held().some((h) => h.blocks)));
  $effect(() => {
    JSON.stringify(unsent);
    save();
  });
  // A handed-over file whose result never came stops showing in its chat too.
  $effect(() => {
    const now = clock.now;
    const gone = unsent.outbox.filter((i) => i.handedOver && i.state === "sending" && now - i.at > HANDED_OVER_MS).map((i) => i.clientId);
    if (gone.length) unsent.outbox = unsent.outbox.filter((i) => !gone.includes(i.clientId));
  });
});
