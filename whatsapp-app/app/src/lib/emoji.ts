// The full emoji set for the pickers: categories in Unicode's order, search by name and everyday words,
// recently used first, and skin tones. The list is bundled (emoji-data.json, made by scripts/make-emoji-data.mjs)
// and only loaded the first time a picker opens.

export interface Emoji {
  char: string;
  name: string;
  words: string;
  tones: boolean;
  version: string;
  group: string;
}

export interface EmojiGroup {
  name: string;
  emoji: Emoji[];
}

let loaded: Promise<EmojiGroup[]> | null = null;

export function loadEmoji(): Promise<EmojiGroup[]> {
  loaded ??= import("./emoji-data.json").then((m) => {
    const data = (m.default ?? m) as { groups: { name: string; emoji: [string, string, string, number, string][] }[] };
    return data.groups.map((g) => ({
      name: g.name,
      emoji: g.emoji.map(([char, name, words, tones, version]) => ({ char, name, words, tones: tones === 1, version, group: g.name })),
    }));
  });
  return loaded;
}

/** Skin tones as WhatsApp offers them: none (yellow), then light to dark. */
export const TONES = ["", "\u{1F3FB}", "\u{1F3FC}", "\u{1F3FD}", "\u{1F3FE}", "\u{1F3FF}"];
export const TONE_NAMES = ["Default", "Light", "Medium-light", "Medium", "Medium-dark", "Dark"];

/** Puts the tone on every person in the emoji, as WhatsApp does when one tone is chosen. */
export function withTone(e: Emoji, tone: number): string {
  const mod = TONES[tone] ?? "";
  if (!mod || !e.tones) return e.char;
  // In "people holding hands" and the like, the handshake between them is part of the sequence and takes no
  // tone of its own. On its own, it does.
  const parts = e.char.split(ZWJ);
  return parts.map((part) => (parts.length > 1 && part.startsWith(HANDSHAKE) ? part : part.replace(PERSON, `$1${mod}`))).join(ZWJ);
}

const ZWJ = String.fromCodePoint(0x200d);
const HANDSHAKE = String.fromCodePoint(0x1f91d);
// A person (or hand) that takes a tone, with the emoji-style marker after it, which a tone replaces.
const PERSON = new RegExp(`(\\p{Emoji_Modifier_Base})${String.fromCodePoint(0xfe0f)}?`, "u");

function read<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key);
    return raw === null ? fallback : (JSON.parse(raw) as T);
  } catch {
    return fallback;
  }
}

function write(key: string, value: unknown) {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {}
}

const RECENT_KEY = "wa-emoji-recent";
const TONE_KEY = "wa-emoji-tone";
const RECENT_MAX = 32;

/** What was used last, newest first, as sent (with its tone). Kept on this PC. */
export function recentEmoji(): string[] {
  const list = read<unknown>(RECENT_KEY, []);
  return Array.isArray(list) ? list.filter((x): x is string => typeof x === "string").slice(0, RECENT_MAX) : [];
}

export function noteUsed(char: string) {
  write(RECENT_KEY, [char, ...recentEmoji().filter((c) => c !== char)].slice(0, RECENT_MAX));
}

export function savedTone(): number {
  const t = read<number>(TONE_KEY, 0);
  return Number.isInteger(t) && t >= 0 && t < TONES.length ? t : 0;
}

export function saveTone(tone: number) {
  write(TONE_KEY, tone);
}

/** Matches every word typed against the name and the everyday words, by their starts. */
export function searchEmoji(groups: EmojiGroup[], query: string): Emoji[] {
  const parts = query.toLowerCase().trim().split(/\s+/).filter(Boolean);
  if (!parts.length) return [];
  const out: Emoji[] = [];
  for (const g of groups) {
    for (const e of g.emoji) {
      const hay = `${e.name} ${e.words.replace(/,/g, " ")}`.toLowerCase().split(/[\s:'’.-]+/);
      if (parts.every((p) => hay.some((w) => w.startsWith(p)))) out.push(e);
    }
  }
  return out;
}

// Whether Windows' emoji font can draw one. Each is drawn once, off screen, and the answer kept. One the font
// lacks comes out as one of three things: the same empty box an unassigned character gets, several separate
// pictures side by side (a combined emoji the font doesn't know), or, for country flags, plain letters.
const drawable = new Map<string, boolean>();
let canvas: CanvasRenderingContext2D | null = null;
let oneWide = 0;
let box: Uint8ClampedArray | null = null;
let flagsInColour: boolean | null = null;
const PX = 24;
const W = PX * 2;
const H = PX + 6;
export const EMOJI_FONT = `"Segoe UI Emoji", "Segoe UI Symbol", "Apple Color Emoji", "Noto Color Emoji", sans-serif`;
// A private-use character no font here has a picture for, so it draws the font's empty box.
const NO_SUCH = "\u{10FFFD}";

function pixels(ctx: CanvasRenderingContext2D, char: string): Uint8ClampedArray {
  ctx.clearRect(0, 0, W, H);
  ctx.fillStyle = "#000";
  ctx.fillText(char, 0, 2);
  return ctx.getImageData(0, 0, W, H).data;
}

function coloured(px: Uint8ClampedArray): boolean {
  for (let i = 0; i < px.length; i += 4) {
    if (px[i + 3] < 40) continue;
    if (Math.max(px[i], px[i + 1], px[i + 2]) - Math.min(px[i], px[i + 1], px[i + 2]) > 24) return true;
  }
  return false;
}

function same(a: Uint8ClampedArray, b: Uint8ClampedArray): boolean {
  if (a.length !== b.length) return false;
  for (let i = 3; i < a.length; i += 4) if (a[i] !== b[i]) return false;
  return true;
}

function context(): CanvasRenderingContext2D | null {
  if (canvas) return canvas;
  const c = document.createElement("canvas");
  c.width = W;
  c.height = H;
  canvas = c.getContext("2d", { willReadFrequently: true });
  if (canvas) {
    canvas.font = `${PX}px ${EMOJI_FONT}`;
    canvas.textBaseline = "top";
    oneWide = canvas.measureText("\u{1F600}").width || PX;
    box = pixels(canvas, NO_SUCH);
  }
  return canvas;
}

export function canDraw(char: string): boolean {
  const known = drawable.get(char);
  if (known !== undefined) return known;
  const ctx = context();
  let ok = true;
  if (ctx) {
    if (flagLetters(char)) {
      flagsInColour ??= coloured(pixels(ctx, "\u{1F1FA}\u{1F1F8}"));
      ok = flagsInColour;
    } else if (ctx.measureText(char).width > oneWide * 1.4) {
      ok = false;
    } else if (box && same(pixels(ctx, char), box)) {
      ok = false;
    }
  }
  drawable.set(char, ok);
  return ok;
}

// Any emoji in a piece of text, to find the ones Windows can't draw. Chromium knows Unicode's own list.
let everyEmoji: RegExp | null = null;
try {
  everyEmoji = new RegExp("\\p{RGI_Emoji}", "gv");
} catch {}
const MIGHT_HAVE_EMOJI = /[\p{Extended_Pictographic}\p{Regional_Indicator}]/u;

export interface TextPiece {
  text: string;
  /** An emoji Windows can't draw: shown in a frame, or a flag as its letters. */
  missing?: boolean;
  letters?: string | null;
}

/** Splits text so any emoji Windows can't draw can be shown in a frame instead of as broken pieces. */
export function emojiPieces(text: string): TextPiece[] {
  if (!everyEmoji || !MIGHT_HAVE_EMOJI.test(text)) return [{ text }];
  const out: TextPiece[] = [];
  let last = 0;
  for (const m of text.matchAll(everyEmoji)) {
    const at = m.index ?? 0;
    if (canDraw(m[0])) continue;
    if (at > last) out.push({ text: text.slice(last, at) });
    out.push({ text: m[0], missing: true, letters: flagLetters(m[0]) });
    last = at + m[0].length;
  }
  if (!out.length) return [{ text }];
  if (last < text.length) out.push({ text: text.slice(last) });
  return out;
}

/** Two letters for a country flag Windows draws as plain letters: 🇫🇷 is FR. */
export function flagLetters(char: string): string | null {
  const points = [...char].map((c) => c.codePointAt(0) ?? 0);
  if (points.length === 2 && points.every((p) => p >= 0x1f1e6 && p <= 0x1f1ff)) {
    return points.map((p) => String.fromCharCode(p - 0x1f1e6 + 65)).join("");
  }
  return null;
}
