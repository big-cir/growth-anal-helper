// Sensitive-looking name rules. Matching columns and tables can only be collected as private.

/** Splits a name into words: snake, kebab, camelCase */
function tokens(name: string): string[] {
  return name.replace(/([a-z0-9])([A-Z])/g, '$1_$2').toLowerCase().split(/[^a-z0-9]+/).filter(Boolean);
}

/** Strong words, matched even without separators */
const STRONG = ['password', 'passwd', 'secret', 'apikey', 'privatekey', 'credential', 'accesstoken', 'refreshtoken', 'deviceid', 'ipaddr', 'emailaddr', 'phonenumber'];
/** Matched when the whole name is this one word */
const WORD = new Set(['pass', 'pwd', 'token', 'salt', 'hash', 'otp', 'mfa', 'totp', 'bearer', 'oauth', 'cookie', 'signature', 'email', 'phone', 'mobile', 'address', 'ip']);
/** Matched only together with other words (analytics columns like session_count are allowed) */
const PAIRED: Record<string, string[]> = { session: ['id', 'key', 'token', 'cookie', 'secret'], push: ['token', 'id', 'key'] };

/** Name of the matched rule, or null */
export function sensitiveName(name: string): string | null {
  const flat = name.toLowerCase().replace(/[^a-z0-9]/g, '');
  for (const w of STRONG) if (flat.includes(w)) return w;
  const ts = tokens(name);
  for (const t of ts) if (WORD.has(t)) return t;
  for (const [w, with_] of Object.entries(PAIRED)) if (ts.includes(w) && ts.some((t) => with_.includes(t))) return w;
  return null;
}
