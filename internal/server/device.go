package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"regexp"
)

// DeviceCookie keeps conversations apart between browsers that share an account.
const DeviceCookie = "gl_device"

var deviceRE = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// device returns the hashed device id of the request, issuing a new cookie when it has none.
func (s *Server) device(w http.ResponseWriter, r *http.Request) string {
	raw := ""
	if ck, err := r.Cookie(DeviceCookie); err == nil && deviceRE.MatchString(ck.Value) {
		raw = ck.Value
	} else {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		raw = base64.RawURLEncoding.EncodeToString(b)
		secure := ""
		if s.external != "" {
			secure = "; Secure"
		}
		w.Header().Add("Set-Cookie", DeviceCookie+"="+raw+"; HttpOnly; SameSite=Strict; Path=/; Max-Age=34560000"+secure)
	}
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:8])
}
