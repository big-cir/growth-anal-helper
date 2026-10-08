import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { formatFinding, makeRules, runPublicCheck, scanText } from '../src/public-check.ts';

// 검사에 걸릴 문자열은 조각을 이어 만든다(이 파일 자체가 걸리지 않게).
const j = (...parts: string[]) => parts.join('');
const SAMPLES: Record<string, string> = {
  'G1 개인 키': j('-----BEGIN ', 'RSA PRIVATE', ' KEY-----'),
  'G2 클라우드 키': j('key = "AK', 'IAABCDEFGHIJKLMNOP"'),
  'G3 클라우드 호스트': j('host: db1.abc.ap-east-1.', 'rds.amazon', 'aws.com'),
  'G4 IPv4': j('connect 10.', '1.2.3'),
  'G5 홈 절대 경로': j('open("/', 'Users/alice/x")'),
  'G6 DB 접속 문자열': j('mysql:', '//u:p@h/db'),
  'G7 고엔트로피 토큰': j('t = "aZ3kQ9pL2mX8vB4nR7', 'sT1yU6wE5oI0hG3jF2dK"'),
};

test('범용 규칙: 각 표본이 해당 규칙에 걸린다', () => {
  const rules = makeRules(null);
  for (const [rule, text] of Object.entries(SAMPLES)) {
    const hits = scanText('f', text, rules).map((f) => f.rule);
    assert.ok(hits.includes(rule), `${rule}: ${hits.join(',')}`);
  }
});

test('범용 규칙: 흔한 정상 코드는 걸리지 않는다', () => {
  const ok = [
    "server.listen(4170, '127.0.0.1')",
    'version 1.2.3',
    'const id = 0.0.0.0',
    j('예시 주소 192.', '0.2.10'),
    'sha256 = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"',
    'const veryLongIdentifierNameWithoutDigitsInsideIt = 1',
    'path = "./home/readme"',
    j('url = "mysql:', '//localhost/db"'),
  ];
  const rules = makeRules(null);
  for (const line of ok) assert.deepEqual(scanText('f', line, rules), [], line);
});

test('금지어: 대소문자 무시 부분 문자열, 메시지에 원문 없음', () => {
  const rules = makeRules(['acme-board']);
  const hits = scanText('src/a.ts', 'x\nconst n = "ACME-Board";\n', rules);
  assert.deepEqual(hits, [{ where: 'src/a.ts', line: 2, rule: 'D1 금지어' }]);
  assert.ok(!formatFinding(hits[0]).toLowerCase().includes('acme'));
});

function repoWith(files: Record<string, string>): string {
  const repo = mkdtempSync(join(tmpdir(), 'gl-pc-'));
  const g = (...a: string[]) => execFileSync('git', a, { cwd: repo, stdio: 'ignore' });
  g('init', '-q');
  g('config', 'user.email', 'test@example.com');
  g('config', 'user.name', 'test');
  for (const [p, c] of Object.entries(files)) writeFileSync(join(repo, p), c);
  return repo;
}

test('기본 모드: 추적·미추적 파일, 경로 이름, 목록 없음 경고, --require-denylist', () => {
  const repo = repoWith({ 'a.txt': 'clean\n', '.gitignore': 'ws/\n' });
  const deny = join(repo, 'ws', 'public-denylist.txt');
  let r = runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: false });
  assert.ok(r.ok);
  assert.equal(r.warnings.length, 1);
  r = runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: true });
  assert.ok(!r.ok);

  execFileSync('mkdir', ['-p', join(repo, 'ws')]);
  writeFileSync(deny, '# 주석\nAcmeBoard\n\n');
  writeFileSync(join(repo, 'b.txt'), 'see acmeboard here\n');
  writeFileSync(join(repo, 'acmeboard-notes.md'), 'x\n');
  r = runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: true });
  assert.ok(!r.ok);
  const where = r.findings.map((f) => f.where).sort();
  assert.deepEqual(where, ['(경로) acmeboard-notes.md', 'b.txt']);
});

test('스테이징 내용: 작업 트리에서 고쳐도 스테이징된 내용이 걸린다', () => {
  const repo = repoWith({ 'a.txt': 'leak acmeboard\n' });
  execFileSync('git', ['add', 'a.txt'], { cwd: repo });
  writeFileSync(join(repo, 'a.txt'), 'clean\n');
  const deny = join(mkdtempSync(join(tmpdir(), 'gl-deny-')), 'deny.txt');
  writeFileSync(deny, 'acmeboard\n');
  const r = runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: true });
  assert.deepEqual(r.findings.map((f) => f.where), ['(스테이징) a.txt']);
});

test('--history: 지운 파일의 이력과 커밋 메시지까지 검사', () => {
  const repo = repoWith({ 'a.txt': 'old acmeboard\n' });
  const g = (...a: string[]) => execFileSync('git', a, { cwd: repo, stdio: 'ignore' });
  g('add', 'a.txt');
  g('commit', '-q', '-m', 'first');
  writeFileSync(join(repo, 'a.txt'), 'clean\n');
  g('commit', '-q', '-am', 'fix AcmeBoard mention');
  const deny = join(tmpdir(), `gl-deny-${process.pid}.txt`);
  writeFileSync(deny, 'acmeboard\n');
  assert.ok(runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: true }).ok);
  const r = runPublicCheck({ repo, denylistFile: deny, history: true, requireDenylist: true });
  const where = r.findings.map((f) => f.where.replace(/ [0-9a-f]{10}$/, ' <sha>'));
  assert.ok(where.includes('(이력) a.txt'), where.join('|'));
  assert.ok(where.includes('(커밋 메시지) <sha>'), where.join('|'));
});

test('바이너리도 그 안의 문자열 조각을 검사한다', () => {
  const hits = scanText('img.bin', j('\x89PNG\0\0', 'xx acme', 'board yy', '\0\x01'), makeRules(['acmeboard']));
  assert.deepEqual(hits, [{ where: 'img.bin (바이너리)', line: null, rule: 'D1 금지어' }]);
});

test('금지어 목록이 비었거나 주석뿐이면: 훅에서는 실패, 아니면 경고', () => {
  const repo = repoWith({ 'a.txt': 'x\n' });
  const deny = join(mkdtempSync(join(tmpdir(), 'gl-deny-')), 'deny.txt');
  writeFileSync(deny, '# 주석만\n\n');
  assert.ok(!runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: true }).ok);
  const r = runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: false });
  assert.ok(r.ok);
  assert.equal(r.warnings.length, 1);
});

test('--history: 같은 내용으로 이름만 바꾼 파일의 옛 경로 이름도 검사', () => {
  const repo = repoWith({ 'acmeboard-plan.txt': 'same\n' });
  const g = (...a: string[]) => execFileSync('git', a, { cwd: repo, stdio: 'ignore' });
  g('add', '.');
  g('commit', '-q', '-m', 'a');
  g('mv', 'acmeboard-plan.txt', 'plan.txt');
  g('commit', '-q', '-m', 'b');
  const deny = join(mkdtempSync(join(tmpdir(), 'gl-deny-')), 'deny.txt');
  writeFileSync(deny, 'acmeboard\n');
  const r = runPublicCheck({ repo, denylistFile: deny, history: true, requireDenylist: true });
  assert.ok(r.findings.some((f) => f.where === '(경로) acmeboard-plan.txt'), JSON.stringify(r.findings));
});
