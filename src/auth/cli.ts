// Account commands. Passwords are never taken as arguments; they are typed hidden in the terminal.
import { closeSync, constants, existsSync, fstatSync, openSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { AuditLog } from './audit.ts';
import { AccountError, addAccount, checkPassword, hashPassword, modifyAccount, readAccounts, ROLES, type Role } from './accounts.ts';

/** Read hidden input from the controlling terminal. Refuses without a terminal and always restores it */
export async function readHidden(prompt: string): Promise<string> {
  const input = process.stdin;
  if (!input.isTTY || typeof input.setRawMode !== 'function') throw new AccountError('Passwords can only be typed in a terminal');
  process.stdout.write(prompt);
  const signals: NodeJS.Signals[] = ['SIGINT', 'SIGTERM', 'SIGHUP', 'SIGQUIT'];
  let onData: (buf: Buffer) => void = () => {};
  let onSignal: () => void = () => {};
  try {
    input.setRawMode(true);
    input.resume();
    return await new Promise<string>((resolve, reject) => {
      let value = '';
      onSignal = () => reject(new AccountError('Cancelled'));
      for (const s of signals) process.once(s, onSignal);
      onData = (buf: Buffer) => {
        for (const ch of buf.toString('utf8')) {
          if (ch === '\r' || ch === '\n') return resolve(value);
          if (ch === '\u0003' || ch === '\u0004') return reject(new AccountError('Cancelled'));
          if (ch === '\u007f' || ch === '\b') value = [...value].slice(0, -1).join('');
          else if (ch >= ' ') value += ch;
        }
      };
      input.on('data', onData);
    });
  } finally {
    input.off('data', onData);
    for (const s of signals) process.off(s, onSignal);
    try {
      input.setRawMode(false);
    } catch {
      // already closed
    }
    input.pause();
    process.stdout.write('\n');
  }
}

async function askNewPassword(): Promise<string> {
  const a = await readHidden('New password (12+ characters): ');
  const b = await readHidden('Again: ');
  if (a !== b) throw new AccountError('The two entries differ');
  return a;
}

const role = (v: string | undefined): Role => {
  if (!v || !ROLES.includes(v as Role)) throw new AccountError(`Role must be ${ROLES.join('|')}`);
  return v as Role;
};

/** Audit before the change (no change if that fails); *_failed if the change fails */
export async function audited(outDir: string, target: string, fn: () => Promise<void> | void): Promise<void> {
  const log = new AuditLog(outDir);
  const user = process.env.USER ?? 'cli';
  log.write({ event: 'account_changed', user, target });
  try {
    await fn();
  } catch (e) {
    log.tryWrite({ event: 'account_changed_failed', user, target });
    throw e;
  }
}

export const ACCOUNT_USAGE = `account add <name> --role viewer|editor|admin
  account passwd <name>
  account role <name> <role>
  account disable <name> | account enable <name>
  account list
  account import          (apply accounts from accounts.input.json in the output folder and clear its passwords)`;

export async function accountCommand(outDir: string, args: string[]): Promise<number> {
  const [sub, name, ...rest] = args;
  try {
    switch (sub) {
      case 'import': {
        const done = await importAccounts(outDir);
        console.log(done.length ? `Applied: ${done.join(', ')}. Passwords in the input file were cleared` : 'No accounts with a password in the file');
        return 0;
      }
      case 'list':
        for (const a of readAccounts(outDir)) console.log(`${a.username}\t${a.role}${a.disabled ? '\t(disabled)' : ''}`);
        return 0;
      case 'add': {
        const i = rest.indexOf('--role');
        const r = role(i >= 0 ? rest[i + 1] : undefined);
        if (!name) throw new AccountError('A name is required');
        const pw = await askNewPassword();
        await audited(outDir, name, () => addAccount(outDir, name, r, pw));
        console.log(`Account created: ${name} (${r})`);
        return 0;
      }
      case 'passwd': {
        if (!name) throw new AccountError('A name is required');
        const pw = await askNewPassword();
        const hash = await hashPassword(pw);
        await audited(outDir, name, () => modifyAccount(outDir, name, { hash }));
        console.log('Password changed. All of that user\'s sessions are signed out');
        return 0;
      }
      case 'role':
        if (!name) throw new AccountError('A name is required');
        await audited(outDir, name, () => modifyAccount(outDir, name, { role: role(rest[0]) }));
        console.log('Role changed. All of that user\'s sessions are signed out');
        return 0;
      case 'disable':
      case 'enable':
        if (!name) throw new AccountError('A name is required');
        await audited(outDir, name, () => modifyAccount(outDir, name, { disabled: sub === 'disable' }));
        console.log(sub === 'disable' ? 'Account disabled. All of that user\'s sessions are signed out' : 'Account enabled');
        return 0;
      default:
        console.error(`Usage:\n  ${ACCOUNT_USAGE}`);
        return 2;
    }
  } catch (e) {
    if (e instanceof AccountError) {
      console.error(`account failed: ${e.message}`);
      return 1;
    }
    throw e;
  }
}

export const ACCOUNTS_INPUT = 'accounts.input.json';
type InputEntry = { username: string; role: Role; password: string };

/** Input file: regular file owned by me, 0600, no symlinks */
function readInputFile(file: string): { accounts: InputEntry[] } {
  let fd: number;
  try {
    fd = openSync(file, constants.O_RDONLY | constants.O_NOFOLLOW);
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === 'ENOENT') throw new AccountError(`Input file not found: ${file}`);
    throw new AccountError(`${file}: cannot open (symlinks not allowed)`);
  }
  try {
    const st = fstatSync(fd);
    const me = typeof process.getuid === 'function' ? process.getuid() : -1;
    if (!st.isFile() || (me >= 0 && st.uid !== me) || (st.mode & 0o077) !== 0) throw new AccountError(`${file}: must be a file owned by me with 0600 (chmod 600)`);
    const raw = JSON.parse(readFileSync(fd, 'utf8')) as { accounts?: unknown };
    if (!Array.isArray(raw.accounts)) throw new AccountError(`${file}: expected { "accounts": [...] }`);
    return { accounts: raw.accounts.map((a, i) => {
      const x = a as Record<string, unknown>;
      if (typeof x.username !== 'string' || typeof x.password !== 'string' || !ROLES.includes(x.role as Role)) throw new AccountError(`${file}: accounts[${i}] must be { username, role (viewer|editor|admin), password }`);
      return { username: x.username, role: x.role as Role, password: x.password };
    }) };
  } finally {
    closeSync(fd);
  }
}

/** Whether the input file still has passwords (checked before the server starts) */
export function pendingInputPasswords(outDir: string): boolean {
  const file = join(outDir, ACCOUNTS_INPUT);
  if (!existsSync(file)) return false;
  try {
    return readInputFile(file).accounts.some((a) => a.password !== '');
  } catch {
    return true;
  }
}

/** Apply only accounts with a password (new ones are added, existing ones get role and password updated), then clear the passwords in the file */
export async function importAccounts(outDir: string): Promise<string[]> {
  const file = join(outDir, ACCOUNTS_INPUT);
  const input = readInputFile(file);
  const todo = input.accounts.filter((a) => a.password !== '');
  for (const a of todo) checkPassword(a.password);
  const done: string[] = [];
  for (const a of todo) {
    const exists = readAccounts(outDir).some((x) => x.username === a.username);
    if (exists) {
      const hash = await hashPassword(a.password);
      await audited(outDir, a.username, () => modifyAccount(outDir, a.username, { hash, role: a.role }));
    } else {
      await audited(outDir, a.username, () => addAccount(outDir, a.username, a.role, a.password));
    }
    done.push(`${a.username}(${a.role})`);
  }
  const cleared = { accounts: input.accounts.map((a) => ({ username: a.username, role: a.role, password: '' })) };
  const tmp = `${file}.${process.pid}.tmp`;
  writeFileSync(tmp, JSON.stringify(cleared, null, 2) + '\n', { mode: 0o600 });
  renameSync(tmp, file);
  return done;
}
