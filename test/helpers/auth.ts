// 테스트용 계정과 로그인 쿠키.
import { addAccount, type Role } from '../../src/auth/accounts.ts';

export const TEST_PASSWORD = 'correct-horse-battery';

export async function makeAccount(outDir: string, username: string, role: Role): Promise<void> {
  await addAccount(outDir, username, role, TEST_PASSWORD);
}

/** 로그인해서 Cookie 헤더 값을 돌려준다 */
export async function login(base: string, username: string, password = TEST_PASSWORD): Promise<string> {
  const res = await fetch(`${base}/api/login`, {
    method: 'POST',
    headers: { Origin: base, 'X-Growth-Lab': '1', 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  });
  if (res.status !== 200) throw new Error(`로그인 실패 ${res.status}`);
  return res.headers.get('set-cookie')!.split(';')[0];
}
