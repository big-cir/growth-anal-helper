// SQL 정적 검사.

/** 규칙이 바뀌면 올린다 */
export const LINT_RULES_VERSION = 1;

export type LintResult = { ok: true; params: string[] } | { ok: false; message: string };

type Tok = { kind: 'word' | 'param' | 'semi' | 'other'; text: string };

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
      if (e < 0) return '닫히지 않은 주석';
      i = e + 2;
      continue;
    }
    if (c === "'" || c === '"' || c === '`') {
      let j = i + 1;
      for (;;) {
        if (j >= sql.length) return c === "'" ? '닫히지 않은 문자열' : '닫히지 않은 식별자';
        if (sql[j] === c) {
          if (sql[j + 1] === c) { j += 2; continue; }
          break;
        }
        j++;
      }
      toks.push({ kind: c === "'" ? 'other' : 'word', text: c === "'" ? "'" : sql.slice(i + 1, j) });
      i = j + 1;
      continue;
    }
    if (c === '[') {
      const e = sql.indexOf(']', i);
      if (e < 0) return '닫히지 않은 식별자';
      toks.push({ kind: 'word', text: sql.slice(i + 1, e) });
      i = e + 1;
      continue;
    }
    if (c === ':' || c === '@' || c === '$' || c === '?') {
      const m = /^[:@$?][A-Za-z0-9_]*/.exec(sql.slice(i))!;
      toks.push({ kind: 'param', text: m[0] });
      i += m[0].length;
      continue;
    }
    if (c === ';') { toks.push({ kind: 'semi', text: ';' }); i++; continue; }
    const w = /^[A-Za-z_][A-Za-z0-9_]*/.exec(sql.slice(i));
    if (w) { toks.push({ kind: 'word', text: w[0] }); i += w[0].length; continue; }
    toks.push({ kind: 'other', text: c });
    i++;
  }
  return toks;
}

/** 문장 하나, SELECT/WITH(재귀 제외), 매개변수는 :as_of와 허용 키만 */
export function lintSql(sql: string, allowedParams: Iterable<string>): LintResult {
  if (sql.length > 64 * 1024) return { ok: false, message: 'SQL이 너무 김(64KB 상한)' };
  const toks = tokenize(sql);
  if (typeof toks === 'string') return { ok: false, message: toks };
  const semis = toks.map((t, i) => (t.kind === 'semi' ? i : -1)).filter((i) => i >= 0);
  if (semis.length > 1 || (semis.length === 1 && semis[0] !== toks.length - 1)) return { ok: false, message: '문장은 하나만 (`;`는 끝에만)' };
  const body = semis.length ? toks.slice(0, -1) : toks;
  if (body.length === 0) return { ok: false, message: '빈 SQL' };
  const first = body[0].kind === 'word' ? body[0].text.toUpperCase() : '';
  if (first !== 'SELECT' && first !== 'WITH') return { ok: false, message: 'SELECT 또는 WITH로 시작해야 함' };
  if (first === 'WITH' && body[1]?.kind === 'word' && body[1].text.toUpperCase() === 'RECURSIVE') {
    return { ok: false, message: 'WITH RECURSIVE는 쓸 수 없음 (주 단위 달력은 d_calendar_week 사용)' };
  }
  const allowed = new Set(['as_of', ...allowedParams]);
  const used = new Set<string>();
  for (const t of body) {
    if (t.kind !== 'param') continue;
    const name = t.text.slice(1);
    if (t.text[0] !== ':' || !/^[A-Za-z_]/.test(name)) return { ok: false, message: `매개변수는 :이름 형식만 (${t.text})` };
    if (!allowed.has(name)) return { ok: false, message: `허용되지 않은 매개변수: ${t.text} (쓸 수 있는 것: ${[...allowed].map((p) => `:${p}`).join(', ')})` };
    used.add(name);
  }
  return { ok: true, params: [...used] };
}
