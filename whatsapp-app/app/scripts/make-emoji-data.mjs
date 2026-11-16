// Builds src/lib/emoji-data.json, the emoji picker's whole list, from two MIT-licensed packages installed as
// dev dependencies: unicode-emoji-json (the Unicode order, categories, names, skin-tone support and versions)
// and emojilib (everyday words, so "lol" finds 😂). The app ships only the result and never fetches anything.
// Run again after updating either package:
//
//   npm run emoji-data
import { createRequire } from "node:module";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const groups = require("unicode-emoji-json/data-by-group.json");
const words = require("emojilib");
const { version } = require("unicode-emoji-json/package.json");

const MAX_WORDS = 12;
let top = "0";
const out = groups.map((g) => ({
  name: g.name,
  emoji: g.emojis.map((e) => {
    const inName = new Set(e.name.toLowerCase().split(/[^a-z0-9]+/).filter(Boolean));
    const extra = [];
    for (const w of words[e.emoji] ?? []) {
      const plain = w.toLowerCase().replace(/_/g, " ");
      if (plain === e.name.toLowerCase() || plain.split(" ").every((p) => inName.has(p))) continue;
      if (!extra.includes(plain)) extra.push(plain);
      if (extra.length === MAX_WORDS) break;
    }
    if (Number(e.emoji_version) > Number(top)) top = e.emoji_version;
    // [character, name, extra words, 1 if it takes a skin tone, emoji version]
    return [e.emoji, e.name, extra.join(","), e.skin_tone_support ? 1 : 0, e.emoji_version];
  }),
}));

const file = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src", "lib", "emoji-data.json");
const text = JSON.stringify({ source: `unicode-emoji-json ${version}, emojilib`, emoji_version: top, groups: out });
writeFileSync(file, text);
console.log(`wrote ${out.reduce((n, g) => n + g.emoji.length, 0)} emoji, up to Emoji ${top}, ${Math.round(text.length / 1024)} KB`);
