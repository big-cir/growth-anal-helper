// 민감해 보이는 이름 규칙. 걸리는 칸·표는 private로만 수집한다.

/** 이름을 낱말로 나눈다: snake·kebab·camelCase */
function tokens(name: string): string[] {
  return name.replace(/([a-z0-9])([A-Z])/g, '$1_$2').toLowerCase().split(/[^a-z0-9]+/).filter(Boolean);
}

/** 구분자 없이 붙어 있어도 잡는 강한 낱말 */
const STRONG = ['password', 'passwd', 'secret', 'apikey', 'privatekey', 'credential', 'accesstoken', 'refreshtoken', 'deviceid', 'ipaddr', 'emailaddr', 'phonenumber'];
/** 낱말 하나로 있을 때 잡는 것 */
const WORD = new Set(['pass', 'pwd', 'token', 'salt', 'hash', 'otp', 'mfa', 'totp', 'bearer', 'oauth', 'cookie', 'signature', 'email', 'phone', 'mobile', 'address', 'ip']);
/** 다른 낱말과 함께일 때만(session_count 같은 분석 칸은 허용) */
const PAIRED: Record<string, string[]> = { session: ['id', 'key', 'token', 'cookie', 'secret'], push: ['token', 'id', 'key'] };

/** 걸린 규칙 이름 또는 null */
export function sensitiveName(name: string): string | null {
  const flat = name.toLowerCase().replace(/[^a-z0-9]/g, '');
  for (const w of STRONG) if (flat.includes(w)) return w;
  const ts = tokens(name);
  for (const t of ts) if (WORD.has(t)) return t;
  for (const [w, with_] of Object.entries(PAIRED)) if (ts.includes(w) && ts.some((t) => with_.includes(t))) return w;
  return null;
}
