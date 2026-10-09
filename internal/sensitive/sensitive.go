// Package sensitive finds sensitive-topic questions and secret-looking values (a secondary check).
package sensitive

import (
	"growth-lab/internal/jsstr"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var (
	cho  = []rune("ㄱㄲㄴㄷㄸㄹㅁㅂㅃㅅㅆㅇㅈㅉㅊㅋㅌㅍㅎ")
	jung = []rune("ㅏㅐㅑㅒㅓㅔㅕㅖㅗㅘㅙㅚㅛㅜㅝㅞㅟㅠㅡㅢㅣ")
	jong = []rune("ㄱㄲㄳㄴㄵㄶㄷㄹㄺㄻㄼㄽㄾㄿㅀㅁㅂㅄㅅㅆㅇㅈㅊㅋㅌㅍㅎ")
)

// NormalizeText splits Hangul into jamo (initial and final not distinguished), lowercases letters and drops spaces and symbols.
func NormalizeText(text string) string {
	var b strings.Builder
	for _, c := range norm.NFKD.String(text) {
		switch {
		case c >= 0x1100 && c <= 0x1112:
			b.WriteRune(cho[c-0x1100])
		case c >= 0x1161 && c <= 0x1175:
			b.WriteRune(jung[c-0x1161])
		case c >= 0x11a8 && c <= 0x11c2:
			b.WriteRune(jong[c-0x11a8])
		case unicode.IsLetter(c) || unicode.IsNumber(c):
			b.WriteString(strings.ToLower(string(c)))
		}
	}
	return b.String()
}

// topics: category → words, Korean and English
var topics = []struct {
	name  string
	words []string
}{
	{"credential", []string{"비밀번호", "패스워드", "암호", "password", "passwd", "토큰", "token", "세션토큰", "세션id", "sessionid", "sessiontoken", "세션쿠키", "쿠키값", "cookievalue", "api키", "apikey", "시크릿", "secret", "인증서", "certificate", "개인키", "privatekey", "해시", "hash", "솔트", "salt", "otp", "2단계인증", "mfa", "로그인정보", "credential", "bearer"}},
	{"account", []string{"계정정보", "관리자계정", "사용자계정", "권한정보", "접근권한", "권한목록", "역할목록", "관리자목록", "accountinfo"}},
	{"connection", []string{"접속정보", "연결정보", "접속주소", "연결문자열", "connectionstring", "dsn", "db계정", "db비밀번호", "db접속", "데이터베이스계정", "데이터베이스접속", "mcp", "커넥터", "connector", "ga4키", "서비스계정", "serviceaccount", "ssh", "호스트주소", "포트번호"}},
	{"config", []string{"환경변수", "env", "설정파일", "설정값", "workspacejson", "accountsjson", "시스템프롬프트", "systemprompt", "지침원문", "프롬프트원문"}},
	{"contact", []string{"이메일", "email", "전화번호", "휴대폰번호", "핸드폰번호", "phone", "집주소", "거주지", "주민등록", "ip주소", "ipaddress"}},
}

var normalized = func() [][]string {
	out := make([][]string, len(topics))
	for i, t := range topics {
		for _, w := range t.words {
			out[i] = append(out[i], NormalizeText(w))
		}
	}
	return out
}()

// Topic returns the matched category or "".
func Topic(text string) string {
	n := NormalizeText(text)
	for i, ws := range normalized {
		for _, w := range ws {
			if strings.Contains(n, w) {
				return topics[i].name
			}
		}
	}
	return ""
}

var shapes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{"bearer", regexp.MustCompile(`(?i)\bbearer` + jsstr.Space + `+[A-Za-z0-9._~+/-]{16,}`)},
	{"pem", regexp.MustCompile(`-----BEGIN [A-Z ]+-----`)},
	{"hex", regexp.MustCompile(`(?i)\b[0-9a-f]{40,}\b`)},
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)},
	{"url_credential", regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^` + jsstr.SpaceChars + `/:@]+:[^` + jsstr.SpaceChars + `/@]+@`)},
}

var (
	base64RE = regexp.MustCompile(`[A-Za-z0-9+/_-]{40,}={0,2}`)
	digitRE  = regexp.MustCompile(`\d`)
	upperRE  = regexp.MustCompile(`[A-Z]`)
	lowerRE  = regexp.MustCompile(`[a-z]`)
	assignRE = regexp.MustCompile(`(?i)\b(pass(word|wd)?|pwd|secret|token|api[_-]?key|credential|private[_-]?key)` + jsstr.Space + `*[:=]` + jsstr.Space + `*[^` + jsstr.SpaceChars + `]+`)
)

// SecretShape returns the kind of a secret-looking value (JWT, bearer, PEM, long hex/base64, email, URL with password) or "".
func SecretShape(text string) string {
	for _, s := range shapes {
		if s.re.MatchString(text) {
			return s.name
		}
	}
	// long base64: 40+ mixed letters and digits (long snake_case names excluded)
	for _, v := range base64RE.FindAllString(text, -1) {
		if digitRE.MatchString(v) && upperRE.MatchString(v) && lowerRE.MatchString(v) {
			return "base64"
		}
	}
	return ""
}

// SecretAssignment reports "key: value" or "key=value" whose key looks like a secret name.
func SecretAssignment(text string) bool { return assignRE.MatchString(text) }
