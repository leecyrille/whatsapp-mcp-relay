// @mentions while typing. The box shows "@Name"; what's sent is "@digits" plus the person's address, worked out
// from the final text at send time, so the text and the list of people notified can never disagree.

export interface Candidate {
  /** The address the group uses for them, which is what a mention must carry. */
  jid: string;
  /** The address their picture and name are filed under. */
  person: string;
  name: string;
}

export interface Pick {
  /** What shows after the @ in the box. */
  label: string;
  jid: string;
}

const WORDISH = /[\p{L}\p{N}]/u;
const MAX_QUERY = 40;

function charBefore(text: string, at: number): string {
  return Array.from(text.slice(Math.max(0, at - 2), at)).pop() ?? "";
}

function fold(s: string): string {
  return s.normalize("NFKD").replace(/\p{M}/gu, "").toLocaleLowerCase();
}

/** The "@query" being typed just before the caret, or null. "@" inside a word, as in an email address, isn't one. */
export function activeQuery(text: string, caret: number): { start: number; query: string } | null {
  const before = text.slice(0, caret);
  const at = before.lastIndexOf("@");
  if (at < 0) return null;
  if (at > 0 && WORDISH.test(charBefore(before, at))) return null;
  const query = before.slice(at + 1);
  if (query.length > MAX_QUERY || /[\n@]/.test(query) || /^\s/.test(query)) return null;
  return { start: at, query };
}

/** People whose name contains the query anywhere, best first: name starts with it, then a word does, then anywhere. */
export function matching(people: Candidate[], query: string): Candidate[] {
  const q = fold(query.trim());
  const digits = query.replace(/\D/g, "");
  const scored: { c: Candidate; score: number }[] = [];
  for (const c of people) {
    const name = fold(c.name);
    let score = -1;
    if (!q) score = 0;
    else if (name.startsWith(q)) score = 0;
    else if (name.split(/[\s\-_.(),]+/).some((w) => w.startsWith(q))) score = 1;
    else if (name.includes(q)) score = 2;
    else if (digits.length >= 3 && digits === query.trim() && (c.person.split("@")[0].includes(digits) || c.jid.split("@")[0].includes(digits))) score = 3;
    if (score >= 0) scored.push({ c, score });
  }
  scored.sort((a, b) => a.score - b.score || a.c.name.localeCompare(b.c.name));
  return scored.map((s) => s.c);
}

/** The label for a newly picked person. Two different people with the same name get "(2)", "(3)" so each mention stays theirs. */
export function labelFor(name: string, jid: string, picked: Pick[]): string {
  const base = name.replace(/[@\n\r]/g, "").trim() || "Unknown";
  for (let n = 1; ; n++) {
    const label = n === 1 ? base : `${base} (${n})`;
    const owner = picked.find((p) => p.label === label);
    if (!owner || owner.jid === jid) return label;
  }
}

function escape(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** Every whole "@label" in the text, in order. Longer labels win, so "@Bob Plumber" isn't read as "@Bob". */
export function findMentions(text: string, picked: Pick[]): { start: number; end: number; pick: Pick }[] {
  const labels = [...new Set(picked.map((p) => p.label))].sort((a, b) => b.length - a.length);
  if (!labels.length) return [];
  const re = new RegExp(`(?<![\\p{L}\\p{N}])@(${labels.map(escape).join("|")})(?![\\p{L}\\p{N}])`, "gu");
  const out: { start: number; end: number; pick: Pick }[] = [];
  for (const m of text.matchAll(re)) {
    const pick = picked.find((p) => p.label === m[1]);
    if (pick) out.push({ start: m.index ?? 0, end: (m.index ?? 0) + m[0].length, pick });
  }
  return out;
}

/** Drops picks whose mention has been edited away. */
export function stillThere(text: string, picked: Pick[]): Pick[] {
  const found = new Set(findMentions(text, picked).map((m) => m.pick.label));
  return picked.filter((p) => found.has(p.label));
}

/** What's sent: each whole "@label" becomes "@digits", and the addresses listed are exactly the ones left in the text. */
export function toWire(text: string, picked: Pick[]): { text: string; mentions: string[] } {
  const found = findMentions(text, picked);
  if (!found.length) return { text, mentions: [] };
  let out = "";
  let last = 0;
  const mentions: string[] = [];
  for (const m of found) {
    out += text.slice(last, m.start) + "@" + m.pick.jid.split("@")[0];
    last = m.end;
    if (!mentions.includes(m.pick.jid)) mentions.push(m.pick.jid);
  }
  return { text: out + text.slice(last), mentions };
}

/** The whole mention the caret is in or just after, for Backspace (after) or Delete (before), so a mention goes in one press. */
export function mentionAt(text: string, picked: Pick[], caret: number, key: "Backspace" | "Delete"): { start: number; end: number } | null {
  for (const m of findMentions(text, picked)) {
    if (key === "Backspace" && caret > m.start && caret <= m.end) return { start: m.start, end: m.end };
    if (key === "Delete" && caret >= m.start && caret < m.end) return { start: m.start, end: m.end };
  }
  return null;
}
