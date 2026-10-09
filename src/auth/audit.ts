// Audit log (<outDir>/logs/audit/audit-YYYY-MM-DD.jsonl): allowed fields only, values up to 200 characters. Directory 0700, files 0600.
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

  /** Throws on failure (the caller then refuses the change). Does not write if permissions are unsafe */
  write(r: AuditRecord): void {
    if (!existsSync(this.dir)) mkdirSync(this.dir, { recursive: true, mode: 0o700 });
    const me = typeof process.getuid === 'function' ? process.getuid() : -1;
    const d = lstatSync(this.dir);
    if (d.isSymbolicLink() || !d.isDirectory() || (me >= 0 && d.uid !== me) || (d.mode & 0o777) !== 0o700) throw new Error(`${this.dir}: audit log directory must be owned by me with 0700`);
    const f = join(this.dir, `audit-${day(new Date())}.jsonl`);
    const line = JSON.stringify({ t: new Date().toISOString(), event: r.event, user: cut(r.user), ip: cut(r.ip), target: cut(r.target) });
    const fd = openSync(f, constants.O_WRONLY | constants.O_APPEND | constants.O_CREAT | constants.O_NOFOLLOW, 0o600);
    try {
      const st = fstatSync(fd);
      if (!st.isFile() || (me >= 0 && st.uid !== me) || (st.mode & 0o777) !== 0o600) throw new Error(`${f}: audit log file must be owned by me with 0600`);
      writeSync(fd, line + '\n');
    } finally {
      closeSync(fd);
    }
  }

  /** Report write failures to stderr only (e.g. failed sign-ins) */
  tryWrite(r: AuditRecord): void {
    try {
      this.write(r);
    } catch (e) {
      console.error(`Audit log write failed: ${(e as Error).message}`);
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
