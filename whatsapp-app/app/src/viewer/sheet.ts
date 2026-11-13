// Spreadsheets: sheet tabs and a table that only draws the rows in view.

import SheetWorker from "./sheet-worker.ts?worker&inline";

interface Sheet {
  name: string;
  rows: string[][];
  moreRows: boolean;
  moreCols: boolean;
}

const ROW = 24;

function column(n: number): string {
  let s = "";
  for (n++; n > 0; n = Math.floor((n - 1) / 26)) s = String.fromCharCode(65 + ((n - 1) % 26)) + s;
  return s;
}

export async function render(root: HTMLElement, bytes: ArrayBuffer): Promise<string> {
  const worker = new SheetWorker();
  const result = await new Promise<{ ok: boolean; sheets?: Sheet[]; totalSheets?: number; error?: string }>((resolve) => {
    worker.onmessage = (e) => resolve(e.data);
    worker.onerror = (e) => resolve({ ok: false, error: e.message });
    worker.postMessage({ bytes }, [bytes]);
  });
  worker.terminate();
  if (!result.ok || !result.sheets) throw new Error(result.error ?? "That spreadsheet couldn't be read.");
  const sheets = result.sheets;
  const tabs = document.createElement("div");
  tabs.className = "tabs";
  tabs.setAttribute("role", "tablist");
  const note = document.createElement("p");
  note.className = "note";
  const grid = document.createElement("div");
  grid.className = "grid";
  root.append(tabs, note, grid);

  const show = (i: number) => {
    for (const [j, b] of [...tabs.children].entries()) b.setAttribute("aria-selected", String(i === j));
    const sheet = sheets[i];
    const width = Math.max(1, ...sheet.rows.map((r) => r.length));
    const limits = [sheet.moreRows ? "the first 2,000 rows" : "", sheet.moreCols ? "the first 60 columns" : ""].filter(Boolean);
    note.textContent = `Values only: formulas show their saved results and nothing is recalculated.${limits.length ? ` Showing ${limits.join(" and ")}. Open it for the rest.` : ""}`;
    grid.replaceChildren();
    grid.scrollTop = 0;
    const table = document.createElement("table");
    const head = document.createElement("tr");
    head.append(document.createElement("th"));
    for (let c = 0; c < width; c++) {
      const th = document.createElement("th");
      th.textContent = column(c);
      head.append(th);
    }
    const thead = document.createElement("thead");
    thead.append(head);
    const body = document.createElement("tbody");
    table.append(thead, body);
    const top = document.createElement("div");
    const bottom = document.createElement("div");
    grid.append(top, table, bottom);
    const draw = () => {
      const first = Math.max(0, Math.floor(grid.scrollTop / ROW) - 20);
      const last = Math.min(sheet.rows.length, first + Math.ceil(grid.clientHeight / ROW) + 40);
      top.style.height = `${first * ROW}px`;
      bottom.style.height = `${(sheet.rows.length - last) * ROW}px`;
      body.replaceChildren(
        ...sheet.rows.slice(first, last).map((r, k) => {
          const tr = document.createElement("tr");
          const th = document.createElement("th");
          th.className = "row";
          th.textContent = String(first + k + 1);
          tr.append(th);
          for (let c = 0; c < width; c++) {
            const td = document.createElement("td");
            td.textContent = r[c] ?? "";
            td.title = r[c] ?? "";
            tr.append(td);
          }
          return tr;
        }),
      );
    };
    grid.onscroll = draw;
    draw();
  };
  sheets.forEach((s, i) => {
    const b = document.createElement("button");
    b.setAttribute("role", "tab");
    b.textContent = s.name;
    b.onclick = () => show(i);
    tabs.append(b);
  });
  show(0);
  const total = result.totalSheets ?? sheets.length;
  return total > sheets.length ? `${total} sheets. Showing the first ${sheets.length}.` : "";
}
