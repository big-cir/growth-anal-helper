// 감사 기록(<outDir>/logs/audit/audit-YYYY-MM-DD.jsonl): 허용한 필드만, 값 200자 이하. 디렉터리 0700, 파일 0600.
import { closeSync, constants, existsSync, fstatSync, lstatSync, mkdirSync, openSync, readdirSync, rmSync, writeSync } from 'node:fs';
import { join } from 'node:path';

export type AuditEvent =
  | 'login_ok' | 'login_fail' | 'logout' | 'session_revoked'
  | 'agent_request' | 'offdict_approved' | 'sensitive_blocked'
  | 'panel_saved' | 'panel_deleted' | 'recompute' | 'regenerate' | 'resummarize'
  | 'account_changed';

export type AuditRecord = { event: AuditEvent | `${AuditEvent}_failed`; user?: string; ip?: string; target?: string };

const cut = (v: string | undefined) => (v === undefined ? undefined : v.slice(0, 200));
const day = (d: Date) => d.toISOString().slice(0, 10);

export class AuditLog {
  private readonly dir: string;

  constructor(outDir: string) {
    this.dir = join(outDir, 'logs', 'audit');
  }

  /** 실패하면 예외(호출한 쪽이 변경을 거부). 권한이 안전하지 않으면 쓰지 않는다 */
  write(r: AuditRecord): void {
    if (!existsSync(this.dir)) mkdirSync(this.dir, { recursive: true, mode: 0o700 });
    const me = typeof process.getuid === 'function' ? process.getuid() : -1;
    const d = lstatSync(this.dir);
    if (d.isSymbolicLink() || !d.isDirectory() || (me >= 0 && d.uid !== me) || (d.mode & 0o777) !== 0o700) throw new Error(`${this.dir}: 감사 기록 디렉터리는 내 소유 0700이어야 함`);
    const f = join(this.dir, `audit-${day(new Date())}.jsonl`);
    const line = JSON.stringify({ t: new Date().toISOString(), event: r.event, user: cut(r.user), ip: cut(r.ip), target: cut(r.target) });
    const fd = openSync(f, constants.O_WRONLY | constants.O_APPEND | constants.O_CREAT | constants.O_NOFOLLOW, 0o600);
    try {
      const st = fstatSync(fd);
      if (!st.isFile() || (me >= 0 && st.uid !== me) || (st.mode & 0o777) !== 0o600) throw new Error(`${f}: 감사 기록 파일은 내 소유 0600이어야 함`);
      writeSync(fd, line + '\n');
    } finally {
      closeSync(fd);
    }
  }

  /** 기록 실패를 stderr에만 남긴다(로그인 실패 등) */
  tryWrite(r: AuditRecord): void {
    try {
      this.write(r);
    } catch (e) {
      console.error(`감사 기록 실패: ${(e as Error).message}`);
    }
  }

  prune(retentionDays: number): void {
    if (!existsSync(this.dir)) return;
    const cutoff = day(new Date(Date.now() - retentionDays * 86_400_000));
    for (const f of readdirSync(this.dir)) {
      const m = /^audit-(\d{4}-\d{2}-\d{2})\.jsonl$/.exec(f);
      if (m && m[1] < cutoff) rmSync(join(this.dir, f), { force: true });
    }
  }
}
