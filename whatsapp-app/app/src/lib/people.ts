import table from "./palettes.json";

export type PersonColour = { light: string; dark: string };

function colourSet(sets: string[], more: string, count: number): string[] {
  if (count === 0) return [];
  const packed = count <= sets.length ? sets[count - 1] : sets[sets.length - 1] + more;
  const colours = packed.match(/.{6}/g)!.map((hex) => `#${hex}`);
  return Array.from({ length: count }, (_, i) => colours[i % colours.length]);
}

/**
 * A colour for each person, from the set made for exactly that many people (see tools/make-palettes.mjs).
 * It's worked out only from who's there, in a fixed order, so nothing is stored and every copy of the app
 * that knows the same people shows the same colours. Past WhatsApp's largest group they'd start to repeat.
 */
export function personColours(people: string[]): Map<string, PersonColour> {
  const sorted = [...new Set(people)].sort();
  const light = colourSet(table.light, table.lightMore, sorted.length);
  const dark = colourSet(table.dark, table.darkMore, sorted.length);
  return new Map(sorted.map((person, i) => [person, { light: light[i], dark: dark[i] }]));
}
