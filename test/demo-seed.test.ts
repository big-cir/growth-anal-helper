// Demo seed data and demo derived SQL.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { DatabaseSync } from 'node:sqlite';
import { mkdtempSync, readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { seedDemo } from '../examples/demo/seed.ts';
import { normalizeTs } from '../src/time.ts';

const DEMO = join(import.meta.dirname, '..', 'examples', 'demo');
const ANCHOR = '2024-06-03 12:00:00';

type Col = { expr: string; as: string; kind: string; role: unknown };
type Spec = { source: string; target: string; key: string[]; cutoffColumn: string; columns: Col[] };

function fileHash(p: string): string {
  const db = new DatabaseSync(p, { readOnly: true });
  const h = createHash('sha256');
  for (const t of ['member', 'board', 'board_member', 'post', 'reply', 'reaction']) {
    for (const row of db.prepare(`SELECT * FROM ${t} ORDER BY 1, 2`).all()) h.update(JSON.stringify(row));
  }
  db.close();
  return h.digest('hex');
}

function quickSnapshot(sourcePath: string, cutoff: string): DatabaseSync {
  const specs: Spec[] = JSON.parse(readFileSync(join(DEMO, 'tables.json'), 'utf8'));
  const src = new DatabaseSync(sourcePath, { readOnly: true });
  const db = new DatabaseSync(':memory:');
  db.exec(`CREATE TABLE snapshot_meta(source_cutoff_at TEXT); INSERT INTO snapshot_meta VALUES ('${cutoff}');
           CREATE TABLE snapshot_params(key TEXT, value TEXT);`);
  const params = JSON.parse(readFileSync(join(DEMO, 'workspace.json'), 'utf8')).params as Record<string, string>;
  for (const [k, v] of Object.entries(params)) db.prepare('INSERT INTO snapshot_params VALUES (?, ?)').run(k, v);
  for (const s of specs) {
    db.exec(`CREATE TABLE ${s.target} (${s.columns.map((c) => c.as).join(', ')})`);
    const ins = db.prepare(`INSERT INTO ${s.target} VALUES (${s.columns.map(() => '?').join(', ')})`);
    const sel = `SELECT ${s.columns.map((c) => `${c.expr} AS ${c.as}`).join(', ')} FROM ${s.source} ORDER BY ${s.key.join(', ')}`;
    for (const row of src.prepare(sel).iterate()) {
      const vals = s.columns.map((c) => {
        const v = (row as Record<string, unknown>)[c.as];
        return c.kind === 'ts' && typeof v === 'string' ? normalizeTs(v) : (v as never);
      });
      ins.run(...vals);
    }
    db.prepare(`DELETE FROM ${s.target} WHERE ${s.cutoffColumn} > ?`).run(cutoff);
    for (const c of s.columns) {
      if (c.kind === 'ts' && c.as.startsWith('deleted')) db.prepare(`UPDATE ${s.target} SET ${c.as} = NULL WHERE ${c.as} > ?`).run(cutoff);
    }
  }
  src.close();
  return db;
}

test('cutoff cleanup in a simple collect: rows after the cutoff are dropped, deletions after it become NULL', () => {
  const p = join(mkdtempSync(join(tmpdir(), 'gl-seed-')), 's.sqlite');
  seedDemo(p, { anchor: ANCHOR });
  const src = new DatabaseSync(p, { readOnly: true });
  const edge = src.prepare("SELECT id, created_at FROM post WHERE created_at >= '2024-04-01' ORDER BY created_at LIMIT 1").get() as { id: number; created_at: string };
  src.close();
  const cutoff = normalizeTs(edge.created_at);
  const db = quickSnapshot(p, cutoff);
  const n = (sql: string) => (db.prepare(sql).get() as { n: number }).n;
  for (const t of ['r_member', 'r_board', 'r_post', 'r_reply', 'r_reaction']) {
    assert.equal(n(`SELECT count(*) n FROM ${t} WHERE created_at > '${cutoff}'`), 0, t);
    assert.ok(n(`SELECT count(*) n FROM ${t}`) > 0, t);
  }
  assert.equal(n(`SELECT count(*) n FROM r_board_member WHERE joined_at > '${cutoff}'`), 0);
  for (const t of ['r_member', 'r_board', 'r_post', 'r_reply']) assert.equal(n(`SELECT count(*) n FROM ${t} WHERE deleted_at > '${cutoff}'`), 0, t);
  assert.equal(n(`SELECT count(*) n FROM r_post WHERE id = ${edge.id} AND created_at = '${cutoff}'`), 1, 'a row exactly at the cutoff is kept');
  db.close();
});

test('seed is deterministic (same seed and anchor → same data)', () => {
  const dir = mkdtempSync(join(tmpdir(), 'gl-seed-'));
  const a = seedDemo(join(dir, 'a.sqlite'), { anchor: ANCHOR });
  const b = seedDemo(join(dir, 'b.sqlite'), { anchor: ANCHOR });
  assert.deepEqual(a, b);
  assert.equal(fileHash(join(dir, 'a.sqlite')), fileHash(join(dir, 'b.sqlite')));
  const c = seedDemo(join(dir, 'c.sqlite'), { anchor: ANCHOR, seed: 7 });
  assert.notDeepEqual(a, c);
});

test('seed data is consistent: every row before the anchor, no activity after leaving, authors are board members', () => {
  const p = join(mkdtempSync(join(tmpdir(), 'gl-seed-')), 's.sqlite');
  seedDemo(p, { anchor: ANCHOR });
  const db = new DatabaseSync(p, { readOnly: true });
  const zero = (sql: string) => assert.equal((db.prepare(sql).get() as { n: number }).n, 0, sql);
  for (const t of ['member', 'board', 'post', 'reply', 'reaction']) zero(`SELECT count(*) n FROM ${t} WHERE created_at >= '${ANCHOR}'`);
  zero(`SELECT count(*) n FROM board_member WHERE joined_at >= '${ANCHOR}' OR left_at >= '${ANCHOR}'`);
  for (const t of ['member', 'board', 'post', 'reply']) zero(`SELECT count(*) n FROM ${t} WHERE deleted_at >= '${ANCHOR}'`);
  zero('SELECT count(*) n FROM post p JOIN member m ON m.id = p.member_id WHERE m.deleted_at IS NOT NULL AND p.created_at >= m.deleted_at');
  zero('SELECT count(*) n FROM post p LEFT JOIN board_member bm ON bm.board_id = p.board_id AND bm.member_id = p.member_id WHERE bm.member_id IS NULL OR p.created_at < bm.joined_at OR (bm.left_at IS NOT NULL AND p.created_at >= bm.left_at)');
  zero('SELECT count(*) n FROM reply x JOIN post p ON p.id = x.post_id WHERE x.created_at < p.created_at');
  zero('SELECT count(*) n FROM board_member bm JOIN board b ON b.id = bm.board_id WHERE b.deleted_at IS NOT NULL AND bm.joined_at >= b.deleted_at');
  zero('SELECT count(*) n FROM post p JOIN board b ON b.id = p.board_id WHERE b.deleted_at IS NOT NULL AND p.created_at >= b.deleted_at');
  for (const t of ['reply', 'reaction']) {
    zero(`SELECT count(*) n FROM ${t} x JOIN post p ON p.id = x.post_id JOIN board b ON b.id = p.board_id
          JOIN member m ON m.id = x.member_id
          LEFT JOIN board_member bm ON bm.board_id = p.board_id AND bm.member_id = x.member_id
          WHERE bm.member_id IS NULL OR x.created_at < bm.joined_at OR (bm.left_at IS NOT NULL AND x.created_at >= bm.left_at)
             OR (b.deleted_at IS NOT NULL AND x.created_at >= b.deleted_at) OR (m.deleted_at IS NOT NULL AND x.created_at >= m.deleted_at)`);
  }
  for (const t of ['member', 'board', 'post', 'reply']) zero(`SELECT count(*) n FROM ${t} WHERE deleted_at IS NOT NULL AND deleted_at <= created_at`);
  zero('SELECT count(*) n FROM board_member WHERE left_at IS NOT NULL AND left_at <= joined_at');
  const n = (sql: string) => (db.prepare(sql).get() as { n: number }).n;
  assert.ok(n('SELECT count(*) n FROM member WHERE deleted_at IS NOT NULL') > 0);
  assert.ok(n('SELECT count(*) n FROM board_member WHERE left_at IS NOT NULL') > 0);
  assert.ok(n('SELECT count(*) n FROM reply WHERE parent_reply_id IS NOT NULL') > 0);
  assert.ok(n("SELECT count(*) n FROM member WHERE length(created_at) = 19") > 0);
  assert.ok(n("SELECT count(*) n FROM post WHERE length(created_at) = 26") > 0);
  db.close();
});

test('demo derived.sql runs and the funnel shrinks at every step', () => {
  const p = join(mkdtempSync(join(tmpdir(), 'gl-seed-')), 's.sqlite');
  seedDemo(p, { anchor: ANCHOR });
  const db = quickSnapshot(p, normalizeTs(ANCHOR));
  db.exec(readFileSync(join(DEMO, 'derived.sql'), 'utf8'));

  const f = db.prepare(`SELECT count(*) signup,
      sum(board_state = 'reached') board, sum(connected_state = 'reached') connected,
      sum(post_state = 'reached') post, sum(received_state = 'reached') received
    FROM d_member_first_week`).get() as Record<string, number>;
  assert.ok(f.signup > 1000, JSON.stringify(f));
  assert.ok(f.signup > f.board && f.board > f.connected && f.connected > f.post && f.post > f.received && f.received > 0, JSON.stringify(f));

  const bad = db.prepare(`SELECT count(*) n FROM d_member_first_week WHERE
      (board_state = 'not' AND connected_state IS NOT NULL) OR
      (connected_state = 'not' AND post_state IS NOT NULL) OR
      (post_state = 'not' AND received_state IS NOT NULL)`).get() as { n: number };
  assert.equal(bad.n, 0);

  const timeMismatch = db.prepare(`SELECT count(*) n FROM d_member_first_week WHERE
      ((board_state = 'reached') <> (board_at IS NOT NULL)) OR ((connected_state = 'reached') <> (connected_at IS NOT NULL)) OR
      ((post_state = 'reached') <> (first_post_at IS NOT NULL)) OR ((received_state = 'reached') <> (received_at IS NOT NULL))`).get() as { n: number };
  assert.equal(timeMismatch.n, 0);

  const order = db.prepare(`SELECT count(*) n FROM d_member_first_week WHERE
      board_at < signup_at OR connected_at < board_at OR first_post_at < connected_at OR received_at < first_post_at
      OR received_at >= week_end OR first_post_at >= week_end`).get() as { n: number };
  assert.equal(order.n, 0);

  const lens = db.prepare('SELECT DISTINCT length(week_end) l FROM d_member_first_week').all() as { l: number }[];
  assert.deepEqual(lens.map((x) => x.l), [26]);

  const loose = db.prepare(`SELECT count(*) n FROM d_member_first_week w WHERE w.post_state = 'reached' AND EXISTS (
      SELECT 1 FROM r_post p JOIN d_activity x ON x.post_id = p.id AND x.member_id <> w.member_id AND x.kind <> 'post'
      WHERE p.member_id = w.member_id AND x.at >= w.first_post_at AND x.at < w.week_end)`).get() as { n: number };
  assert.ok(loose.n > f.received, `loose ${loose.n} vs strict ${f.received}`);

  const w0 = db.prepare('SELECT count(DISTINCT member_id) n FROM d_member_activity_week WHERE life_week = 0 AND post_cnt > 0').get() as { n: number };
  assert.ok(w0.n >= f.post);

  const declared = Object.keys(JSON.parse(readFileSync(join(DEMO, 'derived-columns.json'), 'utf8'))).sort();
  const actual: string[] = [];
  for (const { name } of db.prepare("SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'd\\_%' ESCAPE '\\'").all() as { name: string }[]) {
    for (const c of db.prepare(`PRAGMA table_info(${name})`).all() as { name: string }[]) actual.push(`${name}.${c.name}`);
  }
  assert.deepEqual(actual.sort(), declared);
  db.close();
});
