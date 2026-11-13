// Which files WA Relay can preview, and how big. Everything else keeps its Open and Save buttons only.

export type PreviewKind = "pdf" | "docx" | "sheet" | "pptx" | "text" | "svg";

export const LIMITS: Record<PreviewKind, number> = {
  pdf: 100 * 1024 * 1024,
  docx: 30 * 1024 * 1024,
  sheet: 30 * 1024 * 1024,
  pptx: 100 * 1024 * 1024,
  text: 20 * 1024 * 1024,
  svg: 5 * 1024 * 1024,
};

const BY_EXT: Record<string, PreviewKind> = {
  pdf: "pdf",
  docx: "docx",
  docm: "docx",
  dotx: "docx",
  xlsx: "sheet",
  xlsm: "sheet",
  xlsb: "sheet",
  xls: "sheet",
  ods: "sheet",
  csv: "sheet",
  tsv: "sheet",
  pptx: "pptx",
  pptm: "pptx",
  txt: "text",
  md: "text",
  markdown: "text",
  json: "text",
  log: "text",
  xml: "text",
  yaml: "text",
  yml: "text",
  ini: "text",
  cfg: "text",
  conf: "text",
  toml: "text",
  // Shown as text, never run or rendered.
  html: "text",
  htm: "text",
  js: "text",
  ts: "text",
  py: "text",
  ps1: "text",
  bat: "text",
  cmd: "text",
  sh: "text",
  c: "text",
  h: "text",
  cpp: "text",
  cs: "text",
  java: "text",
  go: "text",
  rs: "text",
  sql: "text",
  css: "text",
  svg: "svg",
};

const BY_MIME: Record<string, PreviewKind> = {
  "application/pdf": "pdf",
  "application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "sheet",
  "application/vnd.ms-excel": "sheet",
  "text/csv": "sheet",
  "application/vnd.openxmlformats-officedocument.presentationml.presentation": "pptx",
  "text/plain": "text",
  "application/json": "text",
};

export function previewKind(filename: string, mime: string): PreviewKind | null {
  const ext = (filename.split(".").pop() ?? "").toLowerCase();
  if (filename.includes(".") && BY_EXT[ext]) return BY_EXT[ext];
  return BY_MIME[(mime || "").split(";")[0].trim().toLowerCase()] ?? null;
}
