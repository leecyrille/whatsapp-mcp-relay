// The document viewer. It runs sandboxed in an origin of its own, with no access to the app or its commands. The app
// sends one file's bytes by message; this draws them and reports back. Each kind's library loads only when needed.

import "./viewer.css";

// The app's command bridge is put into every frame, this one too, and can't be removed. What keeps it harmless is that
// the host answers nothing from this frame: e2e/previews.mjs sends a command both ways and checks.

type Kind = "pdf" | "docx" | "sheet" | "pptx" | "text" | "svg";

const root = document.getElementById("root")!;

export function post(msg: Record<string, unknown>) {
  parent.postMessage(msg, "*");
}

export function note(text: string) {
  const p = document.createElement("p");
  p.className = "note";
  p.textContent = text;
  root.prepend(p);
}

// Links never open by themselves. A click asks the app, which shows the real address before anything opens.
document.addEventListener(
  "click",
  (e) => {
    const a = (e.target as Element | null)?.closest?.("a");
    if (!a) return;
    e.preventDefault();
    e.stopPropagation();
    const href = a.getAttribute("href") ?? "";
    if (/^(https?:|mailto:)/i.test(href)) post({ type: "link", url: (a as HTMLAnchorElement).href });
  },
  true,
);
// Nothing in a document may navigate this frame or submit anything.
document.addEventListener("submit", (e) => e.preventDefault(), true);
// With the keyboard in here, Escape still closes the preview.
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") post({ type: "close" });
});

let opened = false;
window.addEventListener("message", async (e) => {
  if (e.source !== parent || opened) return;
  const data = e.data as { type?: string; kind?: Kind; name?: string; bytes?: ArrayBuffer; theme?: string };
  if (data?.type !== "open" || !(data.bytes instanceof ArrayBuffer) || typeof data.kind !== "string") return;
  opened = true;
  document.documentElement.dataset.theme = data.theme === "dark" ? "dark" : "light";
  const started = performance.now();
  try {
    let detail = "";
    switch (data.kind) {
      case "pdf":
        detail = await (await import("./pdf")).render(root, data.bytes, () => post({ type: "first", ms: Math.round(performance.now() - started) }));
        break;
      case "docx":
        detail = await (await import("./docx")).render(root, data.bytes);
        break;
      case "sheet":
        detail = await (await import("./sheet")).render(root, data.bytes);
        break;
      case "pptx":
        detail = await (await import("./pptx")).render(root, data.bytes);
        break;
      case "svg":
        detail = renderSvg(data.bytes);
        break;
      default:
        detail = (await import("./text")).render(root, data.bytes, data.name ?? "");
    }
    if (detail) note(detail);
    post({ type: "rendered", ms: Math.round(performance.now() - started), detail });
  } catch (err) {
    post({ type: "error", message: err instanceof Error ? err.message : String(err) });
  }
});

/** An SVG only ever as a picture, where it can't run anything or load anything. */
function renderSvg(bytes: ArrayBuffer): string {
  const img = document.createElement("img");
  img.className = "svg";
  img.alt = "Picture";
  img.src = URL.createObjectURL(new Blob([bytes], { type: "image/svg+xml" }));
  root.append(img);
  return "";
}

post({ type: "ready" });
