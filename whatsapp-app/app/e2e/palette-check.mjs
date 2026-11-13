// Measures the message colours, without the app: how far apart the closest two are at each group size,
// whether they're readable in each theme, and whether the same people always get the same colours.
//
//   node e2e/palette-check.mjs
import { readFileSync } from "node:fs";
import { ciede2000, contrast, labOf, oklabDistance, oklabOf, rgbOf } from "./colour-math.mjs";

const table = JSON.parse(readFileSync(new URL("../src/lib/palettes.json", import.meta.url), "utf8"));
// The app's own code, minus its type notes.
const source = readFileSync(new URL("../src/lib/people.ts", import.meta.url), "utf8")
  .replace(/^import table.*$/m, "")
  .replace(/export type .*$/m, "")
  .replace(/\(sets: string\[\], more: string, count: number\): string\[\]/, "(sets, more, count)")
  .replace(/\(people: string\[\]\): Map<string, PersonColour>/, "(people)")
  .replace(/export function/, "function")
  .replace(/!\.map/, ".map");
const personColours = new Function("table", `${source}; return personColours;`)(table);

let failures = 0;
function check(name, ok, detail = "") {
  if (!ok) failures++;
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? ` :: ${detail}` : ""}`);
}

// Known answers, from the CIEDE2000 paper's test data and the OKLab definition.
for (const [a, b, want] of [
  [[50, 2.6772, -79.7751], [50, 0, -82.7485], 2.0425],
  [[50, 2.5, 0], [50, 0, -2.5], 4.3065],
  [[50, 2.5, 0], [73, 25, -18], 27.1492],
  [[50, 2.49, -0.001], [50, -2.49, 0.0011], 7.2195],
  [[60.2574, -34.0099, 36.2677], [60.4626, -34.1751, 39.4387], 1.2644],
]) {
  check(`CIEDE2000 gives the published ${want}`, Math.abs(ciede2000(a, b) - want) < 0.0001);
}
check("OKLab gives the published red", oklabDistance(oklabOf([255, 0, 0]), [0.628, 0.2249, 0.1258]) < 0.001);

const THEMES = {
  light: { bg: "#ffffff", avoid: ["#15191e", "#5e6873"] },
  dark: { bg: "#22282e", avoid: ["#e8ebee", "#a0a9b3"] },
};
const OLD = ["#2f7f6f", "#3b6fb5", "#8a5cb8", "#b5603b", "#a8456f", "#3f8c4c", "#6b6fb8", "#a07f2c"];
const ids = (n) => Array.from({ length: n }, (_, i) => `1555${String(i).padStart(7, "0")}@s.whatsapp.net`);

function closest(colours) {
  let de = Infinity;
  let ok = Infinity;
  const points = colours.map((hex) => ({ lab: labOf(rgbOf(hex)), ok: oklabOf(rgbOf(hex)) }));
  for (let i = 0; i < points.length; i++)
    for (let j = i + 1; j < points.length; j++) {
      de = Math.min(de, ciede2000(points[i].lab, points[j].lab));
      ok = Math.min(ok, oklabDistance(points[i].ok, points[j].ok) * 100);
    }
  return { de, ok };
}

console.log("\npeople | light: closest CIEDE2000, OKLab, worst contrast | dark: the same");
const SIZES = [2, 3, 4, 5, 6, 8, 10, 12, 16, 20, 24, 32, 48, 64, 65, 100, 256, 512, 1024];
const measured = {};
for (const n of SIZES) {
  const map = personColours(ids(n));
  const line = [];
  for (const [theme, { bg }] of Object.entries(THEMES)) {
    const colours = [...map.values()].map((c) => `${c[theme]}`);
    const { de, ok } = closest(colours);
    const worst = Math.min(...colours.map((hex) => contrast(rgbOf(hex), rgbOf(bg))));
    measured[`${theme}${n}`] = { de, ok, worst, distinct: new Set(colours).size };
    line.push(`${de.toFixed(1).padStart(5)} ${ok.toFixed(1).padStart(5)} ${worst.toFixed(2)}:1`);
  }
  console.log(`${String(n).padStart(6)} | ${line.join(" | ")}`);
}
console.log("");

for (const n of SIZES) {
  const ok = Object.keys(THEMES).every((t) => measured[`${t}${n}`].worst >= 4.5);
  check(`${n} people: every colour reads at 4.5:1 or better on a message, in both themes`, ok);
  check(`${n} people: all different`, Object.keys(THEMES).every((t) => measured[`${t}${n}`].distinct === n));
}

// Every size up to the top of the table, not just the ones listed above.
for (const theme of Object.keys(THEMES)) {
  let bad = 0;
  for (let n = 1; n <= table[theme].length; n++) {
    const set = table[theme][n - 1].match(/.{6}/g);
    if (set.length !== n) bad++;
    if (set.some((hex) => contrast(rgbOf(`#${hex}`), rgbOf(THEMES[theme].bg)) < 4.5)) bad++;
    if (THEMES[theme].avoid.some((a) => set.some((hex) => ciede2000(labOf(rgbOf(a)), labOf(rgbOf(`#${hex}`))) < 2))) bad++;
  }
  check(`${theme}: every size to ${table[theme].length} has the right count, reads clearly, and can't pass for plain text`, bad === 0);
}

// The set changes with the number of people, because the best 8 aren't the best 3 plus 5 more.
const three = new Set([...personColours(ids(3)).values()].map((c) => c.light));
const eight = new Set([...personColours(ids(8)).values()].map((c) => c.light));
check("the set is chosen for the number of people, not cut from one long list", [...three].some((c) => !eight.has(c)));

// Better than the colours names used before, at any size those could manage.
const old = closest(OLD);
for (const n of [2, 3, 4, 5, 6, 8]) {
  const now = measured[`light${n}`];
  check(`${n} people: further apart than any pair could be sure of with the old 8 colours (${old.de.toFixed(1)})`, now.de > old.de && now.ok > old.ok, `${now.de.toFixed(1)} / ${now.ok.toFixed(1)}`);
}

// Fewer people means more room. Not strictly at each step, since the two measures vary, but clearly.
for (const theme of Object.keys(THEMES)) {
  const at = (n) => measured[`${theme}${n}`];
  check(`${theme}: closest pairs get closer as a group grows`, at(3).de > at(8).de && at(8).de > at(16).de && at(16).de > at(64).de && at(64).de > at(1024).de);
  // Floors, so a rebuilt table can't quietly get worse. Around 2.3 is the least people can see side by side.
  check(`${theme}: 16 people, the closest pair is at least 13 / 10`, at(16).de >= 13 && at(16).ok >= 10, `${at(16).de.toFixed(1)} / ${at(16).ok.toFixed(1)}`);
  check(`${theme}: 64 people, the closest pair is at least 7 / 4.5`, at(64).de >= 7 && at(64).ok >= 4.5, `${at(64).de.toFixed(1)} / ${at(64).ok.toFixed(1)}`);
}

// Nothing per device: the same people give the same colours, in any order, with duplicates.
const people = ids(12);
const base = JSON.stringify([...personColours(people)]);
const shuffled = [...people].reverse();
check("the same people get the same colours, in any order", JSON.stringify([...personColours(shuffled)]) === base);
check("someone listed twice is one person", JSON.stringify([...personColours([...people, people[3]])]) === base);
check("nobody is fine", personColours([]).size === 0);
check("one person gets a colour", personColours(ids(1)).size === 1);
const over = personColours(ids(1100));
check("past WhatsApp's largest group, colours repeat instead of failing", over.size === 1100 && [...over.values()].every((c) => /^#[0-9a-f]{6}$/.test(c.light) && /^#[0-9a-f]{6}$/.test(c.dark)));

console.log(`\n${failures === 0 ? "all passed" : `${failures} failed`}`);
process.exit(failures ? 1 : 0);
