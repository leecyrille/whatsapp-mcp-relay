import { listen } from "@tauri-apps/api/event";
import { api, type Chat, type Conn, type GroupDoc, type PictureDoc, type UpdateState } from "./api";

function savedBlink(): boolean {
  try {
    return localStorage.getItem("wa-blink") !== "0";
  } catch {
    return true;
  }
}

function savedTwoColumns(): boolean {
  try {
    return localStorage.getItem("wa-two-columns") === "1";
  } catch {
    return false;
  }
}

export const ui = $state({
  conn: { need_login: false, online: false, wa_connected: false, wa_linked: false, claude_up: false, link: "connecting", last_updated_ms: 0 } as Conn,
  chats: [] as Chat[],
  groups: { groups: [], assign: {} } as GroupDoc,
  view: "chats" as "chats" | "contacts" | "claude" | "settings",
  active: null as string | null,
  jumpTo: null as number | null,
  filter: "all" as string,
  // Groups in their own column, with each column's own tab selection. Kept on this PC.
  twoColumns: savedTwoColumns(),
  // Unread chats pulse. Kept on this PC.
  blink: savedBlink(),
  peopleFilter: "all" as string,
  groupsFilter: "all" as string,
  // Goes up when a notification asks for its chat's row to be shown.
  reveal: 0,
  syncing: false,
  syncDone: 0,
  syncHead: 0,
  settings: { read_receipts: true, notifications: true, auto_images: true, media_cache_gb: 5, autostart: false, auto_update: true } as any,
  update: { state: "idle", version: "", current: "" } as UpdateState,
  pictures: { versions: {} as Record<string, number>, own: [] as string[] },
  approvalsWaiting: 0,
  silenceUntil: null as number | null,
  toast: "" as string,
  loaded: false,
  /** The contact card being shown: by card, or for a person, who gets a new one if they have none. */
  card: null as null | { id?: string; jid?: string; name?: string },
});

/** Per-file download state, keyed by message rowid. */
export const media = $state<Record<number, { state: string; done?: number; total?: number; error?: string }>>({});

/** Listeners other components can join. */
type Handler = (payload: any) => void;
const handlers: Record<string, Set<Handler>> = {};

export function on(event: string, fn: Handler): () => void {
  (handlers[event] ??= new Set()).add(fn);
  return () => handlers[event].delete(fn);
}

function fire(event: string, payload: any) {
  handlers[event]?.forEach((fn) => fn(payload));
}

let toastTimer: ReturnType<typeof setTimeout> | undefined;
export function toast(message: string) {
  ui.toast = message;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (ui.toast = ""), 6000);
}

const busyReasons = new Set<string>();
/** Marks something a restart would lose, so an automatic update waits for it. */
export function setBusy(reason: string, busy: boolean) {
  const before = busyReasons.size > 0;
  if (busy) busyReasons.add(reason);
  else busyReasons.delete(reason);
  const after = busyReasons.size > 0;
  if (before !== after) void api.updateSetBusy(after);
}

let reloadQueued = false;
let lastReload = 0;
/** Coalesces bursts of changes into one chat-list read per frame. Reads run side by side and can finish
 *  in any order, so only the newest one counts, or an older list could stay on screen. */
export function reloadChats() {
  if (reloadQueued) return;
  reloadQueued = true;
  requestAnimationFrame(async () => {
    reloadQueued = false;
    const mine = ++lastReload;
    const chats = await api.chatList();
    if (mine === lastReload) ui.chats = chats;
  });
}

export function openChat(jid: string, rowid: number | null = null) {
  ui.active = jid;
  ui.jumpTo = rowid;
  ui.view = "chats";
  void api.setActive(jid);
}

const lessMotion = window.matchMedia("(prefers-reduced-motion: reduce)");

/** The tray and taskbar follow the same two things the unread rows do. */
function tellBlink() {
  void api.blinkSet(ui.blink && !lessMotion.matches);
}

export function setBlink(on: boolean) {
  ui.blink = on;
  try {
    localStorage.setItem("wa-blink", on ? "1" : "0");
  } catch {}
  tellBlink();
}

export function setTwoColumns(on: boolean) {
  ui.twoColumns = on;
  try {
    localStorage.setItem("wa-two-columns", on ? "1" : "0");
  } catch {}
}

export function closeChat() {
  ui.active = null;
  ui.jumpTo = null;
  void api.setActive(null);
}

export function groupOf(jid: string): string {
  return ui.groups.assign[jid] ?? "";
}

/** The colour of the chat's custom group, if it's in one. */
export function groupColor(jid: string): string | undefined {
  const id = groupOf(jid);
  return id ? ui.groups.groups.find((g) => g.id === id)?.color : undefined;
}

/** A clicked notification. The app checked the chat is a known one before this sees it. */
async function openFromNotification() {
  const target = await api.takeOpenTarget();
  if (target?.kind === "chat") {
    openChat(target.jid);
    ui.reveal++;
  }
  else if (target?.kind === "claude") ui.view = "claude";
}

function setPictures(doc: PictureDoc) {
  ui.pictures = { versions: doc.pictures ?? {}, own: doc.own ?? [] };
}

/** What a forward did, by chat name, including any chat a limit or a failure left out. */
export function forwardSummary(r: { sent?: string[]; failed?: { chat: string; error: string }[]; limited?: string[] }): string {
  const name = (jid: string) => ui.chats.find((c) => c.jid === jid)?.name ?? `+${jid.split("@")[0]}`;
  const list = (jids: string[]) => jids.map(name).join(", ");
  const parts: string[] = [];
  if (r.sent?.length) parts.push(`Forwarded to ${list(r.sent)}.`);
  if (r.limited?.length) parts.push(`Not sent to ${list(r.limited)}: the forwarding limit (30 chats an hour, 100 a day) was reached.`);
  for (const f of r.failed ?? []) parts.push(`Not sent to ${name(f.chat)}: ${f.error}`);
  return parts.join(" ") || "Nothing was forwarded.";
}

export async function init() {
  tellBlink();
  lessMotion.addEventListener("change", tellBlink);
  await Promise.all([
    listen<Conn>("conn", (e) => (ui.conn = e.payload)),
    listen<{ chats: string[] }>("chats-changed", (e) => {
      reloadChats();
      fire("chats-changed", e.payload.chats);
    }),
    listen<GroupDoc>("groups", (e) => (ui.groups = e.payload)),
    listen<any>("sync-progress", (e) => {
      ui.syncing = e.payload.syncing;
      ui.syncDone = e.payload.cursor ?? 0;
      ui.syncHead = e.payload.head ?? ui.syncHead;
    }),
    listen<any>("media-state", (e) => {
      media[e.payload.rowid] = e.payload;
    }),
    listen<any>("send-progress", (e) => fire("send-progress", e.payload)),
    listen<any>("send-result", (e) => fire("send-result", e.payload)),
    listen<any>("forward-result", (e) => toast(forwardSummary(e.payload))),
    listen("cards-changed", () => fire("cards-changed", null)),
    listen<any>("claude-event", (e) => fire("claude-event", e.payload)),
    listen<any[]>("files-dropped", (e) => fire("files-dropped", e.payload)),
    listen("contacts-changed", () => fire("contacts-changed", null)),
    listen<UpdateState>("update-state", (e) => (ui.update = e.payload)),
    listen<PictureDoc>("pictures", (e) => setPictures(e.payload)),
    listen("open-target", () => void openFromNotification()),
    listen<number | null>("silence", (e) => (ui.silenceUntil = e.payload)),
    listen<string>("group-members-changed", (e) => fire("group-members-changed", e.payload)),
  ]);
  ui.conn = await api.conn();
  ui.groups = await api.groups();
  ui.chats = await api.chatList();
  ui.settings = await api.settingsGet();
  ui.update = await api.updateState();
  setPictures(await api.pictures());
  ui.silenceUntil = (await api.silenceState()).until_ms;
  ui.loaded = true;
  await openFromNotification();
}
