// Static SQL checks.

/** Bump when the rules change */
export const LINT_RULES_VERSION = 1;

export type LintResult = { ok: true; params: string[] } | { ok: false; message: string };

type Tok = { kind: 'word' | 'param' | 'semi' | 'other'; text: string; start: number; end: number };

function tokenize(sql: string): Tok[] | string {
  const toks: Tok[] = [];
  let i = 0;
  while (i < sql.length) {
    const c = sql[i];
    const n = sql[i + 1];
    if (/\s/.test(c)) { i++; continue; }
    if (c === '-' && n === '-') { const e = sql.indexOf('\n', i); i = e < 0 ? sql.length : e + 1; continue; }
    if (c === '/' && n === '*') {
      const e = sql.indexOf('*/', i + 2);
      if (e < 0) return 'unterminated comment';
      i = e + 2;
      continue;
    }
    if (c === "'" || c === '"' || c === '`') {
      let j = i + 1;
      for (;;) {
        if (j >= sql.length) return c === "'" ? 'unterminated string' : 'unterminated identifier';
        if (sql[j] === c) {
          if (sql[j + 1] === c) { j += 2; continue; }
          break;
        }
        j++;
      }
      toks.push({ kind: c === "'" ? 'other' : 'word', text: c === "'" ? "'" : sql.slice(i + 1, j), start: i, end: j + 1 });
      i = j + 1;
      continue;
    }
    if (c === '[') {
      const e = sql.indexOf(']', i);
      if (e < 0) return 'unterminated identifier';
      toks.push({ kind: 'word', text: sql.slice(i + 1, e), start: i, end: e + 1 });
      i = e + 1;
      continue;
    }
    if (c === ':' || c === '@' || c === '$' || c === '?') {
      const m = /^[:@$?][A-Za-z0-9_]*/.exec(sql.slice(i))!;
      toks.push({ kind: 'param', text: m[0], start: i, end: i + m[0].length });
      i += m[0].length;
      continue;
    }
    if (c === ';') { toks.push({ kind: 'semi', text: ';', start: i, end: i + 1 }); i++; continue; }
    const w = /^[A-Za-z_][A-Za-z0-9_]*/.exec(sql.slice(i));
    if (w) { toks.push({ kind: 'word', text: w[0], start: i, end: i + w[0].length }); i += w[0].length; continue; }
    toks.push({ kind: 'other', text: c, start: i, end: i + 1 });
    i++;
  }
  return toks;
}

/** One statement, SELECT/WITH (not recursive), parameters only :as_of and allowed keys */
export function lintSql(sql: string, allowedParams: Iterable<string>): LintResult {
  if (sql.length > 64 * 1024) return { ok: false, message: 'SQL too long (64KB limit)' };
  const toks = tokenize(sql);
  if (typeof toks === 'string') return { ok: false, message: toks };
  const semis = toks.map((t, i) => (t.kind === 'semi' ? i : -1)).filter((i) => i >= 0);
  if (semis.length > 1 || (semis.length === 1 && semis[0] !== toks.length - 1)) return { ok: false, message: 'one statement only (`;` only at the end)' };
  const body = semis.length ? toks.slice(0, -1) : toks;
  if (body.length === 0) return { ok: false, message: 'empty SQL' };
  const first = body[0].kind === 'word' ? body[0].text.toUpperCase() : '';
  if (first !== 'SELECT' && first !== 'WITH') return { ok: false, message: 'must start with SELECT or WITH' };
  if (first === 'WITH' && body[1]?.kind === 'word' && body[1].text.toUpperCase() === 'RECURSIVE') {
    return { ok: false, message: 'WITH RECURSIVE is not allowed (use d_calendar_week for a weekly calendar)' };
  }
  const allowed = new Set(['as_of', ...allowedParams]);
  const used = new Set<string>();
  for (const t of body) {
    if (t.kind !== 'param') continue;
    const name = t.text.slice(1);
    if (t.text[0] !== ':' || !/^[A-Za-z_]/.test(name)) return { ok: false, message: `parameters must be :name (${t.text})` };
    if (!allowed.has(name)) return { ok: false, message: `parameter not allowed: ${t.text} (allowed: ${[...allowed].map((p) => `:${p}`).join(', ')})` };
    used.add(name);
  }
  return { ok: true, params: [...used] };
}

/** Candidate WITH names (lowercase): every `name [(cols…)] AS [NOT] [MATERIALIZED] (` match, ignoring scope */
export function cteNames(sql: string): Set<string> {
  const toks = tokenize(sql);
  const out = new Set<string>();
  if (typeof toks === 'string') return out;
  const isWord = (i: number, w: string) => toks[i]?.kind === 'word' && toks[i].text.toUpperCase() === w;
  for (let i = 0; i < toks.length; i++) {
    if (toks[i].kind !== 'word') continue;
    let j = i + 1;
    if (toks[j]?.text === '(') {
      let depth = 0;
      for (; j < toks.length; j++) {
        if (toks[j].text === '(') depth++;
        else if (toks[j].text === ')' && --depth === 0) break;
      }
      j++;
    }
    if (!isWord(j, 'AS')) continue;
    j++;
    if (isWord(j, 'NOT')) j++;
    if (isWord(j, 'MATERIALIZED')) j++;
    if (toks[j]?.text === '(') out.add(toks[i].text.toLowerCase());
  }
  return out;
}

/** Words blocked in source cross-check SQL: writes (MySQL allows WITH … UPDATE/DELETE), file writes, locks, delays, external reads */
const SOURCE_DENY = new Set(['UPDATE', 'DELETE', 'INTO', 'OUTFILE', 'DUMPFILE', 'LOCK', 'LOAD_FILE', 'SLEEP', 'BENCHMARK', 'GET_LOCK', 'HANDLER', 'CALL']);

/**
 * Blocks syntax where the source dialect and this tokenizer could disagree on statement boundaries:
 * all comments (`/*!` executable comments, `--` without a space, `#`), backslash escapes in strings, brackets outside quotes
 */
function sourceSyntaxProblem(sql: string): string | null {
  if (sql.includes('\\')) return 'backslashes are not allowed';
  let q: string | null = null;
  for (let i = 0; i < sql.length; i++) {
    const c = sql[i];
    if (q) {
      if (c === q) q = sql[i + 1] === q ? (i++, q) : null;
      continue;
    }
    if (c === "'" || c === '"' || c === '`') q = c;
    else if (c === '#' || (c === '-' && sql[i + 1] === '-') || (c === '/' && sql[i + 1] === '*')) return 'comments are not allowed';
    // In MySQL, [ ] are not quotes (this tokenizer treats them as identifiers)
    else if (c === '[' || c === ']') return 'brackets are not allowed';
    // This tokenizer does not understand PostgreSQL $$ strings
    else if (c === '$') return '$ is not allowed';
  }
  return null;
}

/**
 * Cross-check SQL for the source DB: checks the syntax above, lintSql rules, blocked words and `FOR SHARE`,
 * then replaces parameters with string literals of engine values (values never contain quotes)
 */
export function bindSourceSql(sql: string, values: Record<string, string>): { ok: true; sql: string } | { ok: false; message: string } {
  const syntax = sourceSyntaxProblem(sql);
  if (syntax) return { ok: false, message: syntax };
  const lint = lintSql(sql, Object.keys(values));
  if (!lint.ok) return lint;
  const toks = tokenize(sql) as Tok[];
  for (let i = 0; i < toks.length; i++) {
    const t = toks[i];
    if (t.kind !== 'word' || sql[t.start] === '"' || sql[t.start] === '`' || sql[t.start] === '[') continue;
    const w = t.text.toUpperCase();
    if (SOURCE_DENY.has(w)) return { ok: false, message: `word not allowed in source cross-check SQL: ${t.text}` };
    const next = toks[i + 1];
    if (w === 'FOR' && next?.kind === 'word' && /^SHARE$/i.test(next.text)) return { ok: false, message: `word not allowed in source cross-check SQL: FOR ${next.text}` };
  }
  let out = '';
  let at = 0;
  for (const t of toks) {
    if (t.kind !== 'param') continue;
    const v = values[t.text.slice(1)];
    if (!/^[0-9A-Za-z :._-]*$/.test(v)) return { ok: false, message: `invalid parameter value: ${t.text}` };
    out += `${sql.slice(at, t.start)}'${v}'`;
    at = t.end;
  }
  out += sql.slice(at);
  return { ok: true, sql: out.replace(/;\s*$/, '') };
}
