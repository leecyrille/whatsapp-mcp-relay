// Made-up documents for the preview tests, including hostile ones. Nothing here comes from a real file.
import JSZip from "jszip";
import * as XLSX from "xlsx";

/** A small PDF writer: pages of text, optionally a grey picture on each, and optionally an open action that tries to run script. */
export function pdf(pages, { imageSide = 0, hostile = false } = {}) {
  const parts = [];
  const offsets = [];
  let length = 0;
  const add = (chunk) => {
    const buf = typeof chunk === "string" ? Buffer.from(chunk, "latin1") : chunk;
    parts.push(buf);
    length += buf.length;
  };
  const obj = (n, body) => {
    offsets[n] = length;
    add(`${n} 0 obj\n`);
    for (const b of Array.isArray(body) ? body : [body]) add(b);
    add(`\nendobj\n`);
  };
  add("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n");
  const first = 4;
  const kids = [];
  for (let i = 0; i < pages; i++) kids.push(`${first + i * 3} 0 R`);
  obj(1, `<< /Type /Catalog /Pages 2 0 R${hostile ? " /OpenAction << /S /JavaScript /JS (app.alert('made-up'); this.submitForm('http://example.invalid/')) >>" : ""} >>`);
  obj(2, `<< /Type /Pages /Kids [${kids.join(" ")}] /Count ${pages} >>`);
  obj(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>");
  for (let i = 0; i < pages; i++) {
    const page = first + i * 3;
    const text = `BT /F1 28 Tf 72 720 Td (Made-up page ${i + 1}) Tj ET${imageSide ? ` q 400 0 0 400 100 200 cm /Im${i} Do Q` : ""}`;
    obj(page, `<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >>${imageSide ? ` /XObject << /Im${i} ${page + 2} 0 R >>` : ""} >> /Contents ${page + 1} 0 R >>`);
    obj(page + 1, [`<< /Length ${text.length} >>\nstream\n`, text, "\nendstream"]);
    if (imageSide) {
      const pixels = Buffer.alloc(imageSide * imageSide);
      for (let p = 0; p < pixels.length; p++) pixels[p] = (p * 7 + i * 13) & 0xff;
      obj(page + 2, [`<< /Type /XObject /Subtype /Image /Width ${imageSide} /Height ${imageSide} /ColorSpace /DeviceGray /BitsPerComponent 8 /Length ${pixels.length} >>\nstream\n`, pixels, "\nendstream"]);
    } else {
      obj(page + 2, "<< >>");
    }
  }
  const count = first + pages * 3;
  const xref = length;
  add(`xref\n0 ${count}\n0000000000 65535 f \n`);
  for (let n = 1; n < count; n++) add(`${String(offsets[n]).padStart(10, "0")} 00000 n \n`);
  add(`trailer\n<< /Size ${count} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`);
  return Buffer.concat(parts);
}

const W = 'xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"';

/** A Word file. hostile adds a link, a macro part and a remote picture, none of which may do anything. */
export async function docx(paragraphs, { hostile = false } = {}) {
  const zip = new JSZip();
  zip.file("[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`);
  zip.file("_rels/.rels", `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`);
  const rels = [`<Relationship Id="rLink" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="https://example.com/made-up-link" TargetMode="External"/>`];
  if (hostile) rels.push(`<Relationship Id="rImg" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="http://example.invalid/track.png" TargetMode="External"/>`);
  zip.file("word/_rels/document.xml.rels", `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">${rels.join("")}</Relationships>`);
  if (hostile) zip.file("word/vbaProject.bin", Buffer.from("made-up macro that must never run"));
  const body = [];
  body.push(`<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>Made-up heading</w:t></w:r></w:p>`);
  body.push(`<w:p><w:hyperlink r:id="rLink"><w:r><w:t>A made-up link</w:t></w:r></w:hyperlink></w:p>`);
  if (hostile) body.push(`<w:p><w:r><w:t xml:space="preserve">&lt;script&gt;parent.hacked=1&lt;/script&gt; &lt;img src=x onerror=alert(1)&gt;</w:t></w:r></w:p>`);
  // Varied words, so a long file is long once zipped too.
  let seed = 7;
  const word = () => {
    seed = (seed * 1103515245 + 12345) % 2147483648;
    return "abcdefghijklmnopqrstuvwxyz".slice(seed % 20, (seed % 20) + 3 + (seed % 5));
  };
  for (let i = 0; i < paragraphs; i++) body.push(`<w:p><w:r><w:t>Made-up paragraph ${i + 1} ${Array.from({ length: 30 }, word).join(" ")}</w:t></w:r></w:p>`);
  zip.file("word/document.xml", `<?xml version="1.0" encoding="UTF-8"?><w:document ${W}><w:body>${body.join("")}</w:body></w:document>`);
  return zip.generateAsync({ type: "nodebuffer", compression: "DEFLATE" });
}

/** A workbook with two sheets; the first can be large. */
export function xlsx(rows) {
  const wb = XLSX.utils.book_new();
  const big = [["Job", "Site", "Hours", "Rate", "Total"]];
  for (let i = 1; i <= rows; i++) big.push([`J-${1000 + i}`, `Made-up site ${i % 40}`, i % 12, 95, (i % 12) * 95]);
  XLSX.utils.book_append_sheet(wb, XLSX.utils.aoa_to_sheet(big), "Hours");
  const ws = XLSX.utils.aoa_to_sheet([["Formula"], [1], [2], [{ t: "n", f: "SUM(A2:A3)", v: 3 }]]);
  XLSX.utils.book_append_sheet(wb, ws, "Second sheet");
  return Buffer.from(XLSX.write(wb, { type: "buffer", bookType: "xlsx" }));
}

/** A deck: two slides with text, one with a picture. */
export async function pptx(png) {
  const zip = new JSZip();
  const A = 'xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"';
  zip.file("ppt/slides/slide1.xml", `<?xml version="1.0"?><p:sld ${A}><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Made-up deck title</a:t></a:r></a:p><a:p><a:r><a:t>First point</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`);
  zip.file("ppt/slides/slide2.xml", `<?xml version="1.0"?><p:sld ${A}><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Second slide</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`);
  zip.file("ppt/slides/_rels/slide2.xml.rels", `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="r1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/image1.png"/><Relationship Id="r2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="http://example.invalid/remote.png" TargetMode="External"/></Relationships>`);
  zip.file("ppt/media/image1.png", png);
  return zip.generateAsync({ type: "nodebuffer" });
}
