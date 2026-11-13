// Reads .vcf files from phones and address books (vCard 2.1, 3.0 and 4.0) into contact cards. Pictures are left out:
// a card keeps the picture WA Relay already has. Anything that can't be read as its field goes into the notes
// rather than being lost. The home PC checks every field again before anything is saved.

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

export interface ParsedCard {
  display: string;
  name: { prefix: string; given: string; middle: string; family: string; suffix: string };
  nickname: string;
  phones: Labeled[];
  emails: Labeled[];
  company: string;
  department: string;
  title: string;
  addresses: CardAddress[];
  urls: Labeled[];
  dates: Labeled[];
  related: Labeled[];
  handles: Labeled[];
  notes: string;
}

export const MAX_FILE_BYTES = 5 * 1024 * 1024;
export const MAX_CARDS = 5000;

const TYPE_LABELS: Record<string, string> = {
  cell: "mobile", mobile: "mobile", iphone: "iPhone", work: "work", home: "home", main: "main", fax: "fax",
  pager: "pager", other: "other", car: "car", spouse: "spouse", child: "child", parent: "parent",
};
const IGNORED_TYPES = new Set(["pref", "voice", "internet", "x400", "msg", "text", "dom", "intl", "postal", "parcel"]);

function blank(): ParsedCard {
  return {
    display: "", name: { prefix: "", given: "", middle: "", family: "", suffix: "" }, nickname: "", phones: [], emails: [],
    company: "", department: "", title: "", addresses: [], urls: [], dates: [], related: [], handles: [], notes: "",
  };
}

/** Lines with folded continuations joined, and quoted-printable soft breaks joined too. */
function unfold(text: string): string[] {
  const raw = text.replace(/\r\n?/g, "\n").split("\n");
  const out: string[] = [];
  for (const line of raw) {
    if ((line.startsWith(" ") || line.startsWith("\t")) && out.length) {
      out[out.length - 1] += line.slice(1);
    } else if (out.length && /QUOTED-PRINTABLE/i.test(out[out.length - 1].split(":")[0]) && out[out.length - 1].endsWith("=")) {
      out[out.length - 1] = out[out.length - 1].slice(0, -1) + line;
    } else {
      out.push(line);
    }
  }
  return out;
}

function decodeQuotedPrintable(value: string, charset: string): string {
  const bytes: number[] = [];
  for (let i = 0; i < value.length; i++) {
    if (value[i] === "=" && /^[0-9A-Fa-f]{2}$/.test(value.slice(i + 1, i + 3))) {
      bytes.push(parseInt(value.slice(i + 1, i + 3), 16));
      i += 2;
    } else {
      bytes.push(value.charCodeAt(i) & 0xff);
    }
  }
  try {
    return new TextDecoder(charset || "utf-8").decode(new Uint8Array(bytes));
  } catch {
    return new TextDecoder("utf-8").decode(new Uint8Array(bytes));
  }
}

/** Splits on separators that aren't escaped with a backslash, then unescapes each part. */
function split(value: string, sep: string): string[] {
  const parts: string[] = [];
  let cur = "";
  for (let i = 0; i < value.length; i++) {
    const ch = value[i];
    if (ch === "\\" && i + 1 < value.length) {
      const next = value[i + 1];
      cur += next === "n" || next === "N" ? "\n" : next;
      i++;
    } else if (ch === sep) {
      parts.push(cur);
      cur = "";
    } else {
      cur += ch;
    }
  }
  parts.push(cur);
  return parts.map((p) => p.trim());
}

function unescape(value: string): string {
  return split(value, "\u0000")[0];
}

interface Line {
  group: string;
  name: string;
  params: Record<string, string[]>;
  value: string;
}

function parseLine(line: string): Line | null {
  let inQuote = false;
  let colon = -1;
  for (let i = 0; i < line.length; i++) {
    if (line[i] === '"') inQuote = !inQuote;
    else if (line[i] === ":" && !inQuote) {
      colon = i;
      break;
    }
  }
  if (colon < 0) return null;
  const [head, ...rawParams] = line.slice(0, colon).split(";");
  const dot = head.indexOf(".");
  const params: Record<string, string[]> = {};
  for (const p of rawParams) {
    const eq = p.indexOf("=");
    const key = (eq < 0 ? "TYPE" : p.slice(0, eq)).toUpperCase();
    const vals = (eq < 0 ? p : p.slice(eq + 1)).replace(/"/g, "").split(",");
    (params[key] ??= []).push(...vals.map((v) => v.trim()).filter(Boolean));
  }
  let value = line.slice(colon + 1);
  if ((params.ENCODING ?? []).some((e) => e.toUpperCase() === "QUOTED-PRINTABLE")) {
    value = decodeQuotedPrintable(value, params.CHARSET?.[0] ?? "utf-8");
  }
  return { group: dot < 0 ? "" : head.slice(0, dot).toLowerCase(), name: (dot < 0 ? head : head.slice(dot + 1)).toUpperCase(), params, value };
}

/** Apple's labels look like _$!<Mobile>!$_. */
function cleanLabel(raw: string): string {
  return raw.replace(/^_\$!<(.*)>!\$_$/, "$1").trim();
}

function labelOf(line: Line, groupLabels: Record<string, string>): string {
  if (line.group && groupLabels[line.group]) return groupLabels[line.group];
  for (const t of line.params.TYPE ?? []) {
    const lower = t.toLowerCase();
    if (IGNORED_TYPES.has(lower)) continue;
    return TYPE_LABELS[lower] ?? (lower.startsWith("x-") ? t.slice(2) : t.toLowerCase());
  }
  return "";
}

/** A date as YYYY-MM-DD, or --MM-DD with no year, or null if it can't be read. */
export function normalDate(raw: string): string | null {
  const v = raw.trim().split("T")[0];
  // Apple writes a birthday with no year as 1604.
  let m = v.match(/^1604-?(\d{2})-?(\d{2})$/);
  if (m) return `--${m[1]}-${m[2]}`;
  m = v.match(/^(\d{4})-?(\d{2})-?(\d{2})$/);
  if (m) return `${m[1]}-${m[2]}-${m[3]}`;
  m = v.match(/^--(\d{2})-?(\d{2})$/);
  if (m) return `--${m[1]}-${m[2]}`;
  return null;
}

function phoneOk(v: string): boolean {
  return /^[+\d\s().-]+$/.test(v) && (v.match(/\d/g)?.length ?? 0) >= 7;
}

export function parseVcards(text: string): { cards: ParsedCard[]; tooMany: boolean } {
  const cards: ParsedCard[] = [];
  let card: ParsedCard | null = null;
  let lines: Line[] = [];
  const extraNotes: string[] = [];

  const finish = () => {
    if (!card) return;
    const groupLabels: Record<string, string> = {};
    for (const l of lines) if (l.name === "X-ABLABEL" && l.group) groupLabels[l.group] = cleanLabel(unescape(l.value));
    for (const l of lines) apply(card, l, groupLabels, extraNotes);
    if (extraNotes.length) card.notes = [card.notes, ...extraNotes].filter(Boolean).join("\n");
    extraNotes.length = 0;
    const named = [card.display, card.name.given, card.name.family, card.nickname, card.company].some(Boolean);
    if (named || card.phones.length || card.emails.length) cards.push(card);
  };

  for (const raw of unfold(text)) {
    const line = parseLine(raw);
    if (!line) continue;
    if (line.name === "BEGIN" && line.value.trim().toUpperCase() === "VCARD") {
      card = blank();
      lines = [];
    } else if (line.name === "END" && line.value.trim().toUpperCase() === "VCARD") {
      finish();
      card = null;
      if (cards.length > MAX_CARDS) return { cards: cards.slice(0, MAX_CARDS), tooMany: true };
    } else if (card) {
      lines.push(line);
    }
  }
  return { cards, tooMany: false };
}

function apply(card: ParsedCard, l: Line, groupLabels: Record<string, string>, notes: string[]) {
  if ((l.params.ENCODING ?? []).some((e) => /^(b|base64)$/i.test(e))) return; // Pictures and keys.
  const label = labelOf(l, groupLabels);
  const value = unescape(l.value);
  switch (l.name) {
    case "FN":
      card.display = value;
      break;
    case "N": {
      const [family = "", given = "", middle = "", prefix = "", suffix = ""] = split(l.value, ";");
      card.name = { family, given, middle, prefix, suffix };
      break;
    }
    case "NICKNAME":
      card.nickname = value;
      break;
    case "TEL":
      if (phoneOk(value)) card.phones.push({ label, value });
      else if (value) notes.push(`Phone${label ? ` (${label})` : ""}: ${value}`);
      break;
    case "EMAIL":
      if (value) card.emails.push({ label, value });
      break;
    case "ORG": {
      const [company = "", department = ""] = split(l.value, ";");
      card.company = company;
      card.department = department;
      break;
    }
    case "TITLE":
      card.title = value;
      break;
    case "ADR": {
      const [pobox = "", ext = "", street = "", city = "", region = "", postcode = "", country = ""] = split(l.value, ";");
      const address = { label, street: [pobox, ext, street].filter(Boolean).join(", "), city, region, postcode, country };
      if (address.street + city + region + postcode + country) card.addresses.push(address);
      break;
    }
    case "URL":
      if (value) card.urls.push({ label, value });
      break;
    case "BDAY":
    case "ANNIVERSARY":
    case "X-ANNIVERSARY":
    case "X-ABDATE": {
      const date = normalDate(value);
      const what = l.name === "BDAY" ? "birthday" : l.name === "X-ABDATE" ? label || "date" : "anniversary";
      if (date) card.dates.push({ label: what, value: date });
      else if (value) notes.push(`${what}: ${value}`);
      break;
    }
    case "RELATED":
    case "X-ABRELATEDNAMES":
      if (value) card.related.push({ label, value });
      break;
    case "IMPP": {
      const scheme = value.match(/^([a-z][a-z0-9+.-]*):(.*)$/i);
      card.handles.push(scheme ? { label: label || scheme[1], value: scheme[2] } : { label, value });
      break;
    }
    case "X-SOCIALPROFILE":
      if (value) card.handles.push({ label: label || "social", value });
      break;
    case "X-SKYPE":
    case "X-SKYPE-USERNAME":
    case "X-AIM":
    case "X-JABBER":
    case "X-ICQ":
    case "X-MSN":
    case "X-YAHOO":
    case "X-TWITTER":
      if (value) card.handles.push({ label: l.name.slice(2).toLowerCase().replace("-username", ""), value });
      break;
    case "NOTE":
      card.notes = [card.notes, value].filter(Boolean).join("\n");
      break;
  }
}
