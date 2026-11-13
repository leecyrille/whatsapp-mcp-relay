// Plain text, code, logs, Markdown, JSON and HTML: always as text, never rendered or run.

const MAX_CHARS = 2_000_000;

export function render(root: HTMLElement, bytes: ArrayBuffer, name: string): string {
  let text = new TextDecoder("utf-8", { fatal: false }).decode(bytes);
  if (text.charCodeAt(0) === 0xfeff) text = text.slice(1);
  const cut = text.length > MAX_CHARS;
  if (cut) text = text.slice(0, MAX_CHARS);
  if (!cut && /\.json$/i.test(name)) {
    try {
      text = JSON.stringify(JSON.parse(text), null, 2);
    } catch {
      /* Shown as it is. */
    }
  }
  const pre = document.createElement("pre");
  pre.className = "text";
  pre.textContent = text;
  root.append(pre);
  const notes = [];
  if (/\.html?$/i.test(name)) notes.push("A web page, shown as its source text. Nothing in it runs or loads.");
  if (cut) notes.push("Showing the first 2 million characters. Open it for the rest.");
  return notes.join(" ");
}
