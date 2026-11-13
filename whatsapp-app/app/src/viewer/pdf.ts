// PDF through PDF.js, parsed in a worker. A PDF's own scripts never run: PDF.js only runs them through its
// scripting sandbox, which isn't loaded here, and eval and XFA forms are off. Pages draw as they come into view.

import * as pdfjs from "pdfjs-dist";
import PdfWorker from "pdfjs-dist/build/pdf.worker.min.mjs?worker&inline";

const MAX_PAGES = 300;

export async function render(root: HTMLElement, bytes: ArrayBuffer, onFirst: () => void): Promise<string> {
  pdfjs.GlobalWorkerOptions.workerPort = new PdfWorker();
  const doc = await pdfjs.getDocument({
    data: new Uint8Array(bytes),
    isEvalSupported: false,
    enableXfa: false,
    disableAutoFetch: true,
    stopAtErrors: false,
  }).promise;
  const count = Math.min(doc.numPages, MAX_PAGES);
  const first = await doc.getPage(1);
  const base = first.getViewport({ scale: 1 });
  const holder = document.createElement("div");
  holder.className = "pages";
  root.append(holder);
  const width = Math.min(root.clientWidth - 24, 1100);
  const scale = width / base.width;
  const ratio = Math.min(window.devicePixelRatio || 1, 2);
  let firstDone = false;

  const draw = async (n: number, box: HTMLElement) => {
    const page = n === 1 ? first : await doc.getPage(n);
    const view = page.getViewport({ scale });
    box.style.width = `${view.width}px`;
    box.style.height = `${view.height}px`;
    const canvas = document.createElement("canvas");
    canvas.width = Math.floor(view.width * ratio);
    canvas.height = Math.floor(view.height * ratio);
    canvas.setAttribute("aria-label", `Page ${n}`);
    box.append(canvas);
    await page.render({ canvas, viewport: page.getViewport({ scale: scale * ratio }) }).promise;
    if (!firstDone) {
      firstDone = true;
      onFirst();
    }
  };

  const watcher = new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        if (!entry.isIntersecting) continue;
        const box = entry.target as HTMLElement;
        watcher.unobserve(box);
        void draw(Number(box.dataset.page), box).catch(() => {
          box.textContent = `Page ${box.dataset.page} couldn't be shown.`;
        });
      }
    },
    { rootMargin: "800px 0px" },
  );
  for (let n = 1; n <= count; n++) {
    const box = document.createElement("div");
    box.className = "page";
    box.dataset.page = String(n);
    box.style.width = `${base.width * scale}px`;
    box.style.height = `${base.height * scale}px`;
    holder.append(box);
    watcher.observe(box);
  }
  const pages = `${doc.numPages} ${doc.numPages === 1 ? "page" : "pages"}`;
  return doc.numPages > MAX_PAGES ? `${pages}. Showing the first ${MAX_PAGES}: open it for the rest. Links in a PDF aren't clickable here.` : "";
}
