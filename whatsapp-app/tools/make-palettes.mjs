// Builds app/src/lib/palettes.json: for each number of people, the set of colours that keeps the two
// closest ones as far apart as possible. Run by hand when the message colours change. The app only reads
// the table, so every copy of a version shows the same colours and opening a chat costs no searching.
//
//   node tools/make-palettes.mjs [out.json]
import { writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { contrast, ciede2000, hexOf, labOf, oklabDistance, oklabOf, rgbOf } from "../app/e2e/colour-math.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const out = process.argv[2] ?? path.join(here, "../app/src/lib/palettes.json");
const TABLE_TO = 64; // Largest group seen, each size with its own set.
const MORE_TO = 1024; // WhatsApp's largest group. Past the table, the set grows by adding to the last one.

// What a person's colour has to work against, from app.css. A name always sits on --theirs. --text and
// --muted are held apart too, so a coloured name can't read as plain text or as an unknown sender.
const THEMES = {
  light: { bg: "#ffffff", avoid: ["#15191e", "#5e6873"], lMin: 0.3, lMax: 1 },
  dark: { bg: "#22282e", avoid: ["#e8ebee", "#a0a9b3"], lMin: 0, lMax: 0.95 },
};

const MIN_CONTRAST = 4.6; // 4.5:1 is the target. This leaves room for rounding.

function candidates(theme, step) {
  const found = [];
  for (let r = 0; r <= 255; r += step)
    for (let g = 0; g <= 255; g += step)
      for (let b = 0; b <= 255; b += step) {
        const rgb = [r, g, b];
        if (contrast(rgb, rgbOf(theme.bg)) < MIN_CONTRAST) continue;
        const ok = oklabOf(rgb);
        if (ok[0] < theme.lMin || ok[0] > theme.lMax) continue;
        found.push({ hex: hexOf(rgb), lab: labOf(rgb), ok });
      }
  return found;
}

// The average of CIEDE2000 and OKLab, on OKLab's scale times 100. Either one alone leans a certain way:
// CIEDE2000 squeezes blues together, and OKLab keeps picking blues and magentas. On their average, 3 or 4
// people get the clearly different families you'd expect, like green, red, blue and pink.
function distance(a, b) {
  return (ciede2000(a.lab, b.lab) + oklabDistance(a.ok, b.ok) * 100) / 2;
}

function seeded(seed) {
  return () => {
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

class Search {
  constructor(theme, step) {
    this.items = candidates(theme, step);
    const size = this.items.length;
    this.size = size;
    this.d = new Float32Array(size * size);
    for (let i = 0; i < size; i++)
      for (let j = i + 1; j < size; j++) {
        const v = distance(this.items[i], this.items[j]);
        this.d[i * size + j] = v;
        this.d[j * size + i] = v;
      }
    const avoid = theme.avoid.map((hex) => {
      const rgb = rgbOf(hex);
      return { lab: labOf(rgb), ok: oklabOf(rgb) };
    });
    this.anchor = new Float32Array(size);
    for (let i = 0; i < size; i++) this.anchor[i] = Math.min(...avoid.map((a) => distance(this.items[i], a)));
  }

  /** How close candidate c is to everything else in the set, leaving out position skip. */
  nearest(c, set, skip = -1) {
    let best = this.anchor[c];
    const row = c * this.size;
    for (let i = 0; i < set.length; i++) {
      if (i === skip) continue;
      const v = this.d[row + set[i]];
      if (v < best) best = v;
    }
    return best;
  }

  score(set) {
    let low = Infinity;
    let total = 0;
    for (let i = 0; i < set.length; i++) {
      const v = this.nearest(set[i], set, i);
      total += v;
      if (v < low) low = v;
    }
    return { low, mean: total / set.length };
  }

  farthestFirst(start, n) {
    const set = [start];
    while (set.length < n) {
      let pick = -1;
      let best = -1;
      for (let c = 0; c < this.size; c++) {
        const v = this.nearest(c, set);
        if (v > best) {
          best = v;
          pick = c;
        }
      }
      set.push(pick);
    }
    return set;
  }

  /** Moves whichever colours are closest apart to wherever they'd be furthest from the rest. */
  improve(set) {
    for (let round = 0; round < 40 * set.length; round++) {
      let moved = false;
      const near = set.map((c, i) => this.nearest(c, set, i));
      const low = Math.min(...near);
      for (let i = 0; i < set.length; i++) {
        if (near[i] > low + 0.001) continue;
        let pick = set[i];
        let best = near[i];
        for (let c = 0; c < this.size; c++) {
          const v = this.nearest(c, set, i);
          if (v > best + 0.0001) {
            best = v;
            pick = c;
          }
        }
        if (pick !== set[i]) {
          set[i] = pick;
          moved = true;
        }
      }
      if (!moved) break;
    }
    // Then gives every colour room, without letting the closest pair get closer.
    for (let round = 0; round < 3; round++) {
      for (let i = 0; i < set.length; i++) {
        let pick = set[i];
        let best = this.nearest(pick, set, i);
        for (let c = 0; c < this.size; c++) {
          const v = this.nearest(c, set, i);
          if (v > best + 0.0001) {
            best = v;
            pick = c;
          }
        }
        set[i] = pick;
      }
    }
    return set;
  }

  best(n, starts, seed, previous) {
    const random = seeded(seed);
    let winner = null;
    let score = { low: -1, mean: -1 };
    const consider = (set) => {
      this.improve(set);
      const s = this.score(set);
      if (s.low > score.low + 0.0001 || (Math.abs(s.low - score.low) <= 0.0001 && s.mean > score.mean)) {
        winner = [...set];
        score = s;
      }
    };
    for (let i = 0; i < starts; i++) consider(this.farthestFirst(Math.floor(random() * this.size), n));
    if (previous) {
      // The set for one fewer person, plus the best place for one more, sometimes beats a fresh start.
      const set = [...previous];
      let pick = 0;
      let top = -1;
      for (let c = 0; c < this.size; c++) {
        const v = this.nearest(c, set);
        if (v > top) {
          top = v;
          pick = c;
        }
      }
      set.push(pick);
      consider(set);
    }
    return { set: winner, score };
  }
}

const result = {};
for (const [name, theme] of Object.entries(THEMES)) {
  const search = new Search(theme, 15);
  console.log(name, "candidates", search.size);
  const table = [];
  let previous = null;
  for (let n = 1; n <= TABLE_TO; n++) {
    const { set, score } = search.best(n, n <= 16 ? 24 : 8, n * 7919, previous);
    previous = set;
    table.push(set.map((c) => search.items[c].hex).join(""));
    if (n <= 12 || n % 8 === 0) console.log(`${name} n=${n} closest=${score.low.toFixed(1)}`);
  }
  result[name] = table;

  // Beyond the table: keep adding the furthest colour left, from a finer grid so they don't run out.
  const fine = candidates(theme, 5);
  const taken = table[TABLE_TO - 1].match(/.{6}/g).map((hex) => {
    const rgb = rgbOf(`#${hex}`);
    return { hex, lab: labOf(rgb), ok: oklabOf(rgb) };
  });
  const near = fine.map((c) => {
    let best = Infinity;
    for (const hex of theme.avoid) {
      const rgb = rgbOf(hex);
      best = Math.min(best, distance(c, { lab: labOf(rgb), ok: oklabOf(rgb) }));
    }
    for (const t of taken) best = Math.min(best, distance(c, t));
    return best;
  });
  let more = "";
  for (let n = TABLE_TO + 1; n <= MORE_TO; n++) {
    let pick = 0;
    for (let c = 1; c < fine.length; c++) if (near[c] > near[pick]) pick = c;
    more += fine[pick].hex;
    if (n % 128 === 0) console.log(`${name} n=${n} closest=${near[pick].toFixed(1)}`);
    const chosen = fine[pick];
    for (let c = 0; c < fine.length; c++) {
      const v = distance(fine[c], chosen);
      if (v < near[c]) near[c] = v;
    }
  }
  result[`${name}More`] = more;
}
writeFileSync(out, JSON.stringify(result));
console.log("wrote", out);
