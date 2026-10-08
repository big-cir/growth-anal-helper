// 계정 파일(<outDir>/auth/accounts.json): 권한 검사, scrypt 해시, 잠금 후 원자적 갱신.
import { closeSync, constants, existsSync, fstatSync, fsyncSync, lstatSync, mkdirSync, openSync, readFileSync, renameSync, rmSync, writeSync } from 'node:fs';
import { randomBytes, scrypt as scryptCb, timingSafeEqual } from 'node:crypto';
import { join } from 'node:path';

export type Role = 'viewer' | 'editor' | 'admin';
export const ROLES: Role[] = ['viewer', 'editor', 'admin'];
export type Account = { username: string; role: Role; hash: string; disabled: boolean; created_at: string; changed_at: string };

export class AccountError extends Error {}

export const USERNAME_RE = /^[a-z][a-z0-9_.-]{2,31}$/;
export const PASSWORD_LIMITS = { minChars: 12, maxBytes: 256 };
const SCRYPT = { N: 2 ** 15, r: 8, p: 1, keyLen: 64, saltLen: 16, maxmem: 64 * 1024 * 1024 };

export const authDir = (outDir: string) => join(outDir, 'auth');
export const accountsFile = (outDir: string) => join(authDir(outDir), 'accounts.json');

function scrypt(password: string, salt: Buffer, N: number, r: number, p: number, keyLen: number): Promise<Buffer> {
  return new Promise((resolve, reject) => scryptCb(password, salt, keyLen, { N, r, p, maxmem: SCRYPT.maxmem }, (e, k) => (e ? reject(e) : resolve(k))));
}

export function checkPassword(password: string): void {
  if ([...password].length < PASSWORD_LIMITS.minChars) throw new AccountError(`비밀번호는 ${PASSWORD_LIMITS.minChars}자 이상`);
  if (Buffer.byteLength(password) > PASSWORD_LIMITS.maxBytes) throw new AccountError(`비밀번호는 ${PASSWORD_LIMITS.maxBytes}바이트 이하`);
}

export async function hashPassword(password: string): Promise<string> {
  checkPassword(password);
  const salt = randomBytes(SCRYPT.saltLen);
  const key = await scrypt(password, salt, SCRYPT.N, SCRYPT.r, SCRYPT.p, SCRYPT.keyLen);
  return `scrypt$${SCRYPT.N}$${SCRYPT.r}$${SCRYPT.p}$${salt.toString('base64')}$${key.toString('base64')}`;
}

export async function verifyPassword(password: string, hash: string): Promise<boolean> {
  if (Buffer.byteLength(password) > PASSWORD_LIMITS.maxBytes) return false;
  const m = /^scrypt\$(\d+)\$(\d+)\$(\d+)\$([A-Za-z0-9+/=]+)\$([A-Za-z0-9+/=]+)$/.exec(hash);
  if (!m) return false;
  const [N, r, p] = [Number(m[1]), Number(m[2]), Number(m[3])];
  if (N !== SCRYPT.N || r !== SCRYPT.r || p !== SCRYPT.p) return false;
  const want = Buffer.from(m[5], 'base64');
  const got = await scrypt(password, Buffer.from(m[4], 'base64'), N, r, p, want.length);
  return got.length === want.length && timingSafeEqual(got, want);
}

const uid = () => (typeof process.getuid === 'function' ? process.getuid() : -1);

/** auth/ 디렉터리: 심볼릭 링크가 아닌 디렉터리, 내 소유, 0700 */
function checkDir(dir: string): void {
  const st = lstatSync(dir);
  if (st.isSymbolicLink() || !st.isDirectory()) throw new AccountError(`${dir}: 디렉터리가 아님(심볼릭 링크 불가)`);
  if (uid() >= 0 && st.uid !== uid()) throw new AccountError(`${dir}: 소유자가 현재 사용자가 아님`);
  if ((st.mode & 0o077) !== 0) throw new AccountError(`${dir}: 권한은 0700이어야 함`);
}

export function ensureAuthDir(outDir: string): string {
  const dir = authDir(outDir);
  if (!existsSync(dir)) mkdirSync(dir, { recursive: true, mode: 0o700 });
  checkDir(dir);
  return dir;
}

function parseAccounts(text: string): Account[] {
  const raw = JSON.parse(text) as { version?: number; accounts?: unknown };
  if (raw.version !== 1 || !Array.isArray(raw.accounts)) throw new AccountError('계정 파일 형식 오류');
  const seen = new Set<string>();
  return raw.accounts.map((a) => {
    const x = a as Account;
    if (!USERNAME_RE.test(x.username) || !ROLES.includes(x.role) || typeof x.hash !== 'string' || typeof x.disabled !== 'boolean') throw new AccountError('계정 파일 항목 형식 오류');
    if (seen.has(x.username)) throw new AccountError(`계정 중복: ${x.username}`);
    seen.add(x.username);
    return { username: x.username, role: x.role, hash: x.hash, disabled: x.disabled, created_at: String(x.created_at), changed_at: String(x.changed_at) };
  });
}

/** 파일이 없으면 빈 목록. O_NOFOLLOW로 열고 fstat으로 확인한 그 기술자로 읽는다 */
export function readAccounts(outDir: string): Account[] {
  const dir = authDir(outDir);
  if (!existsSync(dir)) return [];
  checkDir(dir);
  const f = accountsFile(outDir);
  let fd: number;
  try {
    fd = openSync(f, constants.O_RDONLY | constants.O_NOFOLLOW);
  } catch (e) {
    const code = (e as NodeJS.ErrnoException).code;
    if (code === 'ENOENT') return [];
    if (code === 'ELOOP') throw new AccountError(`${f}: 심볼릭 링크는 쓸 수 없음`);
    throw e;
  }
  try {
    const st = fstatSync(fd);
    if (!st.isFile()) throw new AccountError(`${f}: 일반 파일이 아님`);
    if (uid() >= 0 && st.uid !== uid()) throw new AccountError(`${f}: 소유자가 현재 사용자가 아님`);
    if ((st.mode & 0o077) !== 0) throw new AccountError(`${f}: 권한은 0600이어야 함`);
    return parseAccounts(readFileSync(fd, 'utf8'));
  } finally {
    closeSync(fd);
  }
}

function pidAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    return (e as NodeJS.ErrnoException).code === 'EPERM';
  }
}

/** 잠금(PID·시각) → 임시 파일 → fsync → rename → 디렉터리 fsync */
export function updateAccounts(outDir: string, change: (accounts: Account[]) => Account[]): Account[] {
  const dir = ensureAuthDir(outDir);
  const lock = join(dir, '.lock');
  let fd: number;
  try {
    fd = openSync(lock, constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY, 0o600);
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code !== 'EEXIST') throw e;
    const pid = Number(String(readFileSync(lock, 'utf8')).split(' ')[0]);
    if (Number.isInteger(pid) && pid > 0 && pidAlive(pid)) throw new AccountError('다른 계정 변경이 진행 중이에요');
    rmSync(lock, { force: true });
    fd = openSync(lock, constants.O_CREAT | constants.O_EXCL | constants.O_WRONLY, 0o600);
  }
  try {
    writeSync(fd, `${process.pid} ${new Date().toISOString()}`);
    closeSync(fd);
    const next = change(readAccounts(outDir));
    const target = accountsFile(outDir);
    const tmp = join(dir, `.accounts.${process.pid}.tmp`);
    const t = openSync(tmp, constants.O_CREAT | constants.O_TRUNC | constants.O_WRONLY | constants.O_NOFOLLOW, 0o600);
    try {
      writeSync(t, JSON.stringify({ version: 1, accounts: next }, null, 1) + '\n');
      fsyncSync(t);
    } finally {
      closeSync(t);
    }
    renameSync(tmp, target);
    const d = openSync(dir, 'r');
    try {
      fsyncSync(d);
    } finally {
      closeSync(d);
    }
    return next;
  } finally {
    rmSync(lock, { force: true });
  }
}

export async function addAccount(outDir: string, username: string, role: Role, password: string): Promise<void> {
  if (!USERNAME_RE.test(username)) throw new AccountError('이름은 ^[a-z][a-z0-9_.-]{2,31}$');
  if (!ROLES.includes(role)) throw new AccountError(`역할은 ${ROLES.join('|')}`);
  const hash = await hashPassword(password);
  const now = new Date().toISOString();
  updateAccounts(outDir, (list) => {
    if (list.some((a) => a.username === username)) throw new AccountError(`이미 있는 계정: ${username}`);
    return [...list, { username, role, hash, disabled: false, created_at: now, changed_at: now }];
  });
}

export function modifyAccount(outDir: string, username: string, patch: Partial<Pick<Account, 'role' | 'disabled' | 'hash'>>): void {
  if (patch.role !== undefined && !ROLES.includes(patch.role)) throw new AccountError(`역할은 ${ROLES.join('|')}`);
  updateAccounts(outDir, (list) => {
    const i = list.findIndex((a) => a.username === username);
    if (i < 0) throw new AccountError(`없는 계정: ${username}`);
    const next = [...list];
    next[i] = { ...next[i], ...patch, changed_at: new Date().toISOString() };
    return next;
  });
}
