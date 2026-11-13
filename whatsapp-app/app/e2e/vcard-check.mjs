// Checks for src/lib/vcard.ts on made-up files.
//   node --experimental-strip-types e2e/vcard-check.mjs
import { normalDate, parseVcards } from "../src/lib/vcard.ts";

const results = [];
function check(name, ok, detail = "") {
  results.push(ok);
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${!ok ? ` :: ${detail}` : ""}`);
}

const v3 = [
  "BEGIN:VCARD",
  "VERSION:3.0",
  "N:Example;Ann;Marie;Dr.;Jr.",
  "FN:Dr. Ann Example",
  "NICKNAME:Annie",
  "ORG:Made-up Co;Engineering",
  "TITLE:Site Engineer",
  "TEL;TYPE=CELL,VOICE,pref:+1 (555) 000-1001",
  "TEL;TYPE=WORK:555-000-1002 ext. 4",
  "EMAIL;TYPE=INTERNET,WORK:ann@example.com",
  "item1.EMAIL;TYPE=INTERNET:ann.home@example.com",
  "item1.X-ABLabel:_$!<Home>!$_",
  "ADR;TYPE=WORK:;Suite 2;1 Made-up St;Sampletown;AB;T0T 0T0;Canada",
  "URL:https://example.com/ann",
  "BDAY:1604-05-31",
  "X-ANNIVERSARY:2010-06-01",
  "item2.X-ABRELATEDNAMES:Bob Example",
  "item2.X-ABLabel:_$!<Spouse>!$_",
  "IMPP;X-SERVICE-TYPE=Skype:skype:ann.example",
  "X-SOCIALPROFILE;TYPE=linkedin:https://linkedin.example/ann",
  "NOTE:Met at the made-up fair\\, 2023.\\nLikes tea.",
  "PHOTO;ENCODING=b;TYPE=JPEG:AAAA",
  " BBBB",
  "END:VCARD",
].join("\r\n");

const { cards: [a] } = parseVcards(v3);
check("names", a.display === "Dr. Ann Example" && a.name.given === "Ann" && a.name.family === "Example" && a.name.middle === "Marie" && a.name.prefix === "Dr." && a.name.suffix === "Jr.", JSON.stringify(a.name));
check("nickname, company, department and title", a.nickname === "Annie" && a.company === "Made-up Co" && a.department === "Engineering" && a.title === "Site Engineer");
check("phones with their labels", JSON.stringify(a.phones) === JSON.stringify([{ label: "mobile", value: "+1 (555) 000-1001" }]), JSON.stringify(a.phones));
check("a number with words in it goes to the notes, not lost", a.notes.includes("Phone (work): 555-000-1002 ext. 4"), a.notes);
check("emails, with Apple's labels read", JSON.stringify(a.emails) === JSON.stringify([{ label: "work", value: "ann@example.com" }, { label: "Home", value: "ann.home@example.com" }]), JSON.stringify(a.emails));
check("an address in its parts", a.addresses[0]?.street === "Suite 2, 1 Made-up St" && a.addresses[0].city === "Sampletown" && a.addresses[0].postcode === "T0T 0T0" && a.addresses[0].label === "work", JSON.stringify(a.addresses));
check("a birthday with no year, and an anniversary", JSON.stringify(a.dates) === JSON.stringify([{ label: "birthday", value: "--05-31" }, { label: "anniversary", value: "2010-06-01" }]), JSON.stringify(a.dates));
check("related people", a.related[0]?.value === "Bob Example" && a.related[0].label === "Spouse", JSON.stringify(a.related));
check("handles", a.handles.some((h) => h.label === "skype" && h.value === "ann.example") && a.handles.some((h) => h.label === "linkedin"), JSON.stringify(a.handles));
check("escapes in notes", a.notes.startsWith("Met at the made-up fair, 2023.\nLikes tea."), a.notes);
check("pictures are left out", !JSON.stringify(a).includes("AAAA"));

const v21 = [
  "BEGIN:VCARD",
  "VERSION:2.1",
  "N;CHARSET=UTF-8;ENCODING=QUOTED-PRINTABLE:M=C3=BCller;J=C3=BCrgen;;;",
  "FN;CHARSET=UTF-8;ENCODING=QUOTED-PRINTABLE:J=C3=BCrgen M=C3=BC=",
  "ller",
  "TEL;CELL:0044 20 7946 0000",
  "TEL;HOME;VOICE:+44 20 7946 0001",
  "END:VCARD",
  "BEGIN:VCARD",
  "VERSION:4.0",
  "FN:Only Email",
  "EMAIL:only@example.com",
  "BDAY:--0315",
  "RELATED;TYPE=friend:Carol",
  "END:VCARD",
  "BEGIN:VCARD",
  "VERSION:3.0",
  "END:VCARD",
].join("\n");
const two = parseVcards(v21).cards;
check("2.1: quoted-printable and its soft line breaks", two[0]?.display === "Jürgen Müller" && two[0].name.family === "Müller", JSON.stringify(two[0]?.display));
check("2.1: bare types", two[0]?.phones[0]?.label === "mobile" && two[0].phones[1]?.label === "home", JSON.stringify(two[0]?.phones));
check("4.0: a date with no year", two[1]?.dates[0]?.value === "--03-15", JSON.stringify(two[1]?.dates));
check("4.0: related with a type", two[1]?.related[0]?.label === "friend");
check("an empty card is skipped", two.length === 2, String(two.length));

check("dates are read every usual way", normalDate("19900531") === "1990-05-31" && normalDate("1990-05-31T00:00:00Z") === "1990-05-31" && normalDate("31/05/1990") === null);
const folded = parseVcards("BEGIN:VCARD\r\nFN:Long\r\nNOTE:one\r\n  two\r\nEND:VCARD").cards[0];
check("folded lines join", folded.notes === "one two", folded.notes);
const many = Array.from({ length: 5003 }, (_, i) => `BEGIN:VCARD\nFN:Person ${i}\nEND:VCARD`).join("\n");
const capped = parseVcards(many);
check("a huge file stops at the limit and says so", capped.cards.length === 5000 && capped.tooMany);
check("a stray line outside a card is ignored", parseVcards("FN:Nobody\nBEGIN:VCARD\nFN:Somebody\nEND:VCARD").cards.map((c) => c.display).join() === "Somebody");
check("markup stays as text", parseVcards("BEGIN:VCARD\nFN:<img src=x onerror=alert(1)>\nEND:VCARD").cards[0].display === "<img src=x onerror=alert(1)>");

const passed = results.filter(Boolean).length;
console.log(`${passed} of ${results.length} passed`);
process.exit(passed === results.length ? 0 : 1);
