// PowerPoint: the text and pictures of each slide, in order. No browser library lays slides out faithfully, so this
// doesn't pretend to: it says so, and Open shows the real thing.

import JSZip from "jszip";

const MAX_SLIDES = 300;
const MAX_PICTURES = 200;
const PICTURE_TYPES: Record<string, string> = { png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif", bmp: "image/bmp", webp: "image/webp" };

export async function render(root: HTMLElement, bytes: ArrayBuffer): Promise<string> {
  const zip = await JSZip.loadAsync(bytes);
  const slides = Object.keys(zip.files)
    .map((name) => ({ name, n: Number(name.match(/^ppt\/slides\/slide(\d+)\.xml$/)?.[1]) }))
    .filter((s) => s.n > 0)
    .sort((a, b) => a.n - b.n);
  if (!slides.length) throw new Error("No slides found in that file.");
  let pictures = 0;
  for (const slide of slides.slice(0, MAX_SLIDES)) {
    const box = document.createElement("section");
    box.className = "slide";
    const title = document.createElement("h2");
    title.textContent = `Slide ${slide.n}`;
    box.append(title);
    const xml = new DOMParser().parseFromString(await zip.file(slide.name)!.async("string"), "application/xml");
    for (const para of xml.getElementsByTagNameNS("*", "p")) {
      const text = [...para.getElementsByTagNameNS("*", "t")].map((t) => t.textContent ?? "").join("");
      if (text.trim()) {
        const p = document.createElement("p");
        p.textContent = text;
        box.append(p);
      }
    }
    const rels = zip.file(`ppt/slides/_rels/slide${slide.n}.xml.rels`);
    if (rels) {
      const doc = new DOMParser().parseFromString(await rels.async("string"), "application/xml");
      for (const rel of doc.getElementsByTagNameNS("*", "Relationship")) {
        if (!/\/image$/.test(rel.getAttribute("Type") ?? "") || rel.getAttribute("TargetMode") === "External" || pictures >= MAX_PICTURES) continue;
        const target = (rel.getAttribute("Target") ?? "").replace(/^\.\.\//, "ppt/");
        const type = PICTURE_TYPES[(target.split(".").pop() ?? "").toLowerCase()];
        const file = zip.file(target);
        if (!type || !file) continue;
        const img = document.createElement("img");
        img.alt = `Picture on slide ${slide.n}`;
        img.src = URL.createObjectURL(new Blob([await file.async("uint8array")], { type }));
        box.append(img);
        pictures++;
      }
    }
    root.append(box);
  }
  const more = slides.length > MAX_SLIDES ? ` Showing the first ${MAX_SLIDES} of ${slides.length} slides.` : "";
  return `The text and pictures from each slide, not its layout. Open it in PowerPoint to see the slides themselves.${more}`;
}
