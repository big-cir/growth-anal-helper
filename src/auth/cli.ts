// 계정 관리 명령. 비밀번호는 명령줄 인자로 받지 않고 터미널에서 숨겨서 입력받는다.
import { closeSync, constants, existsSync, fstatSync, openSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { AuditLog } from './audit.ts';
import { AccountError, addAccount, checkPassword, hashPassword, modifyAccount, readAccounts, ROLES, type Role } from './accounts.ts';

/** 제어 터미널에서 입력을 화면에 보이지 않게 읽는다. 터미널이 아니면 거부. 어떤 식으로 끝나도 터미널을 되돌린다 */
export async function readHidden(prompt: string): Promise<string> {
  const input = process.stdin;
  if (!input.isTTY || typeof input.setRawMode !== 'function') throw new AccountError('비밀번호는 터미널에서만 입력할 수 있어요');
  process.stdout.write(prompt);
  const signals: NodeJS.Signals[] = ['SIGINT', 'SIGTERM', 'SIGHUP', 'SIGQUIT'];
  let onData: (buf: Buffer) => void = () => {};
  let onSignal: () => void = () => {};
  try {
    input.setRawMode(true);
    input.resume();
    return await new Promise<string>((resolve, reject) => {
      let value = '';
      onSignal = () => reject(new AccountError('취소됨'));
      for (const s of signals) process.once(s, onSignal);
      onData = (buf: Buffer) => {
        for (const ch of buf.toString('utf8')) {
          if (ch === '\r' || ch === '\n') return resolve(value);
          if (ch === '\u0003' || ch === '\u0004') return reject(new AccountError('취소됨'));
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
      // 이미 닫힘
    }
    input.pause();
    process.stdout.write('\n');
  }
}

async function askNewPassword(): Promise<string> {
  const a = await readHidden('새 비밀번호(12자 이상): ');
  const b = await readHidden('한 번 더: ');
  if (a !== b) throw new AccountError('두 입력이 달라요');
  return a;
}

const role = (v: string | undefined): Role => {
  if (!v || !ROLES.includes(v as Role)) throw new AccountError(`역할은 ${ROLES.join('|')}`);
  return v as Role;
};

/** 변경 전에 감사 기록(실패하면 변경하지 않음), 변경이 실패하면 *_failed */
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

export const ACCOUNT_USAGE = `account add <이름> --role viewer|editor|admin
  account passwd <이름>
  account role <이름> <역할>
  account disable <이름> | account enable <이름>
  account list
  account import          (산출물 폴더의 accounts.input.json에 적은 계정을 반영하고 비밀번호 칸을 비움)`;

export async function accountCommand(outDir: string, args: string[]): Promise<number> {
  const [sub, name, ...rest] = args;
  try {
    switch (sub) {
      case 'import': {
        const done = await importAccounts(outDir);
        console.log(done.length ? `반영했어요: ${done.join(', ')}. 입력 파일의 비밀번호 칸은 비웠어요` : '비밀번호가 적힌 계정이 없어요');
        return 0;
      }
      case 'list':
        for (const a of readAccounts(outDir)) console.log(`${a.username}\t${a.role}${a.disabled ? '\t(비활성)' : ''}`);
        return 0;
      case 'add': {
        const i = rest.indexOf('--role');
        const r = role(i >= 0 ? rest[i + 1] : undefined);
        if (!name) throw new AccountError('이름이 필요해요');
        const pw = await askNewPassword();
        await audited(outDir, name, () => addAccount(outDir, name, r, pw));
        console.log(`계정을 만들었어요: ${name} (${r})`);
        return 0;
      }
      case 'passwd': {
        if (!name) throw new AccountError('이름이 필요해요');
        const pw = await askNewPassword();
        const hash = await hashPassword(pw);
        await audited(outDir, name, () => modifyAccount(outDir, name, { hash }));
        console.log('비밀번호를 바꿨어요. 그 사용자의 로그인은 모두 끊겨요');
        return 0;
      }
      case 'role':
        if (!name) throw new AccountError('이름이 필요해요');
        await audited(outDir, name, () => modifyAccount(outDir, name, { role: role(rest[0]) }));
        console.log('역할을 바꿨어요. 그 사용자의 로그인은 모두 끊겨요');
        return 0;
      case 'disable':
      case 'enable':
        if (!name) throw new AccountError('이름이 필요해요');
        await audited(outDir, name, () => modifyAccount(outDir, name, { disabled: sub === 'disable' }));
        console.log(sub === 'disable' ? '계정을 껐어요. 그 사용자의 로그인은 모두 끊겨요' : '계정을 켰어요');
        return 0;
      default:
        console.error(`사용법:\n  ${ACCOUNT_USAGE}`);
        return 2;
    }
  } catch (e) {
    if (e instanceof AccountError) {
      console.error(`account 실패: ${e.message}`);
      return 1;
    }
    throw e;
  }
}

export const ACCOUNTS_INPUT = 'accounts.input.json';
type InputEntry = { username: string; role: Role; password: string };

/** 입력 파일: 내 소유 일반 파일, 0600, 심볼릭 링크 불가 */
function readInputFile(file: string): { accounts: InputEntry[] } {
  let fd: number;
  try {
    fd = openSync(file, constants.O_RDONLY | constants.O_NOFOLLOW);
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === 'ENOENT') throw new AccountError(`입력 파일이 없어요: ${file}`);
    throw new AccountError(`${file}: 열 수 없음(심볼릭 링크 불가)`);
  }
  try {
    const st = fstatSync(fd);
    const me = typeof process.getuid === 'function' ? process.getuid() : -1;
    if (!st.isFile() || (me >= 0 && st.uid !== me) || (st.mode & 0o077) !== 0) throw new AccountError(`${file}: 내 소유 0600 파일이어야 해요 (chmod 600)`);
    const raw = JSON.parse(readFileSync(fd, 'utf8')) as { accounts?: unknown };
    if (!Array.isArray(raw.accounts)) throw new AccountError(`${file}: { "accounts": [...] } 형식`);
    return { accounts: raw.accounts.map((a, i) => {
      const x = a as Record<string, unknown>;
      if (typeof x.username !== 'string' || typeof x.password !== 'string' || !ROLES.includes(x.role as Role)) throw new AccountError(`${file}: accounts[${i}]는 { username, role(viewer|editor|admin), password }`);
      return { username: x.username, role: x.role as Role, password: x.password };
    }) };
  } finally {
    closeSync(fd);
  }
}

/** 입력 파일에 비밀번호가 남아 있는지(서버 시작 전 확인) */
export function pendingInputPasswords(outDir: string): boolean {
  const file = join(outDir, ACCOUNTS_INPUT);
  if (!existsSync(file)) return false;
  try {
    return readInputFile(file).accounts.some((a) => a.password !== '');
  } catch {
    return true;
  }
}

/** 비밀번호가 적힌 계정만 반영(새 계정은 추가, 있는 계정은 역할·비밀번호 변경)하고, 파일의 비밀번호 칸을 비운다 */
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
