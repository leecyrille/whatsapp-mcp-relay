// Unit checks for src/lib/mentions.ts. Made-up names and addresses only.
//   node --experimental-strip-types e2e/mentions-check.mjs
import { activeQuery, findMentions, labelFor, matching, mentionAt, stillThere, toWire } from "../src/lib/mentions.ts";

const results = [];
function check(name, ok, detail = "") {
  results.push(ok);
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${!ok ? ` :: ${detail}` : ""}`);
}
const eq = (a, b) => JSON.stringify(a) === JSON.stringify(b);

check("@ at the start opens", eq(activeQuery("@", 1), { start: 0, query: "" }));
check("@ after a space opens with the query", eq(activeQuery("hi @bo", 6), { start: 3, query: "bo" }));
check("@ after punctuation opens", eq(activeQuery("(@bo", 4), { start: 1, query: "bo" }));
check("an email address doesn't", activeQuery("mail me a@b", 11) === null);
check("@ after an accented letter doesn't", activeQuery("café@x", 6) === null);
check("a space right after @ closes it", activeQuery("@ bo", 4) === null);
check("a new line closes it", activeQuery("@bo\nb", 5) === null);
check("spaces inside the query are fine", eq(activeQuery("@bob pl", 7), { start: 0, query: "bob pl" }));
check("a very long query closes it", activeQuery("@" + "x".repeat(41), 42) === null);
check("only the text before the caret counts", eq(activeQuery("@bob later", 4), { start: 0, query: "bob" }));

const people = [
  { jid: "1001@lid", person: "15550001001@s.whatsapp.net", name: "Bob Plumber" },
  { jid: "1002@lid", person: "1002@lid", name: "Robert Smith" },
  { jid: "15550001003@s.whatsapp.net", person: "15550001003@s.whatsapp.net", name: "Ann Bobbins" },
  { jid: "1004@lid", person: "1004@lid", name: "Zoë Example" },
  { jid: "1005@lid", person: "1005@lid", name: "Carol" },
];
const names = (q) => matching(people, q).map((p) => p.name);
check("empty query lists everyone, by name", eq(names(""), ["Ann Bobbins", "Bob Plumber", "Carol", "Robert Smith", "Zoë Example"]), names(""));
check("name start first, then word start", eq(names("bob"), ["Bob Plumber", "Ann Bobbins"]), names("bob"));
check("then anywhere in the name", eq(names("ert"), ["Robert Smith"]), names("ert"));
check("case doesn't matter", eq(names("PLUM"), ["Bob Plumber"]));
check("accents don't matter", eq(names("zoe"), ["Zoë Example"]));
check("a surname finds them", eq(names("smi"), ["Robert Smith"]));
check("a part of a number finds them", eq(names("1003"), ["Ann Bobbins"]), names("1003"));
check("nothing matches nothing", eq(names("xyz"), []));
const many = Array.from({ length: 1024 }, (_, i) => ({ jid: `${9000 + i}@lid`, person: `${9000 + i}@lid`, name: `Made-up Person ${i}` }));
const t = performance.now();
for (let i = 0; i < 100; i++) matching(many, "person 1");
check("1,024 people filter quickly", (performance.now() - t) / 100 < 5, `${((performance.now() - t) / 100).toFixed(2)} ms`);

const bob = { label: "Bob Plumber", jid: "1001@lid" };
const carol = { label: "Carol", jid: "1005@lid" };
check("a label is the name", labelFor("Bob Plumber", "1001@lid", []) === "Bob Plumber");
check("the same person keeps the same label", labelFor("Bob Plumber", "1001@lid", [bob]) === "Bob Plumber");
check("a different person with the same name gets (2)", labelFor("Bob Plumber", "2001@lid", [bob]) === "Bob Plumber (2)");
check("and a third gets (3)", labelFor("Bob Plumber", "3001@lid", [bob, { label: "Bob Plumber (2)", jid: "2001@lid" }]) === "Bob Plumber (3)");
check("@ and new lines are taken out of a label", labelFor("a@b\nc", "1@lid", []) === "abc");
check("an empty name still gets a label", labelFor("  ", "1@lid", []) === "Unknown");

check("text becomes digits and the list holds the address", eq(toWire("hi @Bob Plumber and @Carol!", [bob, carol]), { text: "hi @1001 and @1005!", mentions: ["1001@lid", "1005@lid"] }));
check("a phone address sends its number", eq(toWire("@Ann", [{ label: "Ann", jid: "15550001003@s.whatsapp.net" }]), { text: "@15550001003", mentions: ["15550001003@s.whatsapp.net"] }));
check("twice in the text, listed once", eq(toWire("@Carol @Carol", [carol]), { text: "@1005 @1005", mentions: ["1005@lid"] }));
check("a mention edited away isn't sent", eq(toWire("@Bob Plumbe", [bob]), { text: "@Bob Plumbe", mentions: [] }));
check("one followed by more letters isn't a mention", eq(toWire("@Carolyn", [carol]), { text: "@Carolyn", mentions: [] }));
check("one in an email address isn't a mention", eq(toWire("x@Carol", [carol]), { text: "x@Carol", mentions: [] }));
check("apostrophes and punctuation after are fine", eq(toWire("@Carol's, @Carol.", [carol]), { text: "@1005's, @1005.", mentions: ["1005@lid"] }));
const shortBob = { label: "Bob", jid: "7@lid" };
check("the longer name wins", eq(toWire("@Bob Plumber @Bob", [shortBob, bob]), { text: "@1001 @7", mentions: ["1001@lid", "7@lid"] }));
const twin = { label: "Bob Plumber (2)", jid: "2001@lid" };
check("same-name people stay apart", eq(toWire("@Bob Plumber @Bob Plumber (2)", [bob, twin]), { text: "@1001 @2001", mentions: ["1001@lid", "2001@lid"] }));
check("regex characters in a name are plain", eq(toWire("@A.B (x)", [{ label: "A.B (x)", jid: "5@lid" }]), { text: "@5", mentions: ["5@lid"] }));
check("no picks, nothing changes", eq(toWire("@Carol", []), { text: "@Carol", mentions: [] }));
check("edited-away picks are dropped from the draft", eq(stillThere("hi @Carol", [bob, carol]), [carol]));
check("emoji before the @ is fine", eq(toWire("👋@Carol", [carol]), { text: "👋@1005", mentions: ["1005@lid"] }));

const text = "hi @Carol there";
check("Backspace at the end of a mention takes all of it", eq(mentionAt(text, [carol], 9, "Backspace"), { start: 3, end: 9 }));
check("Backspace inside one takes all of it", eq(mentionAt(text, [carol], 6, "Backspace"), { start: 3, end: 9 }));
check("Backspace just before the @ leaves it", mentionAt(text, [carol], 3, "Backspace") === null);
check("Delete at the @ takes all of it", eq(mentionAt(text, [carol], 3, "Delete"), { start: 3, end: 9 }));
check("Delete at the end leaves it", mentionAt(text, [carol], 9, "Delete") === null);
check("findMentions gives the positions", eq(findMentions(text, [carol]).map((m) => [m.start, m.end]), [[3, 9]]));

const passed = results.filter(Boolean).length;
console.log(`${passed} of ${results.length} passed`);
process.exit(passed === results.length ? 0 : 1);
