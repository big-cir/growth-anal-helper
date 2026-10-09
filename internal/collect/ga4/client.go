package ga4

// GA4 Data API calls: gets a token with a service account JWT, then calls runReport and checkCompatibility.
// Endpoints are fixed. Errors carry only the kind and status code (no URL, headers, body, key or token).

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"growth-lab/internal/jsjson"
)

// Fixed endpoints.
const (
	TokenURL = "https://oauth2.googleapis.com/token"
	APIBase  = "https://analyticsdata.googleapis.com/v1beta"
	Scope    = "https://www.googleapis.com/auth/analytics.readonly"
)

// ClientLimits bound one collect.
var ClientLimits = struct {
	TimeoutMs, BodyBytes, Retries, MaxCalls int
	QuotaMinShare                           float64
	MaxRetryWaitMs                          int
}{TimeoutMs: 60_000, BodyBytes: 50 * 1024 * 1024, Retries: 3, MaxCalls: 200, QuotaMinShare: 0.1, MaxRetryWaitMs: 60_000}

var messages = map[string]string{
	"auth":             "GA4 authentication failed",
	"quota":            "GA4 quota exhausted",
	"http_4xx":         "GA4 request rejected",
	"http_5xx":         "GA4 server error",
	"timeout":          "GA4 response timed out",
	"invalid_response": "unexpected GA4 response format",
	"limit":            "GA4 call limit reached",
	"incompatible":     "incompatible GA4 dimensions and metrics",
}

// Error is a GA4 call failure; Status is 0 when there is no HTTP status.
type Error struct {
	Kind   string
	Status int
	msg    string
}

func (e *Error) Error() string { return e.msg }

// NewError builds the message from the kind, status and place.
func NewError(kind string, status int, where string) *Error {
	msg := messages[kind]
	if status != 0 {
		msg += fmt.Sprintf(" (HTTP %d)", status)
	}
	if where != "" {
		msg += " — " + where
	}
	return &Error{Kind: kind, Status: status, msg: msg}
}

// HTTPRequest is one call.
type HTTPRequest struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    string
}

// HTTPResponse has lowercase header names.
type HTTPResponse struct {
	Status  int
	Headers map[string]string
	Body    string
}

// Transport sends one request.
type Transport func(HTTPRequest) (HTTPResponse, error)

// HTTPSTransport: no redirects, timeout, body size limit. Errors keep only the kind.
func HTTPSTransport(req HTTPRequest) (HTTPResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(ClientLimits.TimeoutMs)*time.Millisecond)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, req.Method, req.URL, strings.NewReader(req.Body))
	if err != nil {
		return HTTPResponse{}, NewError("http_5xx", 0, "")
	}
	for k, v := range req.Headers {
		r.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect") }}
	res, err := client.Do(r)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return HTTPResponse{}, NewError("timeout", 0, "")
		}
		return HTTPResponse{}, NewError("http_5xx", 0, "")
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(res.Body, int64(ClientLimits.BodyBytes)+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return HTTPResponse{}, NewError("timeout", 0, "")
		}
		return HTTPResponse{}, NewError("http_5xx", 0, "")
	}
	if n > int64(ClientLimits.BodyBytes) {
		return HTTPResponse{}, NewError("invalid_response", res.StatusCode, "response too large")
	}
	headers := map[string]string{}
	for k, v := range res.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	return HTTPResponse{Status: res.StatusCode, Headers: headers, Body: strings.ToValidUTF8(buf.String(), "�")}, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// SignedAssertion is the service account JWT (RS256) with fixed claims.
func SignedAssertion(cred *Credentials, nowSec int64) (string, error) {
	header := b64url([]byte(jsjson.MustStringify(jsjson.Object{{Key: "alg", Value: "RS256"}, {Key: "typ", Value: "JWT"}})))
	claims := b64url([]byte(jsjson.MustStringify(jsjson.Object{{Key: "iss", Value: cred.ClientEmail}, {Key: "scope", Value: Scope}, {Key: "aud", Value: TokenURL}, {Key: "iat", Value: nowSec}, {Key: "exp", Value: nowSec + 3600}})))
	sum := sha256.Sum256([]byte(header + "." + claims))
	sig, err := rsa.SignPKCS1v15(nil, cred.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return header + "." + claims + "." + b64url(sig), nil
}

// ClientOptions replace the transport, sleep and clock (for tests).
type ClientOptions struct {
	Transport Transport
	Sleep     func(ms int)
	Now       func() int64
	// Jitter returns a random integer below n
	Jitter func(n int) int
}

// Client calls the GA4 Data API for one property.
type Client struct {
	cred       *Credentials
	propertyID string
	o          ClientOptions
	token      string
	until      int64
	Calls      int
}

var propertyIDRE = regexp.MustCompile(`^\d{1,20}$`)

// NewClient checks the property ID and fills default options.
func NewClient(cred *Credentials, propertyID string, o ClientOptions) (*Client, error) {
	if !propertyIDRE.MatchString(propertyID) {
		return nil, NewError("invalid_response", 0, "property_id format")
	}
	if o.Transport == nil {
		o.Transport = HTTPSTransport
	}
	if o.Sleep == nil {
		o.Sleep = func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
	}
	if o.Now == nil {
		o.Now = func() int64 { return time.Now().UnixMilli() }
	}
	if o.Jitter == nil {
		o.Jitter = func(n int) int {
			v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
			return int(v.Int64())
		}
	}
	return &Client{cred: cred, propertyID: propertyID, o: o}, nil
}

// send applies the call limit and retries 429 and 5xx (exponential backoff with jitter, Retry-After).
func (c *Client) send(req HTTPRequest, where string) (HTTPResponse, error) {
	for attempt := 0; ; attempt++ {
		if c.Calls >= ClientLimits.MaxCalls {
			return HTTPResponse{}, NewError("limit", 0, where)
		}
		c.Calls++
		res, err := c.o.Transport(req)
		if err != nil {
			var ge *Error
			if !errors.As(err, &ge) {
				ge = NewError("http_5xx", 0, where)
			}
			if ge.Kind != "timeout" && ge.Kind != "http_5xx" {
				return HTTPResponse{}, NewError(ge.Kind, ge.Status, where)
			}
			if attempt >= ClientLimits.Retries {
				return HTTPResponse{}, NewError(ge.Kind, ge.Status, where)
			}
			c.o.Sleep(1000*(1<<attempt) + c.o.Jitter(250))
			continue
		}
		if res.Status == 429 || res.Status >= 500 {
			if attempt >= ClientLimits.Retries {
				kind := "http_5xx"
				if res.Status == 429 {
					kind = "quota"
				}
				return HTTPResponse{}, NewError(kind, res.Status, where)
			}
			ra := jsNumber(retryAfter(res.Headers))
			wait := 0
			if !math.IsNaN(ra) && !math.IsInf(ra, 0) && ra > 0 {
				wait = int(math.Min(ra*1000, float64(ClientLimits.MaxRetryWaitMs)))
			} else {
				wait = 1000*(1<<attempt) + c.o.Jitter(250)
			}
			c.o.Sleep(wait)
			continue
		}
		if res.Status == 401 || res.Status == 403 {
			return HTTPResponse{}, NewError("auth", res.Status, where)
		}
		if res.Status >= 400 {
			return HTTPResponse{}, NewError("http_4xx", res.Status, where)
		}
		return res, nil
	}
}

// retryAfter is Number(headers['retry-after']): a missing header reads as NaN.
func retryAfter(h map[string]string) string {
	if v, ok := h["retry-after"]; ok {
		return v
	}
	return "NaN"
}

// formEncode serializes like URLSearchParams.
func formEncode(pairs [][2]string) string {
	esc := func(s string) string {
		var b strings.Builder
		for _, c := range []byte(s) {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '*', c == '-', c == '.', c == '_':
				b.WriteByte(c)
			case c == ' ':
				b.WriteByte('+')
			default:
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
		return b.String()
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = esc(p[0]) + "=" + esc(p[1])
	}
	return strings.Join(parts, "&")
}

func (c *Client) accessToken() (string, error) {
	if c.token != "" && c.until > c.o.Now()+60_000 {
		return c.token, nil
	}
	assertion, err := SignedAssertion(c.cred, int64(math.Floor(float64(c.o.Now())/1000)))
	if err != nil {
		return "", err
	}
	body := formEncode([][2]string{{"grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer"}, {"assertion", assertion}})
	res, err := c.send(HTTPRequest{URL: TokenURL, Method: "POST", Headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, Body: body}, "token")
	if err != nil {
		return "", err
	}
	v, perr := jsjson.Parse(res.Body)
	if perr != nil {
		return "", NewError("auth", res.Status, "token response")
	}
	if v == nil {
		return "", errors.New("Cannot read properties of null (reading 'token_type')")
	}
	j, _ := v.(jsjson.Object)
	tok, tokOK := prop(j, "access_token").(string)
	exp, expOK := prop(j, "expires_in").(float64)
	if prop(j, "token_type") != "Bearer" || !tokOK || tok == "" || !expOK || exp != math.Trunc(exp) || math.Abs(exp) > 1<<53-1 || exp <= 0 {
		return "", NewError("auth", res.Status, "token response")
	}
	c.token = tok
	c.until = c.o.Now() + int64(exp)*1000
	return tok, nil
}

func (c *Client) post(method string, payload any, where string) (jsjson.Object, error) {
	token, err := c.accessToken()
	if err != nil {
		return nil, err
	}
	res, err := c.send(HTTPRequest{
		URL:     APIBase + "/properties/" + c.propertyID + ":" + method,
		Method:  "POST",
		Headers: map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"},
		Body:    jsjson.MustStringify(payload),
	}, where)
	if err != nil {
		return nil, err
	}
	v, perr := jsjson.Parse(res.Body)
	j, ok := v.(jsjson.Object)
	if perr != nil || !ok {
		return nil, NewError("invalid_response", res.Status, where)
	}
	return j, nil
}

// RunReport calls runReport.
func (c *Client) RunReport(payload any, where string) (jsjson.Object, error) {
	return c.post("runReport", payload, where)
}

// CheckCompatibility calls checkCompatibility.
func (c *Client) CheckCompatibility(payload any, where string) (jsjson.Object, error) {
	return c.post("checkCompatibility", payload, where)
}
