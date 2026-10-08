// 데모 소스 DB 생성기: 가상 커뮤니티 게시판. 같은 seed·anchor면 같은 DB.
// 사용: node examples/demo/seed.ts <출력.sqlite> [--anchor "YYYY-MM-DD HH:MM:SS"] [--seed N]
import { DatabaseSync } from 'node:sqlite';
import { existsSync, mkdirSync, rmSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

export type SeedOptions = { anchor: string; seed?: number; weeks?: number; members?: number };
export type SeedSummary = { members: number; boards: number; boardMembers: number; posts: number; replies: number; reactions: number };

/** 결정적 난수(mulberry32) */
function rng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const MS_DAY = 86_400_000;

function parseLocal(ts: string): number {
  const m = /^(\d{4})-(\d{2})-(\d{2})(?:[ T](\d{2}):(\d{2}):(\d{2}))?$/.exec(ts);
  if (!m) throw new Error(`anchor 형식 오류: ${ts}`);
  return Date.UTC(+m[1], +m[2] - 1, +m[3], +(m[4] ?? 0), +(m[5] ?? 0), +(m[6] ?? 0));
}

/** micro면 소수점 6자리, 아니면 초까지 */
function fmt(ms: number, micro: boolean, r: () => number): string {
  const iso = new Date(ms).toISOString();
  const base = `${iso.slice(0, 10)} ${iso.slice(11, 19)}`;
  return micro ? `${base}.${String(Math.floor(r() * 1e6)).padStart(6, '0')}` : base;
}

const SCHEMA = `
CREATE TABLE member (
  id INTEGER PRIMARY KEY, created_at TEXT NOT NULL, deleted_at TEXT, country TEXT NOT NULL, nickname TEXT NOT NULL
);
CREATE TABLE board (
  id INTEGER PRIMARY KEY, created_at TEXT NOT NULL, deleted_at TEXT, owner_id INTEGER NOT NULL, title TEXT NOT NULL
);
CREATE TABLE board_member (
  board_id INTEGER NOT NULL, member_id INTEGER NOT NULL, joined_at TEXT NOT NULL, left_at TEXT,
  PRIMARY KEY (board_id, member_id)
);
CREATE TABLE post (
  id INTEGER PRIMARY KEY, board_id INTEGER NOT NULL, member_id INTEGER NOT NULL, created_at TEXT NOT NULL, deleted_at TEXT, body TEXT NOT NULL
);
CREATE TABLE reply (
  id INTEGER PRIMARY KEY, post_id INTEGER NOT NULL, member_id INTEGER NOT NULL, parent_reply_id INTEGER,
  created_at TEXT NOT NULL, deleted_at TEXT, body TEXT NOT NULL
);
CREATE TABLE reaction (
  id INTEGER PRIMARY KEY, post_id INTEGER NOT NULL, member_id INTEGER NOT NULL, created_at TEXT NOT NULL
);`;

const COUNTRIES = ['KR', 'KR', 'KR', 'KR', 'JP', 'US', 'TW'];

type Member = { id: number; signup: number; deleted: number | null; propensity: number; boards: number[] };
type Board = { id: number; created: number; deleted: number | null; members: Map<number, number> };

export function seedDemo(path: string, o: SeedOptions): SeedSummary {
  const r = rng(o.seed ?? 20240501);
  const anchor = parseLocal(o.anchor);
  const weeks = o.weeks ?? 20;
  const total = o.members ?? 1800;
  const start = anchor - weeks * 7 * MS_DAY;
  const pick = <T>(xs: T[]) => xs[Math.floor(r() * xs.length)];

  // 회원
  const members: Member[] = [];
  for (let i = 0; i < total; i++) {
    const u = Math.sqrt(r());
    const signup = start + Math.floor(u * (anchor - start - MS_DAY));
    members.push({ id: 0, signup, deleted: null, propensity: 0.25 + r() * 0.6, boards: [] });
  }
  members.sort((a, b) => a.signup - b.signup);
  members.forEach((m, i) => { m.id = 1001 + i; });
  // 일부 탈퇴(탈퇴 뒤 활동 없음)
  for (const m of members) if (r() < 0.04) m.deleted = m.signup + Math.floor((3 + r() * 40) * MS_DAY);
  const activeAt = (m: Member, at: number) => at < anchor && (m.deleted === null || at < m.deleted);

  // 게시판 만들기·가입
  const boards: Board[] = [];
  const events: { at: number; kind: 'create' | 'join'; m: Member }[] = [];
  for (const m of members) {
    const roll = r();
    if (roll < 0.28) events.push({ at: m.signup + Math.floor(r() * 2 * MS_DAY), kind: 'create', m });
    else if (roll < 0.78) events.push({ at: m.signup + Math.floor(r() * 7 * MS_DAY), kind: 'join', m });
    else if (roll < 0.86) events.push({ at: m.signup + Math.floor((7 + r() * 20) * MS_DAY), kind: 'join', m });
  }
  events.sort((a, b) => a.at - b.at);
  for (const e of events) {
    if (!activeAt(e.m, e.at)) continue;
    if (e.kind === 'create' || boards.length === 0) {
      const deleted = r() < 0.03 ? e.at + Math.floor((10 + r() * 30) * MS_DAY) : null;
      const b: Board = { id: 501 + boards.length, created: e.at, deleted, members: new Map([[e.m.id, e.at]]) };
      boards.push(b);
      e.m.boards.push(b.id);
    } else {
      const open = (b: Board) => b.deleted === null || e.at + 1000 < b.deleted;
      const recent = boards.slice(-60).filter((b) => b.members.size < 6 && !b.members.has(e.m.id) && open(b));
      const b = recent.length ? pick(recent) : pick(boards);
      if (b.members.has(e.m.id) || !open(b)) continue;
      b.members.set(e.m.id, Math.max(e.at, b.created + 1000));
      e.m.boards.push(b.id);
    }
  }
  const boardById = new Map(boards.map((b) => [b.id, b]));
  const memberById = new Map(members.map((m) => [m.id, m]));

  // 일부는 게시판을 떠난다
  const leftAt = new Map<string, number>();
  for (const b of boards) for (const [mid, joined] of b.members) {
    if (mid !== [...b.members.keys()][0] && r() < 0.05) leftAt.set(`${b.id}:${mid}`, joined + Math.floor((2 + r() * 30) * MS_DAY));
  }
  const inBoard = (bid: number, m: Member, at: number) => {
    const left = leftAt.get(`${bid}:${m.id}`);
    const b = boardById.get(bid)!;
    return activeAt(m, at) && (left === undefined || at < left) && (b.deleted === null || at < b.deleted);
  };

  // 활동
  const posts: { id: number; board: number; member: number; at: number; deleted: number | null }[] = [];
  const replies: { id: number; post: number; member: number; parent: number | null; at: number; deleted: number | null }[] = [];
  const reactions: { id: number; post: number; member: number; at: number }[] = [];
  for (const m of members) {
    for (let w = 0; ; w++) {
      const weekStart = m.signup + w * 7 * MS_DAY;
      if (weekStart >= anchor) break;
      for (const bid of m.boards) {
        const b = boardById.get(bid)!;
        const joined = b.members.get(m.id)!;
        if (joined >= weekStart + 7 * MS_DAY) continue;
        const connected = b.members.size >= 2;
        const p = m.propensity * Math.pow(0.82, w) * (connected ? 1.7 : 0.7);
        if (r() >= Math.min(p, 0.95)) continue;
        const n = 1 + Math.floor(r() * 3);
        for (let k = 0; k < n; k++) {
          const at = Math.max(joined + 60_000, weekStart) + Math.floor(r() * 7 * MS_DAY);
          if (!inBoard(bid, m, at)) continue;
          posts.push({ id: 0, board: bid, member: m.id, at, deleted: r() < 0.03 ? at + 1000 + Math.floor(r() * 10 * MS_DAY) : null });
        }
      }
    }
  }
  posts.sort((a, b) => a.at - b.at);
  posts.forEach((p, i) => { p.id = 70001 + i; });

  for (const p of posts) {
    const b = boardById.get(p.board)!;
    for (const [mid, joined] of b.members) {
      if (mid === p.member) continue;
      const other = memberById.get(mid)!;
      if (r() < other.propensity * 0.5) {
        const at = Math.max(p.at, joined) + Math.floor(r() * 2 * MS_DAY) + 30_000;
        if (inBoard(p.board, other, at)) reactions.push({ id: 0, post: p.id, member: mid, at });
      }
      if (r() < other.propensity * 0.35) {
        const at = Math.max(p.at, joined) + Math.floor(r() * 3 * MS_DAY) + 60_000;
        if (!inBoard(p.board, other, at)) continue;
        replies.push({ id: 0, post: p.id, member: mid, parent: null, at, deleted: r() < 0.04 ? at + MS_DAY : null });
        if (r() < 0.4) {
          const at2 = at + Math.floor(r() * MS_DAY) + 30_000;
          if (inBoard(p.board, memberById.get(p.member)!, at2)) replies.push({ id: -1, post: p.id, member: p.member, parent: null, at: at2, deleted: null });
        }
      }
    }
  }
  replies.sort((a, b) => a.at - b.at);
  const lastReplyOfPost = new Map<number, number>();
  replies.forEach((x, i) => {
    const isNested = x.id === -1;
    x.id = 900001 + i;
    x.parent = isNested ? lastReplyOfPost.get(x.post) ?? null : null;
    if (!isNested) lastReplyOfPost.set(x.post, x.id);
  });
  reactions.sort((a, b) => a.at - b.at);
  reactions.forEach((x, i) => { x.id = 300001 + i; });


  // 저장
  if (existsSync(path)) rmSync(path);
  mkdirSync(dirname(resolve(path)), { recursive: true });
  const db = new DatabaseSync(path);
  db.exec(SCHEMA);
  db.exec('BEGIN');
  const ins = (sql: string) => db.prepare(sql);
  const iMember = ins('INSERT INTO member VALUES (?, ?, ?, ?, ?)');
  for (const m of members) iMember.run(m.id, fmt(m.signup, false, r), m.deleted === null || m.deleted >= anchor ? null : fmt(m.deleted, false, r), pick(COUNTRIES), `회원${m.id}`);
  const iBoard = ins('INSERT INTO board VALUES (?, ?, ?, ?, ?)');
  for (const b of boards) {
    iBoard.run(b.id, fmt(b.created, true, r), b.deleted === null || b.deleted >= anchor ? null : fmt(b.deleted, true, r), [...b.members.keys()][0], `소모임 ${b.id}`);
  }
  const iBm = ins('INSERT INTO board_member VALUES (?, ?, ?, ?)');
  for (const b of boards) for (const [mid, joined] of b.members) {
    const left = leftAt.get(`${b.id}:${mid}`);
    iBm.run(b.id, mid, fmt(joined, true, r), left === undefined || left >= anchor ? null : fmt(left, true, r));
  }
  const iPost = ins('INSERT INTO post VALUES (?, ?, ?, ?, ?, ?)');
  for (const p of posts) iPost.run(p.id, p.board, p.member, fmt(p.at, true, r), p.deleted === null || p.deleted >= anchor ? null : fmt(p.deleted, true, r), `글 ${p.id}`);
  const iReply = ins('INSERT INTO reply VALUES (?, ?, ?, ?, ?, ?, ?)');
  for (const x of replies) iReply.run(x.id, x.post, x.member, x.parent, fmt(x.at, true, r), x.deleted === null || x.deleted >= anchor ? null : fmt(x.deleted, true, r), `댓글 ${x.id}`);
  const iReaction = ins('INSERT INTO reaction VALUES (?, ?, ?, ?)');
  for (const x of reactions) iReaction.run(x.id, x.post, x.member, fmt(x.at, true, r));
  db.exec('COMMIT');
  db.close();

  return {
    members: members.length,
    boards: boards.length,
    boardMembers: boards.reduce((s, b) => s + b.members.size, 0),
    posts: posts.length,
    replies: replies.length,
    reactions: reactions.length,
  };
}

function nowLocal(): string {
  const d = new Date();
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const out = args[0];
  if (!out || out.startsWith('--')) {
    console.error('사용법: node examples/demo/seed.ts <출력.sqlite> [--anchor "YYYY-MM-DD HH:MM:SS"] [--seed N]');
    process.exit(2);
  }
  const opt = (name: string) => {
    const i = args.indexOf(name);
    return i >= 0 ? args[i + 1] : undefined;
  };
  const summary = seedDemo(out, { anchor: opt('--anchor') ?? nowLocal(), seed: opt('--seed') ? Number(opt('--seed')) : undefined });
  console.log(JSON.stringify(summary));
}
