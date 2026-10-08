// mysql --batch 출력 한 줄 파싱.

export function unescapeField(s: string): string {
  if (!s.includes('\\')) return s;
  let out = '';
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    if (ch !== '\\') {
      out += ch;
      continue;
    }
    const nx = s[++i];
    if (nx === 't') out += '\t';
    else if (nx === 'n') out += '\n';
    else if (nx === '\\') out += '\\';
    else if (nx === '0') out += '\0';
    else throw new Error(`알 수 없는 이스케이프: \\${nx ?? '(끝)'}`);
  }
  return out;
}

/** `NULL`은 null */
export function parseLine(line: string): (string | null)[] {
  return line.split('\t').map((f) => (f === 'NULL' ? null : unescapeField(f)));
}
