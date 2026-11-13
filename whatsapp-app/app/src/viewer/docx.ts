// Word documents through docx-preview, which draws them as plain page markup. Macros are never read, pictures come
// from inside the file only, and links only ask the app.

import { renderAsync } from "docx-preview";

export async function render(root: HTMLElement, bytes: ArrayBuffer): Promise<string> {
  const host = document.createElement("div");
  host.className = "docx-host";
  root.append(host);
  await renderAsync(new Blob([bytes]), host, undefined, {
    inWrapper: true,
    breakPages: true,
    ignoreLastRenderedPageBreak: true,
    experimental: false,
    useBase64URL: true,
    renderChanges: false,
    renderComments: false,
    renderHeaders: true,
    renderFooters: true,
    renderFootnotes: true,
    renderEndnotes: true,
    debug: false,
  });
  // Anything the converter left that could fetch or run something is taken out.
  for (const el of host.querySelectorAll("script, iframe, object, embed, link, meta, base, form")) el.remove();
  for (const el of host.querySelectorAll("*")) {
    for (const attr of [...el.attributes]) if (/^on/i.test(attr.name)) el.removeAttribute(attr.name);
  }
  return "Layout is close to Word's, but not exact: fonts, spacing and some objects can differ. Open it in Word for the real thing.";
}
