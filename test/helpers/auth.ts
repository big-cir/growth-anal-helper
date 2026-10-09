// Test accounts and sign-in cookies.
import { addAccount, type Role } from '../../src/auth/accounts.ts';

export const TEST_PASSWORD = 'correct-horse-battery';

export async function makeAccount(outDir: string, username: string, role: Role): Promise<void> {
  await addAccount(outDir, username, role, TEST_PASSWORD);
}

/** Sign in and return the Cookie header value */
export async function login(base: string, username: string, password = TEST_PASSWORD): Promise<string> {
  const res = await fetch(`${base}/api/login`, {
    method: 'POST',
    headers: { Origin: base, 'X-Growth-Lab': '1', 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  });
  if (res.status !== 200) throw new Error(`sign-in failed ${res.status}`);
  return res.headers.get('set-cookie')!.split(';')[0];
}
