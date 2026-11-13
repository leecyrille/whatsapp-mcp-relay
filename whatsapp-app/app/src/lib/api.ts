import { invoke } from "@tauri-apps/api/core";

export interface Chat {
  jid: string;
  name: string;
  is_group: boolean;
  last_ms: number;
  preview: string;
  preview_from_me: boolean;
  preview_status: string;
  unread: number;
}

export interface Labeled {
  label: string;
  value: string;
}

export interface CardAddress {
  label: string;
  street: string;
  city: string;
  region: string;
  postcode: string;
  country: string;
}

/** A contact card, as the home PC keeps it. */
export interface Card {
  id: string;
  version: number;
  created_ms: number;
  updated_ms: number;
  display: string;
  name: { prefix?: string; given?: string; middle?: string; family?: string; suffix?: string };
  nickname?: string;
  phones?: Labeled[];
  emails?: Labeled[];
  company?: string;
  department?: string;
  title?: string;
  addresses?: CardAddress[];
  urls?: Labeled[];
  dates?: Labeled[];
  related?: Labeled[];
  handles?: Labeled[];
  notes?: string;
  links?: string[];
}

export interface CardChange {
  seq: number;
  at_ms: number;
  who: string;
  action: string;
  id: string;
  before?: Card;
  after?: Card;
}

export interface LinkSuggestion {
  jid: string;
  name: string;
  why: "same number" | "same name only";
  linked_to?: string;
}

export interface ImportItem {
  index: number;
  display: string;
  kind: "new" | "merge" | "ambiguous" | "invalid";
  match_id?: string;
  match_name?: string;
  matches?: string[];
  adds?: string[];
  conflicts?: { field: string; existing: string; incoming: string }[];
  links?: string[];
  error?: string;
}

export interface MemberList {
  members: { jid: string; person: string; lid: string; name: string; you: boolean; admin: boolean }[];
  fetched_ms: number | null;
}

export interface Message {
  rowid: number;
  raw_chat: string;
  id: string;
  chat: string;
  sender_name: string;
  sender_jid: string;
  person: string;
  from_me: boolean;
  ts: number;
  text: string;
  media: string;
  filename: string;
  mime: string;
  size: number;
  has_thumb: boolean;
  width: number;
  height: number;
  seconds: number;
  quoted_id: string;
  quoted_name: string;
  quoted_text: string;
  mentions?: Record<string, string>;
  status: string;
  edited: boolean;
  revoked: boolean;
  reactions: Record<string, string> | null;
}

export interface SearchHit {
  rowid: number;
  chat: string;
  chat_name: string;
  ts: number;
  from_me: boolean;
  sender_name: string;
  snippet: string;
  media: string;
}

export interface Contact {
  jid: string;
  name: string;
  number: string;
  has_chat: boolean;
}

export interface Group {
  id: string;
  name: string;
  color: string;
  order: number;
}

export interface GroupDoc {
  groups: Group[];
  assign: Record<string, string>;
}

export interface Conn {
  need_login: boolean;
  online: boolean;
  wa_connected: boolean;
  wa_linked: boolean;
  claude_up: boolean;
  link: LinkState;
  last_updated_ms: number;
}

export type LinkState = "ok" | "connecting" | "catching_up" | "login" | "no_internet" | "home_down" | "relay_down" | "bridge_down" | "unlinked" | "wa_down";

export interface Picked {
  token: string;
  name: string;
  size: number;
  kind: string;
}

export interface ThreadItem {
  id: string;
  kind: "user" | "assistant" | "tool" | "approval" | "error" | "result";
  text?: string;
  tool?: string;
  input?: any;
  state?: string;
  by?: string;
  ts: number;
  cost?: number;
}

export interface Thread {
  id: string;
  title: string;
  project: string;
  session_id?: string;
  created: number;
  updated: number;
  running: boolean;
  items?: ThreadItem[];
}

export interface Proposal {
  jid: string;
  group: string;
  confidence: number;
  reason: string;
}

export interface SortJob {
  id: string;
  state: "running" | "done" | "failed";
  error?: string;
  proposals?: Proposal[];
  items: number;
  cost?: number;
}

export interface PictureDoc {
  pictures?: Record<string, number>;
  own?: string[];
}

export interface UpdateState {
  state: "idle" | "checking" | "up_to_date" | "downloading" | "ready" | "need_login" | "offline" | "refused" | "failed";
  version: string;
  current: string;
}

export const api = {
  conn: () => invoke<Conn>("conn_state"),
  openLogin: () => invoke<void>("open_login"),
  signOut: (erase: boolean) => invoke<void>("sign_out", { erase }),
  chatList: () => invoke<Chat[]>("chat_list"),
  page: (chat: string, beforeTs: number | null = null, beforeRowid: number | null = null, limit = 60) =>
    invoke<Message[]>("messages_page", { chat, beforeTs, beforeRowid, limit }),
  after: (chat: string, afterTs: number, afterRowid: number, limit = 300) =>
    invoke<Message[]>("messages_after", { chat, afterTs, afterRowid, limit }),
  around: (rowid: number) => invoke<Message[]>("messages_around", { rowid }),
  search: (query: string, limit = 100) => invoke<SearchHit[]>("search", { query, limit }),
  markRead: (chat: string) => invoke<void>("mark_read", { chat }),
  setActive: (chat: string | null) => invoke<void>("set_active_chat", { chat }),
  sendText: (chat: string, text: string, quote: unknown, clientId: string, mentions: string[] = []) =>
    invoke<Message>("send_text", { chat, text, quote, clientId, mentions }),
  cardsGet: () => invoke<{ cards: Card[]; rev: number; offline?: boolean }>("cards_get"),
  cardSave: (card: Partial<Card>, expect: number) => invoke<{ ok: boolean; card: Card }>("card_save", { card, expect }),
  cardDelete: (id: string, expect: number) => invoke<void>("card_delete", { id, expect }),
  cardHistory: (id: string | null) => invoke<{ entries: CardChange[] }>("card_history", { id }),
  cardUndo: (seq: number) => invoke<{ ok: boolean; card: Card | null }>("card_undo", { seq }),
  cardSuggest: (card: Partial<Card>) => invoke<{ suggestions: LinkSuggestion[] }>("card_suggest", { card }),
  cardImport: (cards: unknown[], apply: boolean, choices: unknown[], linkExact: boolean) =>
    invoke<{ ok: boolean; items: ImportItem[]; added?: number; updated?: number }>("card_import", { cards, apply, choices, linkExact }),
  pickVcard: () => invoke<string | null>("pick_vcard"),
  editMessage: (rawChat: string, id: string, text: string, mentions: string[]) =>
    invoke<void>("edit_message", { rawChat, id, text, mentions }),
  revokeMessage: (rawChat: string, id: string) => invoke<void>("revoke_message", { rawChat, id }),
  hideMessage: (rawChat: string, id: string) => invoke<void>("hide_message", { rawChat, id }),
  forwardMessage: (rawChat: string, id: string, to: string[], clientId: string) =>
    invoke<{ ok: boolean; limited?: string[] }>("forward_message", { rawChat, id, to, clientId }),
  react: (rawChat: string, id: string, senderJid: string, emoji: string) =>
    invoke<void>("react", { rawChat, id, senderJid, emoji }),
  loadOlder: (rawChat: string) => invoke<void>("load_older", { rawChat }),
  pickFiles: () => invoke<Picked[]>("pick_files"),
  savePasted: (bytes: number[], ext: string) => invoke<Picked>("save_pasted", { bytes, ext }),
  forgetToken: (token: string) => invoke<void>("forget_token", { token }),
  sendFile: (args: {
    chat: string;
    token: string;
    kind: string;
    caption: string;
    thumb: string | null;
    width: number;
    height: number;
    seconds: number;
    quote: unknown;
    clientId: string;
    mentions: string[];
  }) => invoke<void>("send_file", args),
  mediaFetch: (rowid: number) => invoke<void>("media_fetch", { rowid }),
  mediaSave: (rowid: number) => invoke<boolean>("media_save", { rowid }),
  mediaOpen: (rowid: number) => invoke<void>("media_open", { rowid }),
  openLink: (url: string) => invoke<void>("open_link", { url }),
  contacts: () => invoke<Contact[]>("contacts"),
  groups: () => invoke<GroupDoc>("groups_get"),
  groupCreate: (name: string, color: string | null) => invoke<GroupDoc>("group_create", { name, color }),
  groupUpdate: (id: string, name: string | null, color: string | null, order: number | null) =>
    invoke<GroupDoc>("group_update", { id, name, color, order }),
  groupDelete: (id: string) => invoke<GroupDoc>("group_delete", { id }),
  assign: (jids: string[], group: string) => invoke<GroupDoc>("assign", { jids, group }),
  claude: <T = any>(method: string, path: string, body?: unknown) =>
    invoke<T>("claude_api", { method, path, body: body ?? null }),
  sortStart: (includeSnippets: boolean, onlyUnsorted: boolean, includeContacts: boolean) =>
    invoke<SortJob>("sort_start", { includeSnippets, onlyUnsorted, includeContacts }),
  settingsGet: () => invoke<any>("settings_get"),
  relayGet: () => invoke<{ address: string; configured: boolean }>("relay_get"),
  /** Saves the address and restarts the app on it, so this only comes back with a refusal. */
  relaySet: (address: string) => invoke<void>("relay_set", { address }),
  settingsSet: (key: string, value: unknown) => invoke<void>("settings_set", { key, value }),
  stats: () => invoke<any>("cache_stats"),
  pictures: () => invoke<PictureDoc>("pictures_get"),
  pickPicture: () => invoke<Picked | null>("pick_picture"),
  pictureSet: (jid: string, token: string) => invoke<void>("picture_set", { jid, token }),
  pictureRemove: (jid: string) => invoke<void>("picture_remove", { jid }),
  updateState: () => invoke<UpdateState>("update_state"),
  updateCheck: () => invoke<void>("update_check"),
  updateInstall: () => invoke<void>("update_install"),
  updateSetBusy: (busy: boolean) => invoke<void>("update_set_busy", { busy }),
  silenceState: () => invoke<{ until_ms: number | null }>("silence_state"),
  silenceSet: (minutes: number) => invoke<void>("silence_set", { minutes }),
  silenceClear: () => invoke<void>("silence_clear"),
  blinkSet: (on: boolean) => invoke<void>("blink_set", { on }),
  groupPhotoAccess: (jid: string) => invoke<{ can_change: boolean; why: string }>("picture_group_access", { jid }),
  groupPhotoSet: (jid: string, token: string) => invoke<{ custom_showing?: boolean }>("picture_group_set", { jid, token }),
  whatsappGroupCreate: (name: string, members: string[]) =>
    invoke<{ jid: string; added: string[]; not_added: { jid: string; code: number; invited?: boolean }[] }>("whatsapp_group_create", { name, members }),
  groupMembers: (jid: string) => invoke<MemberList>("group_members", { jid }),
  groupMembersRefresh: (jid: string) => invoke<MemberList>("group_members_refresh", { jid }),
  chatSenders: (jid: string) => invoke<string[]>("chat_senders", { jid }),
  contactNameSet: (jid: string, name: string) => invoke<{ name: string }>("contact_name_set", { jid, name }),
  contactNameRemove: (jid: string) => invoke<{ changed: boolean }>("contact_name_remove", { jid }),
  whatsappGroupRename: (jid: string, name: string) => invoke<{ name: string; changed: boolean }>("whatsapp_group_rename", { jid, name }),
  whatsappGroupLeave: (jid: string) => invoke<unknown>("whatsapp_group_leave", { jid }),
  takeOpenTarget: () => invoke<{ kind: "app" | "claude" } | { kind: "chat"; jid: string } | null>("take_open_target"),
};

export const avatarUrl = (jid: string, version: number) => `http://wamedia.localhost/avatar/${jid}?v=${version}`;
export const thumbUrl = (rowid: number) => `http://wamedia.localhost/thumb/${rowid}`;
export const fileUrl = (rowid: number) => `http://wamedia.localhost/file/${rowid}`;
export const localUrl = (token: string) => `http://wamedia.localhost/local/${token}`;

export function newId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

export function errorText(e: unknown): string {
  return typeof e === "string" ? e : e instanceof Error ? e.message : "Something went wrong.";
}
