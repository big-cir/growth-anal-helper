// 공개 저장소에 들어가면 안 되는 내용 검사: 범용 규칙 + 워크스페이스 금지어 목록(public-denylist.txt).
// 결과에는 걸린 원문을 찍지 않는다.
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

export type Finding = { where: string; line: number | null; rule: string };

type Rule = { id: string; test: (line: string) => boolean };

const GENERIC_RULES: Rule[] = [
  { id: 'G1 개인 키', test: (l) => /-{5}BEGIN [A-Z ]*PRIVATE KEY-{5}/.test(l) },
  { id: 'G2 클라우드 키', test: (l) => /\b(AKIA|ASIA)[0-9A-Z]{16}\b/.test(l) || /\bgh[pousr]_[A-Za-z0-9]{36,}\b/.test(l) || /\bsk-[A-Za-z0-9_-]{20,}\b/.test(l) },
  { id: 'G3 클라우드 호스트', test: (l) => /[A-Za-z0-9-]+\.(amazonaws|rds)\.com\b/i.test(l) || /\.rds\.[a-z]/i.test(l) || /\.(compute|ec2)\.internal\b/i.test(l) },
  { id: 'G4 IPv4', test: (l) => findIpv4(l) },
  { id: 'G5 홈 절대 경로', test: (l) => /(^|[^A-Za-z0-9_.~])\/(Users|home)\/[^/\s'"`]+/.test(l) },
  { id: 'G6 DB 접속 문자열', test: (l) => /\b(mysql|postgres(ql)?|mongodb(\+srv)?|redis)[:]\/\/[^\s'"`]+@/i.test(l) || /\bjdbc[:][a-z]/i.test(l) },
  { id: 'G7 고엔트로피 토큰', test: (l) => findHighEntropy(l) },
];

function findIpv4(line: string): boolean {
  const re = /(?<![\d.])(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})(?![\d.])/g;
  for (const m of line.matchAll(re)) {
    const o = m.slice(1, 5).map(Number);
    if (o.some((x) => x > 255)) continue;
    const [a, b, c] = o;
    if (a === 127 || (a === 0 && b === 0 && c === 0)) continue;
    if ((a === 192 && b === 0 && c === 2) || (a === 198 && b === 51 && c === 100) || (a === 203 && b === 0 && c === 113)) continue;
    return true;
  }
  return false;
}

function entropy(s: string): number {
  const counts = new Map<string, number>();
  for (const ch of s) counts.set(ch, (counts.get(ch) ?? 0) + 1);
  let h = 0;
  for (const n of counts.values()) {
    const p = n / s.length;
    h -= p * Math.log2(p);
  }
  return h;
}

function findHighEntropy(line: string): boolean {
  for (const m of line.matchAll(/[A-Za-z0-9+/_=-]{32,}/g)) {
    const t = m[0];
    if (!/[a-z]/.test(t) || !/[A-Z]/.test(t) || !/[0-9]/.test(t)) continue;
    if (entropy(t) >= 4.5) return true;
  }
  return false;
}

export function loadDenylist(file: string): string[] {
  return readFileSync(file, 'utf8')
    .split(/\r?\n/)
    .map((l) => l.trim())
    .filter((l) => l !== '' && !l.startsWith('#'))
    .map((l) => l.toLowerCase());
}

export function makeRules(denylist: string[] | null): Rule[] {
  const rules = [...GENERIC_RULES];
  denylist?.forEach((word, i) => rules.push({ id: `D${i + 1} 금지어`, test: (l) => l.toLowerCase().includes(word) }));
  return rules;
}

/** 줄 단위 검사. 바이너리는 제어 문자를 줄바꿈으로 바꿔 검사한다 */
export function scanText(where: string, text: string, rules: Rule[]): Finding[] {
  const binary = text.includes('\0');
  const body = binary ? text.replace(/[\x00-\x08\x0b-\x1f\x7f]+/g, '\n') : text;
  const label = binary ? `${where} (바이너리)` : where;
  const out: Finding[] = [];
  body.split('\n').forEach((line, i) => {
    for (const r of rules) if (r.test(line)) out.push({ where: label, line: binary ? null : i + 1, rule: r.id });
  });
  return out;
}

export function scanPath(path: string, rules: Rule[]): Finding[] {
  return rules.filter((r) => r.test(path)).map((r) => ({ where: `(경로) ${path}`, line: null, rule: r.id }));
}

function git(cwd: string, args: string[], input?: string): Buffer {
  return execFileSync('git', args, { cwd, input, maxBuffer: 1 << 30, stdio: ['pipe', 'pipe', 'pipe'] });
}

function nulList(buf: Buffer): string[] {
  return buf.toString('utf8').split('\0').filter(Boolean);
}

/** 추적·미추적 파일 내용, 스테이징된 내용, 경로 이름 */
export function scanWorkingSet(repo: string, rules: Rule[]): Finding[] {
  const findings: Finding[] = [];
  const files = new Set([
    ...nulList(git(repo, ['ls-files', '-z'])),
    ...nulList(git(repo, ['ls-files', '-z', '--others', '--exclude-standard'])),
  ]);
  for (const f of files) {
    findings.push(...scanPath(f, rules));
    const abs = join(repo, f);
    if (existsSync(abs)) findings.push(...scanText(f, readFileSync(abs).toString('utf8'), rules));
  }
  for (const f of nulList(git(repo, ['diff', '--cached', '--name-only', '-z', '--diff-filter=ACMR']))) {
    findings.push(...scanPath(f, rules));
    findings.push(...scanText(`(스테이징) ${f}`, git(repo, ['show', `:${f}`]).toString('utf8'), rules));
  }
  return findings;
}

/** 모든 커밋 메시지와 blob 내용·경로 */
export function scanHistory(repo: string, rules: Rule[]): Finding[] {
  const findings: Finding[] = [];
  const commits = git(repo, ['rev-list', '--all']).toString('utf8').split('\n').filter(Boolean);
  if (commits.length === 0) return findings;

  for (const sha of commits) {
    const msg = git(repo, ['log', '-1', '--format=%B', sha]).toString('utf8');
    findings.push(...scanText(`(커밋 메시지) ${sha.slice(0, 10)}`, msg, rules));
  }

  const allPaths = new Set(git(repo, ['log', '--all', '--format=', '--name-only', '-z', '--no-renames']).toString('utf8').split('\0').map((p) => p.trim()).filter(Boolean));
  for (const p of allPaths) findings.push(...scanPath(p, rules));

  const objects = git(repo, ['rev-list', '--objects', '--all']).toString('utf8').split('\n').filter(Boolean);
  const pathOf = new Map<string, string>();
  for (const line of objects) {
    const sp = line.indexOf(' ');
    const sha = sp < 0 ? line : line.slice(0, sp);
    const p = sp < 0 ? '' : line.slice(sp + 1);
    if (!pathOf.has(sha) || (p && !pathOf.get(sha))) pathOf.set(sha, p);
  }
  const out = git(repo, ['cat-file', '--batch'], [...pathOf.keys()].join('\n') + '\n');
  let pos = 0;
  while (pos < out.length) {
    const nl = out.indexOf(0x0a, pos);
    const [sha, type, sizeStr] = out.subarray(pos, nl).toString('utf8').split(' ');
    const size = Number(sizeStr);
    const body = out.subarray(nl + 1, nl + 1 + size);
    pos = nl + 1 + size + 1;
    if (type === 'blob') findings.push(...scanText(`(이력) ${pathOf.get(sha) || sha.slice(0, 10)}`, body.toString('utf8'), rules));
  }
  return dedupe(findings);
}

function dedupe(fs: Finding[]): Finding[] {
  const seen = new Set<string>();
  return fs.filter((f) => {
    const k = `${f.where}\0${f.line}\0${f.rule}`;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
}

export type PublicCheckOptions = { repo: string; denylistFile: string; history: boolean; requireDenylist: boolean };
export type PublicCheckResult = { findings: Finding[]; warnings: string[]; ok: boolean };

export function runPublicCheck(o: PublicCheckOptions): PublicCheckResult {
  const warnings: string[] = [];
  let denylist: string[] | null = null;
  if (existsSync(o.denylistFile)) {
    denylist = loadDenylist(o.denylistFile);
    if (denylist.length === 0) {
      if (o.requireDenylist) return { findings: [], warnings: ['금지어 목록이 비어 있음 (훅에서는 항목이 하나 이상 필요)'], ok: false };
      warnings.push('금지어 목록이 비어 있어 금지어 검사를 하지 않음');
      denylist = null;
    }
  } else if (o.requireDenylist) {
    return { findings: [], warnings: ['금지어 목록이 없음 (훅에서는 필수): 워크스페이스의 public-denylist.txt'], ok: false };
  } else {
    warnings.push('금지어 목록이 없어 금지어 검사를 하지 않음 (범용 검사만 함)');
  }
  const rules = makeRules(denylist);
  const findings = dedupe(o.history ? [...scanWorkingSet(o.repo, rules), ...scanHistory(o.repo, rules)] : scanWorkingSet(o.repo, rules));
  return { findings, warnings, ok: findings.length === 0 };
}

export function formatFinding(f: Finding): string {
  return `${f.where}${f.line === null ? '' : `:${f.line}`}  [${f.rule}]`;
}
