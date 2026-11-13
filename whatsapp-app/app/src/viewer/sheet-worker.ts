// Reads a spreadsheet off the viewer's main thread. Formulas aren't evaluated and macros aren't read: only the values
// the file already holds, capped in rows, columns and cell length.

import * as XLSX from "xlsx";

export const MAX_ROWS = 2000;
export const MAX_COLS = 60;
export const MAX_SHEETS = 30;
const MAX_CELL = 400;

self.onmessage = (e: MessageEvent<{ bytes: ArrayBuffer }>) => {
  try {
    const wb = XLSX.read(new Uint8Array(e.data.bytes), {
      type: "array",
      dense: true,
      sheetRows: MAX_ROWS + 1,
      cellFormula: false,
      cellHTML: false,
      cellStyles: false,
      bookVBA: false,
    });
    const sheets = wb.SheetNames.slice(0, MAX_SHEETS).map((name) => {
      const rows = XLSX.utils.sheet_to_json<unknown[]>(wb.Sheets[name], { header: 1, raw: false, defval: "", blankrows: true });
      const wide = rows.some((r) => r.length > MAX_COLS);
      return {
        name,
        rows: rows.slice(0, MAX_ROWS).map((r) => r.slice(0, MAX_COLS).map((v) => String(v ?? "").slice(0, MAX_CELL))),
        moreRows: rows.length > MAX_ROWS,
        moreCols: wide,
      };
    });
    postMessage({ ok: true, sheets, totalSheets: wb.SheetNames.length });
  } catch (err) {
    postMessage({ ok: false, error: err instanceof Error ? err.message : String(err) });
  }
};
