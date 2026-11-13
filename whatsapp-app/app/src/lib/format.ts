const time = new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" });
const weekday = new Intl.DateTimeFormat(undefined, { weekday: "short" });
const shortDate = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" });
const longDate = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", year: "numeric" });
const fullDay = new Intl.DateTimeFormat(undefined, { weekday: "long", month: "long", day: "numeric", year: "numeric" });

function startOfDay(ms: number): number {
  const d = new Date(ms);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
}

export function clock(ms: number): string {
  return time.format(ms);
}

/** Time for the chat list: today's clock, then weekday, then date. */
export function listTime(ms: number): string {
  if (!ms) return "";
  const days = Math.round((startOfDay(Date.now()) - startOfDay(ms)) / 86400000);
  if (days <= 0) return time.format(ms);
  if (days === 1) return "Yesterday";
  if (days < 7) return weekday.format(ms);
  return new Date(ms).getFullYear() === new Date().getFullYear() ? shortDate.format(ms) : longDate.format(ms);
}

/** When the app was last known to be current. */
export function lastUpdated(ms: number): string {
  const days = Math.round((startOfDay(Date.now()) - startOfDay(ms)) / 86400000);
  if (days <= 0) return `today at ${time.format(ms)}`;
  if (days === 1) return `yesterday at ${time.format(ms)}`;
  return `${listTime(ms)} at ${time.format(ms)}`;
}

/** Divider between days in a conversation. */
export function dayLabel(ms: number): string {
  const days = Math.round((startOfDay(Date.now()) - startOfDay(ms)) / 86400000);
  if (days === 0) return "Today";
  if (days === 1) return "Yesterday";
  return fullDay.format(ms);
}

export function sameDay(a: number, b: number): boolean {
  return startOfDay(a) === startOfDay(b);
}

export function fileSize(bytes: number): string {
  if (!bytes) return "";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1048576).toFixed(1)} MB`;
  return `${(bytes / 1073741824).toFixed(2)} GB`;
}

export function duration(seconds: number): string {
  if (!seconds) return "";
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  return `${m}:${String(s).padStart(2, "0")}`;
}

export function initials(name: string): string {
  const cleaned = name.replace(/[^\p{L}\p{N} ]/gu, "").trim();
  if (!cleaned) return "#";
  const parts = cleaned.split(/\s+/);
  const first = [...parts[0]][0] ?? "";
  const last = parts.length > 1 ? ([...parts[parts.length - 1]][0] ?? "") : "";
  return (first + last).toUpperCase();
}

const AVATAR_COLORS = ["#2f7f6f", "#3b6fb5", "#8a5cb8", "#b5603b", "#a8456f", "#3f8c4c", "#6b6fb8", "#a07f2c"];

export function avatarColor(key: string): string {
  let hash = 0;
  for (let i = 0; i < key.length; i++) hash = (hash * 31 + key.charCodeAt(i)) | 0;
  return AVATAR_COLORS[Math.abs(hash) % AVATAR_COLORS.length];
}

export type Segment = { text: string; url?: string; bold?: boolean; italic?: boolean; code?: boolean; strike?: boolean; mention?: boolean };

const MENTION_RE = /(?<![\p{L}\p{N}])@(\d{5,20})(?![\p{L}\p{N}])/gu;

/** Swaps an @ID for a name the app found for it. Plain text pieces only, and it never becomes markup. */
function withMentions(piece: Segment, names: Record<string, string>): Segment[] {
  if (piece.url || piece.code) return [piece];
  const out: Segment[] = [];
  let last = 0;
  for (const m of piece.text.matchAll(MENTION_RE)) {
    const name = names[m[1]];
    if (!name) continue;
    const at = m.index ?? 0;
    if (at > last) out.push({ ...piece, text: piece.text.slice(last, at) });
    out.push({ text: `@${name}`, mention: true });
    last = at + m[0].length;
  }
  if (last === 0) return [piece];
  if (last < piece.text.length) out.push({ ...piece, text: piece.text.slice(last) });
  return out;
}

const URL_RE = /\bhttps?:\/\/[^\s<>"')\]]+[^\s<>"')\].,;:!?]/gi;

function formatted(text: string): Segment[] {
  // WhatsApp's own markers. Plain string slicing, so nothing here can become markup.
  const out: Segment[] = [];
  const re = /(^|[\s(])(\*[^*\n]+\*|_[^_\n]+_|~[^~\n]+~|`[^`\n]+`)(?=$|[\s).,;:!?])/g;
  let last = 0;
  let match: RegExpExecArray | null;
  while ((match = re.exec(text))) {
    const start = match.index + match[1].length;
    if (start > last) out.push({ text: text.slice(last, start) });
    const body = match[2].slice(1, -1);
    const mark = match[2][0];
    out.push({ text: body, bold: mark === "*", italic: mark === "_", strike: mark === "~", code: mark === "`" });
    last = start + match[2].length;
  }
  if (last < text.length) out.push({ text: text.slice(last) });
  return out;
}

/** Splits message text into plain, formatted and link pieces. Rendered as text nodes, never as HTML. */
export function segments(text: string, mentions: Record<string, string> = {}): Segment[] {
  const out: Segment[] = [];
  let last = 0;
  for (const m of text.matchAll(URL_RE)) {
    const at = m.index ?? 0;
    if (at > last) out.push(...formatted(text.slice(last, at)));
    out.push({ text: m[0], url: m[0] });
    last = at + m[0].length;
  }
  if (last < text.length) out.push(...formatted(text.slice(last)));
  return out.flatMap((piece) => withMentions(piece, mentions));
}

const HIDDEN_CHARS = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f\u00ad\u200b-\u200f\u2028-\u202e\u2060-\u206f\ufeff]/gu;

/**
 * Prepares text for an approval card so it can't hide what it does. Invisible and direction-changing
 * characters are named, and long blank runs are collapsed, so a command can't be pushed out of sight.
 */
export function revealed(text: string): { text: string; lines: number; bytes: number } {
  const bytes = new TextEncoder().encode(text).length;
  const lines = text.split("\n").length;
  const shown = text
    .replace(HIDDEN_CHARS, (c) => `⟦U+${c.codePointAt(0)!.toString(16).toUpperCase().padStart(4, "0")}⟧`)
    .replace(/(?:[ \t]*\r?\n){3,}/g, (run) => `\n⟦${run.split("\n").length - 1} blank lines⟧\n`)
    .replace(/[ \t]{9,}/g, (run) => ` ⟦${run.length} spaces⟧ `);
  return { text: shown, lines, bytes };
}

/** Splits a search snippet at the highlight markers the database puts around each match. */
export function snippetParts(snippet: string): { text: string; hit: boolean }[] {
  const parts: { text: string; hit: boolean }[] = [];
  for (const piece of snippet.split("\u0001")) {
    const end = piece.indexOf("\u0002");
    if (end < 0) {
      if (piece) parts.push({ text: piece, hit: false });
    } else {
      parts.push({ text: piece.slice(0, end), hit: true });
      if (piece.length > end + 1) parts.push({ text: piece.slice(end + 1), hit: false });
    }
  }
  return parts;
}
