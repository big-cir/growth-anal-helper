import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { formatFinding, makeRules, runPublicCheck, scanText } from '../src/public-check.ts';

// Strings that should be caught are built from pieces (so this file itself is not caught).
const j = (...parts: string[]) => parts.join('');
const SAMPLES: Record<string, string> = {
  'G1 private key': j('-----BEGIN ', 'RSA PRIVATE', ' KEY-----'),
  'G2 cloud key': j('key = "AK', 'IAABCDEFGHIJKLMNOP"'),
  'G3 cloud host': j('host: db1.abc.ap-east-1.', 'rds.amazon', 'aws.com'),
  'G4 IPv4': j('connect 10.', '1.2.3'),
  'G5 absolute home path': j('open("/', 'Users/alice/x")'),
  'G6 DB connection string': j('mysql:', '//u:p@h/db'),
  'G7 high-entropy token': j('t = "aZ3kQ9pL2mX8vB4nR7', 'sT1yU6wE5oI0hG3jF2dK"'),
};

test('generic rules: each sample hits its rule', () => {
  const rules = makeRules(null);
  for (const [rule, text] of Object.entries(SAMPLES)) {
    const hits = scanText('f', text, rules).map((f) => f.rule);
    assert.ok(hits.includes(rule), `${rule}: ${hits.join(',')}`);
  }
});

test('generic rules: common normal code is not caught', () => {
  const ok = [
    "server.listen(4170, '127.0.0.1')",
    'version 1.2.3',
    'const id = 0.0.0.0',
    j('example address 192.', '0.2.10'),
    'sha256 = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"',
    'const veryLongIdentifierNameWithoutDigitsInsideIt = 1',
    'path = "./home/readme"',
    j('url = "mysql:', '//localhost/db"'),
  ];
  const rules = makeRules(null);
  for (const line of ok) assert.deepEqual(scanText('f', line, rules), [], line);
});

test('denylist: case-insensitive substring, no matched text in the message', () => {
  const rules = makeRules(['acme-board']);
  const hits = scanText('src/a.ts', 'x\nconst n = "ACME-Board";\n', rules);
  assert.deepEqual(hits, [{ where: 'src/a.ts', line: 2, rule: 'D1 denylisted word' }]);
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

test('default mode: tracked and untracked files, path names, missing-list warning, --require-denylist', () => {
  const repo = repoWith({ 'a.txt': 'clean\n', '.gitignore': 'ws/\n' });
  const deny = join(repo, 'ws', 'public-denylist.txt');
  let r = runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: false });
  assert.ok(r.ok);
  assert.equal(r.warnings.length, 1);
  r = runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: true });
  assert.ok(!r.ok);

  execFileSync('mkdir', ['-p', join(repo, 'ws')]);
  writeFileSync(deny, '# comment\nAcmeBoard\n\n');
  writeFileSync(join(repo, 'b.txt'), 'see acmeboard here\n');
  writeFileSync(join(repo, 'acmeboard-notes.md'), 'x\n');
  r = runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: true });
  assert.ok(!r.ok);
  const where = r.findings.map((f) => f.where).sort();
  assert.deepEqual(where, ['(path) acmeboard-notes.md', 'b.txt']);
});

test('staged content: caught even if fixed in the working tree', () => {
  const repo = repoWith({ 'a.txt': 'leak acmeboard\n' });
  execFileSync('git', ['add', 'a.txt'], { cwd: repo });
  writeFileSync(join(repo, 'a.txt'), 'clean\n');
  const deny = join(mkdtempSync(join(tmpdir(), 'gl-deny-')), 'deny.txt');
  writeFileSync(deny, 'acmeboard\n');
  const r = runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: true });
  assert.deepEqual(r.findings.map((f) => f.where), ['(staged) a.txt']);
});

test('--history: checks history of deleted files and commit messages', () => {
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
  assert.ok(where.includes('(history) a.txt'), where.join('|'));
  assert.ok(where.includes('(commit message) <sha>'), where.join('|'));
});

test('binary files are checked for embedded strings', () => {
  const hits = scanText('img.bin', j('\x89PNG\0\0', 'xx acme', 'board yy', '\0\x01'), makeRules(['acmeboard']));
  assert.deepEqual(hits, [{ where: 'img.bin (binary)', line: null, rule: 'D1 denylisted word' }]);
});

test('empty or comment-only denylist: fail in the hook, warn otherwise', () => {
  const repo = repoWith({ 'a.txt': 'x\n' });
  const deny = join(mkdtempSync(join(tmpdir(), 'gl-deny-')), 'deny.txt');
  writeFileSync(deny, '# comment only\n\n');
  assert.ok(!runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: true }).ok);
  const r = runPublicCheck({ repo, denylistFile: deny, history: false, requireDenylist: false });
  assert.ok(r.ok);
  assert.equal(r.warnings.length, 1);
});

test('--history: old path names of renamed files with the same content are checked', () => {
  const repo = repoWith({ 'acmeboard-plan.txt': 'same\n' });
  const g = (...a: string[]) => execFileSync('git', a, { cwd: repo, stdio: 'ignore' });
  g('add', '.');
  g('commit', '-q', '-m', 'a');
  g('mv', 'acmeboard-plan.txt', 'plan.txt');
  g('commit', '-q', '-m', 'b');
  const deny = join(mkdtempSync(join(tmpdir(), 'gl-deny-')), 'deny.txt');
  writeFileSync(deny, 'acmeboard\n');
  const r = runPublicCheck({ repo, denylistFile: deny, history: true, requireDenylist: true });
  assert.ok(r.findings.some((f) => f.where === '(path) acmeboard-plan.txt'), JSON.stringify(r.findings));
});
