// 시각은 `YYYY-MM-DD HH:MM:SS.ffffff`(26자)로 맞춰 문자열 비교 = 시간 비교가 되게 한다. 시간대 변환은 하지 않는다.

const TS_RE = /^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,6}))?$/;
const DATE_RE = /^(\d{4})-(\d{2})-(\d{2})$/;

type Parts = { y: number; mo: number; d: number; h: number; mi: number; s: number; frac: string };

function parse(value: string): Parts {
  const m = TS_RE.exec(value) ?? (DATE_RE.test(value) ? TS_RE.exec(`${value} 00:00:00`) : null);
  if (!m) throw new Error(`시각 형식이 아님: ${JSON.stringify(value)}`);
  const p: Parts = {
    y: Number(m[1]), mo: Number(m[2]), d: Number(m[3]),
    h: Number(m[4]), mi: Number(m[5]), s: Number(m[6]),
    frac: (m[7] ?? '').padEnd(6, '0'),
  };
  if (p.y < 1000) throw new Error(`지원하지 않는 연도(1000 이상): ${JSON.stringify(value)}`);
  const check = new Date(Date.UTC(p.y, p.mo - 1, p.d));
  if (check.getUTCFullYear() !== p.y || check.getUTCMonth() !== p.mo - 1 || check.getUTCDate() !== p.d) {
    throw new Error(`없는 날짜: ${JSON.stringify(value)}`);
  }
  if (p.h > 23 || p.mi > 59 || p.s > 59) throw new Error(`없는 시각: ${JSON.stringify(value)}`);
  return p;
}

const pad = (n: number, w = 2) => String(n).padStart(w, '0');

function format(p: Parts): string {
  if (p.y < 1000 || p.y > 9999) throw new Error(`지원 범위(1000~9999년) 밖의 날짜: ${p.y}`);
  return `${pad(p.y, 4)}-${pad(p.mo)}-${pad(p.d)} ${pad(p.h)}:${pad(p.mi)}:${pad(p.s)}.${p.frac}`;
}

/** 26자 형식으로 맞춘다. 날짜만 있으면 00:00:00을 붙인다 */
export function normalizeTs(value: string): string {
  return format(parse(value));
}

export function isNormalizedTs(value: string): boolean {
  if (value.length !== 26) return false;
  try { return normalizeTs(value) === value; } catch { return false; }
}

function fromDayNumber(days: number, rest: Omit<Parts, 'y' | 'mo' | 'd'>): Parts {
  const dt = new Date(days * 86_400_000);
  return { y: dt.getUTCFullYear(), mo: dt.getUTCMonth() + 1, d: dt.getUTCDate(), ...rest };
}

function dayNumber(p: Parts): number {
  return Math.floor(Date.UTC(p.y, p.mo - 1, p.d) / 86_400_000);
}

export function addDays(value: string, n: number): string {
  if (!Number.isInteger(n)) throw new Error(`정수 일수가 아님: ${n}`);
  const p = parse(value);
  return format(fromDayNumber(dayNumber(p) + n, { h: p.h, mi: p.mi, s: p.s, frac: p.frac }));
}

/** 그 주의 월요일 00:00 */
export function weekStart(value: string): string {
  const p = parse(value);
  const days = dayNumber(p);
  const dow = (new Date(days * 86_400_000).getUTCDay() + 6) % 7;
  return format(fromDayNumber(days - dow, { h: 0, mi: 0, s: 0, frac: '000000' }));
}
