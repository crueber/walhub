// web/src/lib/highlight.js — §2.2: no shiki; a cheap hand-rolled tokenizer for the
// common cases (keywords / strings / comments / numbers) driven by a
// filename-extension → language table; unknown extensions render plain.
// Pure string functions — importable in Node.

const LANG_FOR_EXT = {
  js: "js", mjs: "js", cjs: "js", jsx: "js", ts: "js", tsx: "js",
  go: "go", py: "python", rb: "ruby", rs: "rust", sh: "shell", bash: "shell", zsh: "shell",
  json: "json", yml: "yaml", yaml: "yaml", toml: "toml", ini: "toml", conf: "toml",
  c: "c", h: "c", cpp: "c", cc: "c", hpp: "c", hxx: "c", cs: "c", java: "c", kt: "c", swift: "c",
  css: "css", html: "html", xml: "html", svg: "html", sql: "sql",
};

/** languageFor("main.go") → "go"; unknown → null (render plain). */
export function languageFor(filename) {
  const m = /\.([a-z0-9]+)$/i.exec(String(filename ?? ""));
  return m ? (LANG_FOR_EXT[m[1].toLowerCase()] ?? null) : null;
}

function esc(s) {
  return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

const KEYWORDS = {
  js: "abstract async await break case catch class const continue debugger default delete do else enum export extends false finally for function get if implements import in instanceof interface let new null of package private protected public return set static super switch this throw true try typeof undefined var void while with yield",
  go: "break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var nil true false make new",
  python: "and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield None True False",
  rust: "as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while",
  ruby: "begin break case class def do else elsif end ensure for if module next nil redo rescue retry return self super then undef unless until when while yield true false",
  c: "auto break case char class const continue default delete do double else enum extern false float for goto if inline int interface long namespace new NULL override private protected public restrict return short signed sizeof static struct switch template this true typedef union unsigned using virtual void volatile while",
  shell: "break case continue do done elif else esac fi for function if in return select then until while local export readonly shift unset set echo cd",
  json: "true false null",
  yaml: "true false null yes no on off",
  toml: "true false",
  sql: "select from where insert into update delete values create table drop alter join left right inner outer on group by order limit as and or not null primary key index",
};

const STRING_CHARS = {
  default: ["'", '"', "`"],
  c: ["'", '"'], rust: ["'", '"'], python: ["'", '"'], shell: ["'", '"'],
  sql: ["'"], json: ['"'], yaml: ["'", '"'], toml: ["'", '"'],
};
const LINE_COMMENTS = {
  default: ["//"], python: ["#"], shell: ["#"], ruby: ["#"], yaml: ["#"], toml: ["#"], sql: ["--"],
};
const BLOCK_COMMENTS = { default: [["/*", "*/"]], c: [["/*", "*/"]] };
const RULES = ["js", "go", "python", "rust", "ruby", "c", "shell", "json", "yaml", "toml", "sql"];

function escRx(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

const REGEX_CACHE = new Map();
const KW_CACHE = new Map();

function kwSet(lang) {
  let s = KW_CACHE.get(lang);
  if (!s) KW_CACHE.set(lang, (s = new Set((KEYWORDS[lang] ?? "").split(" ").filter(Boolean))));
  return s;
}

function buildRegex(lang) {
  const pick = (table) => table[RULES.includes(lang) ? lang : "default"] ?? table.default;
  const parts = [];
  for (const [open, close] of pick(BLOCK_COMMENTS)) parts.push(`${escRx(open)}[\\s\\S]*?${escRx(close)}`);
  for (const lc of pick(LINE_COMMENTS)) parts.push(`${escRx(lc)}[^\\n]*`);
  for (const ch of pick(STRING_CHARS)) {
    const safe = ch === "]" || ch === "\\" || ch === "^" || ch === "-" ? `\\${ch}` : ch;
    parts.push(`${escRx(ch)}(?:\\\\.|[^\\\\${safe}\\n])*${escRx(ch)}`);
  }
  parts.push("\\b\\d[\\w.]*\\b");
  if ((KEYWORDS[lang] ?? "") !== "") parts.push("\\b[A-Za-z_$][\\w$]*\\b");
  return new RegExp(parts.join("|"), "g");
}

/**
 * highlight(code, lang) → HTML with <span class="tok-kw|tok-str|tok-com|tok-num">
 * around tinted runs. Unknown language → escaped plain text.
 */
export function highlight(code, lang) {
  code = String(code ?? "");
  if (!lang || !RULES.includes(lang)) return esc(code);
  let rx = REGEX_CACHE.get(lang);
  if (!rx) REGEX_CACHE.set(lang, (rx = buildRegex(lang)));
  const kw = kwSet(lang);
  let out = "";
  let last = 0;
  for (const m of code.matchAll(rx)) {
    out += esc(code.slice(last, m.index));
    last = m.index + m[0].length;
    const text = m[0];
    let cls = "";
    if (/^\d/.test(text)) cls = "tok-num";
    else if (text[0] === '"' || text[0] === "'" || text[0] === "`") cls = "tok-str";
    else if (text.startsWith("//") || text.startsWith("#") || text.startsWith("--") || text.startsWith("/*")) cls = "tok-com";
    else if (kw.has(text)) cls = "tok-kw";
    out += cls ? `<span class="${cls}">${esc(text)}</span>` : esc(text);
  }
  out += esc(code.slice(last));
  return out;
}