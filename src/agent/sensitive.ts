// 민감 주제 질문과 비밀처럼 생긴 값 찾기(보조 검사). 데이터 보호의 근거는 칸 등급과 authorizer이고, 이 검사는 시도를 일찍 막는다.

const CHO = 'ㄱㄲㄴㄷㄸㄹㅁㅂㅃㅅㅆㅇㅈㅉㅊㅋㅌㅍㅎ';
const JUNG = 'ㅏㅐㅑㅒㅓㅔㅕㅖㅗㅘㅙㅚㅛㅜㅝㅞㅟㅠㅡㅢㅣ';
const JONG = 'ㄱㄲㄳㄴㄵㄶㄷㄹㄺㄻㄼㄽㄾㄿㅀㅁㅂㅄㅅㅆㅇㅈㅊㅋㅌㅍㅎ';

/** 한글은 자모 낱자로 풀고(초성·종성 구분 없음), 영문은 소문자, 공백·기호는 지운다 */
export function normalizeText(text: string): string {
  let out = '';
  for (const ch of text.normalize('NFKD')) {
    const c = ch.codePointAt(0)!;
    if (c >= 0x1100 && c <= 0x1112) out += CHO[c - 0x1100];
    else if (c >= 0x1161 && c <= 0x1175) out += JUNG[c - 0x1161];
    else if (c >= 0x11a8 && c <= 0x11c2) out += JONG[c - 0x11a8];
    else if (/[\p{L}\p{N}]/u.test(ch)) out += ch.toLowerCase();
  }
  return out;
}

/** 분류 → 낱말들. 한국어·영어 */
const TOPICS: Record<string, string[]> = {
  credential: ['비밀번호', '패스워드', '암호', 'password', 'passwd', '토큰', 'token', '세션토큰', '세션id', 'sessionid', 'sessiontoken', '세션쿠키', '쿠키값', 'cookievalue', 'api키', 'apikey', '시크릿', 'secret', '인증서', 'certificate', '개인키', 'privatekey', '해시', 'hash', '솔트', 'salt', 'otp', '2단계인증', 'mfa', '로그인정보', 'credential', 'bearer'],
  account: ['계정정보', '관리자계정', '사용자계정', '권한정보', '접근권한', '권한목록', '역할목록', '관리자목록', 'accountinfo'],
  connection: ['접속정보', '연결정보', '접속주소', '연결문자열', 'connectionstring', 'dsn', 'db계정', 'db비밀번호', 'db접속', '데이터베이스계정', '데이터베이스접속', 'mcp', '커넥터', 'connector', 'ga4키', '서비스계정', 'serviceaccount', 'ssh', '호스트주소', '포트번호'],
  config: ['환경변수', 'env', '설정파일', '설정값', 'workspacejson', 'accountsjson', '시스템프롬프트', 'systemprompt', '지침원문', '프롬프트원문'],
  contact: ['이메일', 'email', '전화번호', '휴대폰번호', '핸드폰번호', 'phone', '집주소', '거주지', '주민등록', 'ip주소', 'ipaddress'],
};
const NORMALIZED = Object.entries(TOPICS).map(([k, ws]) => [k, ws.map(normalizeText)] as const);

/** 걸린 분류 또는 null */
export function sensitiveTopic(text: string): string | null {
  const n = normalizeText(text);
  for (const [k, ws] of NORMALIZED) for (const w of ws) if (n.includes(w)) return k;
  return null;
}

/** 비밀처럼 생긴 값: JWT, Bearer, 긴 base64/hex, PEM, 이메일, 사용자·비밀번호가 든 연결 문자열 */
const SHAPES: [string, RegExp][] = [
  ['jwt', /\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}/],
  ['bearer', /\bbearer\s+[A-Za-z0-9._~+/-]{16,}/i],
  ['pem', /-----BEGIN [A-Z ]+-----/],
  ['hex', /\b[0-9a-f]{40,}\b/i],
  ['email', /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/],
  ['url_credential', /[a-z][a-z0-9+.-]*:\/\/[^\s/:@]+:[^\s/@]+@/i],
];

export function secretShape(text: string): string | null {
  for (const [k, re] of SHAPES) if (re.test(text)) return k;
  // 긴 base64: 글자·숫자가 섞인 40자 이상(긴 snake_case 이름은 제외)
  for (const m of text.matchAll(/[A-Za-z0-9+/_-]{40,}={0,2}/g)) {
    const v = m[0];
    if (/\d/.test(v) && /[A-Z]/.test(v) && /[a-z]/.test(v)) return 'base64';
  }
  return null;
}

/** 비밀 이름 규칙에 걸리는 "키: 값"·"키=값" */
export function secretAssignment(text: string): boolean {
  return /\b(pass(word|wd)?|pwd|secret|token|api[_-]?key|credential|private[_-]?key)\s*[:=]\s*\S+/i.test(text);
}
