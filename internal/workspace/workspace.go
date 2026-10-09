// Package workspace finds the workspace and validates workspace.json.
package workspace

import (
	"fmt"
	"growth-lab/internal/jsstr"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/idna"

	"growth-lab/internal/civiltime"
	"growth-lab/internal/collect/ga4"
	"growth-lab/internal/jsjson"
)

// Datasource is a server database or a SQLite file.
type Datasource struct {
	Kind     string // mysql, postgres, sqlite
	Host     string
	Port     int
	User     string
	Password string
	Database string
	Path     string // sqlite
}

// Pricing is dollars per million tokens.
type Pricing struct{ InputPerMTok, OutputPerMTok float64 }

// Agent is the agent configuration.
type Agent struct {
	Provider        string
	Bin             string
	Model           *string
	CallBudgetUsd   float64
	RequestBudget   float64
	MaxTurns        int
	MaxProbes       int
	MaxFixes        int
	CallTimeoutMs   int
	Concurrency     int
	DataMode        string
	APIKey          string
	BaseURL         *string
	Pricing         *Pricing
	MaxOutputTokens int
}

// Server is the HTTP server configuration.
type Server struct {
	Port               int
	Auth               bool
	PublicOrigin       *string
	ProxyHops          int
	AuditRetentionDays int
}

// Config is workspace.json after validation.
type Config struct {
	Name             string
	Language         string
	Datasource       Datasource
	ReadablePrefixes []string
	PanelPrefixes    []string
	Params           jsjson.Object // values: float64, string, []string
	Agent            Agent
	HeapLimitMb      int
	Server           Server
	OutDir           string
	GA4              *ga4.Connection
}

// Workspace is a folder and its config.
type Workspace struct {
	Dir    string
	Config Config
}

// ConfigError is a workspace.json error.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return e.Msg }

func defaultAgent() Agent {
	return Agent{Provider: "claude-code", Bin: "claude", CallBudgetUsd: 0.5, RequestBudget: 1.0, MaxTurns: 8, MaxProbes: 4, MaxFixes: 2, CallTimeoutMs: 90000, Concurrency: 2, DataMode: "pseudonymized", MaxOutputTokens: 8192}
}

var agentKeys = []string{"provider", "bin", "model", "callBudgetUsd", "requestBudgetUsd", "maxTurns", "maxProbes", "maxFixes", "callTimeoutMs", "concurrency", "dataMode", "apiKey", "baseUrl", "pricing", "maxOutputTokens"}

// FindDir returns GROWTH_LAB_WORKSPACE (relative to cwd), or ./workspace.
func FindDir(getenv func(string) string, cwd string) string {
	if v := getenv("GROWTH_LAB_WORKSPACE"); v != "" {
		return resolve(cwd, v)
	}
	return resolve(cwd, "workspace")
}

func resolve(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

var identPrefixRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type failure struct{ err error }

func fail(path, msg string) { panic(failure{&ConfigError{"workspace.json " + path + ": " + msg}}) }

func catch(err *error) {
	if r := recover(); r != nil {
		f, ok := r.(failure)
		if !ok {
			panic(r)
		}
		*err = f.err
	}
}

func obj(v any, path string) jsjson.Object {
	o, ok := v.(jsjson.Object)
	if !ok {
		fail(path, "must be an object")
	}
	return o
}

func str(v any, path string) string {
	s, ok := v.(string)
	if !ok || s == "" {
		fail(path, "must be a non-empty string")
	}
	return s
}

func strArray(v any, path string, nonEmpty bool) []string {
	arr, ok := v.([]any)
	if !ok {
		fail(path, "must be an array of strings")
	}
	out := make([]string, len(arr))
	for i, x := range arr {
		s, ok := x.(string)
		if !ok {
			fail(path, "must be an array of strings")
		}
		out[i] = s
	}
	if nonEmpty && len(out) == 0 {
		fail(path, "must not be empty")
	}
	return out
}

func num(v any, path string, isInt bool, min float64, exclusiveMin bool) float64 {
	f, ok := v.(float64)
	if !ok || math.IsInf(f, 0) || math.IsNaN(f) {
		fail(path, "must be a number")
	}
	if isInt && f != math.Trunc(f) {
		fail(path, "must be an integer")
	}
	if (exclusiveMin && f <= min) || (!exclusiveMin && f < min) {
		op := ">="
		if exclusiveMin {
			op = ">"
		}
		fail(path, "must be "+op+" "+jsjson.Number(min))
	}
	return f
}

func onlyKeys(o jsjson.Object, allowed []string, path string) {
	for _, m := range o {
		ok := false
		for _, a := range allowed {
			if a == m.Key {
				ok = true
			}
		}
		if !ok {
			fail(path+"."+m.Key, "unknown key")
		}
	}
}

func get(o jsjson.Object, k string) (any, bool) { return o.Get(k) }

// paramValue allows a timestamp or "timestamp~timestamp" only.
func paramValue(v, path string) string {
	parts := strings.Split(v, "~")
	if len(parts) > 2 {
		fail(path, `only numbers, timestamps or "timestamp~timestamp"`)
	}
	for _, p := range parts {
		if _, err := civiltime.Normalize(jsstr.Trim(p)); err != nil {
			fail(path, `only numbers, timestamps or "timestamp~timestamp" (no free text)`)
		}
	}
	return v
}

// ExpandHome expands a leading `~/` or `=~/` to the home directory.
func ExpandHome(arg, home string) string {
	if strings.HasPrefix(arg, "~/") {
		return filepath.Join(home, arg[2:])
	}
	if i := strings.Index(arg, "=~/"); i >= 0 {
		return arg[:i+1] + filepath.Join(home, arg[i+3:])
	}
	return arg
}

// ParseConfig validates workspace.json content (decoded by jsjson.Parse).
func ParseConfig(raw any, dir, home string) (cfg Config, err error) {
	defer catch(&err)
	root := obj(raw, "(root)")
	onlyKeys(root, []string{"name", "language", "datasource", "policy", "params", "agent", "run", "server", "outDir", "ga4"}, "")

	dsv, _ := get(root, "datasource")
	datasource := parseDatasource(obj(dsv, ".datasource"), dir, home)

	polv, _ := get(root, "policy")
	pol := obj(polv, ".policy")
	onlyKeys(pol, []string{"readablePrefixes", "panelReadablePrefixes"}, ".policy")
	rpv, _ := get(pol, "readablePrefixes")
	readable := strArray(rpv, ".policy.readablePrefixes", true)
	for _, p := range readable {
		if !identPrefixRE.MatchString(p) {
			fail(".policy.readablePrefixes", "not an identifier prefix: "+jsjson.QuoteString(p))
		}
		if strings.HasPrefix(strings.ToLower(p), "sqlite_") {
			fail(".policy.readablePrefixes", "sqlite_ internal tables are not allowed")
		}
	}
	panel := []string{"d_"}
	if v, ok := get(pol, "panelReadablePrefixes"); ok {
		panel = strArray(v, ".policy.panelReadablePrefixes", true)
	}
	for _, p := range panel {
		if !identPrefixRE.MatchString(p) {
			fail(".policy.panelReadablePrefixes", "not an identifier prefix: "+jsjson.QuoteString(p))
		}
		wider := true
		for _, r := range readable {
			if strings.HasPrefix(p, r) {
				wider = false
			}
		}
		if wider {
			fail(".policy.panelReadablePrefixes", "cannot be wider than readablePrefixes: "+jsjson.QuoteString(p))
		}
	}

	params := jsjson.Object{}
	if pv, ok := get(root, "params"); ok {
		for _, m := range obj(pv, ".params") {
			k := m.Key
			if !identPrefixRE.MatchString(k) {
				fail(".params."+k, "key must be an identifier")
			}
			if k == "as_of" {
				fail(".params.as_of", "reserved by the engine")
			}
			switch v := m.Value.(type) {
			case float64:
				params = append(params, jsjson.Member{Key: k, Value: v})
			case string:
				params = append(params, jsjson.Member{Key: k, Value: paramValue(v, ".params."+k)})
			default:
				arr := strArray(v, ".params."+k, false)
				for i, x := range arr {
					paramValue(x, fmt.Sprintf(".params.%s[%d]", k, i))
				}
				params = append(params, jsjson.Member{Key: k, Value: arr})
			}
		}
	}

	agent := parseAgent(root)

	r := jsjson.Object{}
	if v, ok := get(root, "run"); ok {
		r = obj(v, ".run")
	}
	onlyKeys(r, []string{"heapLimitMb"}, ".run")
	s := jsjson.Object{}
	if v, ok := get(root, "server"); ok {
		s = obj(v, ".server")
	}
	onlyKeys(s, []string{"port", "auth", "publicOrigin", "proxyHops", "auditRetentionDays"}, ".server")
	var publicOrigin *string
	if v, ok := get(s, "publicOrigin"); ok {
		o := httpsOrigin(str(v, ".server.publicOrigin"))
		publicOrigin = &o
	}
	if _, ok := get(s, "proxyHops"); ok && publicOrigin == nil {
		fail(".server.proxyHops", "only with publicOrigin")
	}
	auth := false
	if v, ok := get(s, "auth"); ok {
		b, isBool := v.(bool)
		if !isBool {
			fail(".server.auth", "true or false")
		}
		auth = b
	}
	if publicOrigin != nil && !auth {
		fail(".server.auth", "must be true with publicOrigin")
	}

	namev, _ := get(root, "name")
	cfg.Name = str(namev, ".name")
	cfg.Language = "en"
	if v, ok := get(root, "language"); ok {
		if v != "en" && v != "ko" {
			fail(".language", `"en" or "ko"`)
		}
		cfg.Language = v.(string)
	}
	cfg.Datasource = datasource
	cfg.ReadablePrefixes = readable
	cfg.PanelPrefixes = panel
	cfg.Params = params
	cfg.Agent = agent
	cfg.HeapLimitMb = 2048
	if v, ok := get(r, "heapLimitMb"); ok {
		cfg.HeapLimitMb = int(num(v, ".run.heapLimitMb", true, 64, false))
	}
	cfg.Server = Server{Port: 4170, Auth: auth, PublicOrigin: publicOrigin, ProxyHops: 1, AuditRetentionDays: 90}
	if v, ok := get(s, "port"); ok {
		cfg.Server.Port = int(num(v, ".server.port", true, 1, false))
	}
	if v, ok := get(s, "proxyHops"); ok {
		cfg.Server.ProxyHops = int(num(v, ".server.proxyHops", true, 1, false))
	}
	if v, ok := get(s, "auditRetentionDays"); ok {
		cfg.Server.AuditRetentionDays = int(num(v, ".server.auditRetentionDays", true, 1, false))
	}
	cfg.OutDir = dir
	if v, ok := get(root, "outDir"); ok {
		cfg.OutDir = relativeOutDir(dir, str(v, ".outDir"))
	}
	if v, ok := get(root, "ga4"); ok {
		c, err := ga4.ParseConnection(v, func(p string) string {
			x := ExpandHome(p, home)
			if filepath.IsAbs(x) {
				return x
			}
			return resolve(dir, x)
		})
		if err != nil {
			fail(".ga4", err.Error())
		}
		cfg.GA4 = &c
	}
	return cfg, nil
}

func parseAgent(root jsjson.Object) Agent {
	a := jsjson.Object{}
	if v, ok := get(root, "agent"); ok {
		a = obj(v, ".agent")
	}
	onlyKeys(a, agentKeys, ".agent")
	agent := defaultAgent()
	if v, ok := get(a, "bin"); ok {
		agent.Bin = str(v, ".agent.bin")
	}
	if v, ok := get(a, "model"); ok {
		if v == nil {
			agent.Model = nil
		} else {
			m := str(v, ".agent.model")
			agent.Model = &m
		}
	}
	if v, ok := get(a, "callBudgetUsd"); ok {
		agent.CallBudgetUsd = num(v, ".agent.callBudgetUsd", false, 0, true)
	}
	if v, ok := get(a, "requestBudgetUsd"); ok {
		agent.RequestBudget = num(v, ".agent.requestBudgetUsd", false, 0, true)
	}
	for _, k := range []string{"maxTurns", "maxProbes", "maxFixes", "callTimeoutMs", "concurrency"} {
		v, ok := get(a, k)
		if !ok {
			continue
		}
		min := 1.0
		if k == "maxFixes" || k == "maxProbes" {
			min = 0
		}
		n := int(num(v, ".agent."+k, true, min, false))
		switch k {
		case "maxTurns":
			agent.MaxTurns = n
		case "maxProbes":
			agent.MaxProbes = n
		case "maxFixes":
			agent.MaxFixes = n
		case "callTimeoutMs":
			agent.CallTimeoutMs = n
		case "concurrency":
			agent.Concurrency = n
		}
	}
	if v, ok := get(a, "provider"); ok {
		if v != "claude-code" && v != "anthropic" && v != "openai" {
			fail(".agent.provider", `"claude-code", "anthropic", "openai"`)
		}
		agent.Provider = v.(string)
	}
	if v, ok := get(a, "apiKey"); ok {
		s, isStr := v.(string)
		if !isStr {
			fail(".agent.apiKey", "must be a string")
		}
		agent.APIKey = s
	}
	if v, ok := get(a, "baseUrl"); ok {
		raw := str(v, ".agent.baseUrl")
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" {
			fail(".agent.baseUrl", "invalid URL")
		}
		if u.Scheme != "https" && u.Scheme != "http" {
			fail(".agent.baseUrl", "http(s) URLs only")
		}
		if u.Host == "" {
			fail(".agent.baseUrl", "invalid URL")
		}
		agent.BaseURL = &raw
	}
	if v, ok := get(a, "pricing"); ok {
		p := obj(v, ".agent.pricing")
		onlyKeys(p, []string{"inputPerMTok", "outputPerMTok"}, ".agent.pricing")
		in, _ := get(p, "inputPerMTok")
		out, _ := get(p, "outputPerMTok")
		agent.Pricing = &Pricing{InputPerMTok: num(in, ".agent.pricing.inputPerMTok", false, 0, false), OutputPerMTok: num(out, ".agent.pricing.outputPerMTok", false, 0, false)}
	}
	if v, ok := get(a, "maxOutputTokens"); ok {
		agent.MaxOutputTokens = int(num(v, ".agent.maxOutputTokens", true, 256, false))
	}
	if agent.Provider != "claude-code" {
		if agent.Model == nil {
			fail(".agent.model", "a model name is required for API providers")
		}
		if agent.Provider == "anthropic" && agent.APIKey == "" {
			fail(".agent.apiKey", "anthropic requires an API key")
		}
	} else {
		for _, k := range []string{"apiKey", "baseUrl", "pricing", "maxOutputTokens"} {
			if _, ok := get(a, k); ok {
				fail(".agent."+k, "only for API providers (anthropic, openai)")
			}
		}
	}
	if v, ok := get(a, "dataMode"); ok {
		if v != "pseudonymized" && v != "schema_only" {
			fail(".agent.dataMode", `"pseudonymized" or "schema_only"`)
		}
		agent.DataMode = v.(string)
	}
	return agent
}

var hostRE = regexp.MustCompile(`^(mysql|postgres|postgresql|sqlite)://(.+)$`)

// parseDatasource: host is `mysql://host[:port]`, `postgres://host[:port]` or `sqlite://file path`.
func parseDatasource(o jsjson.Object, dir, home string) Datasource {
	hv, _ := get(o, "host")
	host := str(hv, ".datasource.host")
	m := hostRE.FindStringSubmatch(host)
	if m == nil {
		fail(".datasource.host", "must start with mysql://, postgres:// or sqlite://")
	}
	if m[1] == "sqlite" {
		onlyKeys(o, []string{"host"}, ".datasource")
		p := ExpandHome(m[2], home)
		if !filepath.IsAbs(p) {
			p = resolve(dir, p)
		}
		return Datasource{Kind: "sqlite", Path: p}
	}
	onlyKeys(o, []string{"host", "user", "password", "database"}, ".datasource")
	kind := "postgres"
	if m[1] == "mysql" {
		kind = "mysql"
	}
	h, port, ok := opaqueHost(m[2])
	if !ok {
		fail(".datasource.host", "invalid host[:port]")
	}
	if h == "" {
		fail(".datasource.host", "host[:port] only (user and database go in their own keys)")
	}
	pw := ""
	if v, ok := get(o, "password"); ok {
		s, isStr := v.(string)
		if !isStr {
			fail(".datasource.password", "must be a string")
		}
		pw = s
	}
	if port == 0 {
		port = map[string]int{"mysql": 3306, "postgres": 5432}[kind]
	}
	uv, _ := get(o, "user")
	dv, _ := get(o, "database")
	return Datasource{Kind: kind, Host: h, Port: port, User: str(uv, ".datasource.user"), Password: pw, Database: str(dv, ".datasource.database")}
}

// opaqueHost parses the authority of a non-special URL like WHATWG URL does, for host[:port] only.
// Returns ok=false when URL parsing itself would throw, and an empty host when anything but host[:port] is present.
func opaqueHost(rest string) (host string, port int, ok bool) {
	for _, r := range rest {
		if r == ' ' || r < 0x20 || r == 0x7f {
			return "", 0, false
		}
	}
	end := strings.IndexAny(rest, "/?#")
	auth, tail := rest, ""
	if end >= 0 {
		auth, tail = rest[:end], rest[end:]
	}
	if tail != "" && tail != "/" && tail != "?" && tail != "#" {
		if !(strings.HasPrefix(tail, "?") && len(tail) == 1) {
			_, perr := parsePort(portPart(auth))
			if perr {
				return "", 0, false
			}
			return "", 0, true
		}
	}
	if strings.Contains(auth, "@") {
		_, perr := parsePort(portPart(auth[strings.LastIndex(auth, "@")+1:]))
		if perr {
			return "", 0, false
		}
		return "", 0, true
	}
	h := auth
	p := ""
	if strings.HasPrefix(h, "[") {
		i := strings.Index(h, "]")
		if i < 0 {
			return "", 0, false
		}
		p = strings.TrimPrefix(h[i+1:], ":")
		if h[i+1:] != "" && !strings.HasPrefix(h[i+1:], ":") {
			return "", 0, false
		}
		h = h[1:i]
	} else if i := strings.LastIndex(h, ":"); i >= 0 {
		h, p = h[:i], h[i+1:]
	}
	n, perr := parsePort(p)
	if perr {
		return "", 0, false
	}
	for _, c := range []string{"<", ">", "^", "|", "%", "\\"} {
		if strings.Contains(h, c) {
			return "", 0, false
		}
	}
	return h, n, true
}

func portPart(auth string) string {
	if strings.HasPrefix(auth, "[") {
		if i := strings.Index(auth, "]"); i >= 0 {
			return strings.TrimPrefix(auth[i+1:], ":")
		}
	}
	if i := strings.LastIndex(auth, ":"); i >= 0 {
		return auth[i+1:]
	}
	return ""
}

// parsePort returns 0 for an empty port; err when it is not a number up to 65535.
func parsePort(p string) (int, bool) {
	if p == "" {
		return 0, false
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return 0, true
		}
	}
	n, err := strconv.Atoi(p)
	if err != nil || n > 65535 {
		return 0, true
	}
	return n, false
}

// httpsOrigin validates `https://host[:port]` and returns the origin as WHATWG URL computes it.
func httpsOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		fail(".server.publicOrigin", "invalid URL")
	}
	if strings.ToLower(u.Scheme) != "https" || u.User != nil || (u.Path != "/" && u.Path != "") || u.RawQuery != "" || u.Fragment != "" {
		fail(".server.publicOrigin", "only https://host[:port]")
	}
	host := strings.ToLower(u.Hostname())
	if !strings.Contains(host, ":") {
		a, err := idna.Lookup.ToASCII(host)
		if err != nil {
			fail(".server.publicOrigin", "invalid URL")
		}
		host = a
	} else {
		host = "[" + host + "]"
	}
	port := u.Port()
	if port != "" {
		n, bad := parsePort(port)
		if bad {
			fail(".server.publicOrigin", "invalid URL")
		}
		port = strconv.Itoa(n)
	}
	if port == "" || port == "443" {
		return "https://" + host
	}
	return "https://" + host + ":" + port
}

func relativeOutDir(dir, v string) string {
	if filepath.IsAbs(v) || strings.HasPrefix(v, "~") {
		fail(".outDir", "must be a path relative to the workspace")
	}
	out := resolve(dir, v)
	rel, err := filepath.Rel(dir, out)
	if err != nil || rel == "." || rel == "" || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		fail(".outDir", "must be a subfolder of the workspace")
	}
	return out
}

// Load reads and validates workspace.json in dir.
func Load(dir string) (*Workspace, error) {
	file := filepath.Join(dir, "workspace.json")
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil, &ConfigError{"workspace config not found: " + file}
	}
	if err != nil {
		return nil, err
	}
	raw, err := jsjson.Parse(string(b))
	if err != nil {
		return nil, &ConfigError{"cannot read workspace.json: " + err.Error()}
	}
	home, _ := os.UserHomeDir()
	cfg, err := ParseConfig(raw, dir, home)
	if err != nil {
		return nil, err
	}
	return &Workspace{Dir: dir, Config: cfg}, nil
}

// OutputPaths are the paths the engine writes.
func OutputPaths(workspaceDir, outRoot string) []string {
	out := []string{outRoot}
	for _, d := range []string{"snapshots", "panels", "conversations", "results", "logs", ".agent-cwd", "agent-sessions"} {
		out = append(out, filepath.Join(outRoot, d))
	}
	return out
}

// JS returns the config in its stored JSON form.
func (c Config) JS() jsjson.Object {
	var ds jsjson.Object
	if c.Datasource.Kind == "sqlite" {
		ds = jsjson.Object{{Key: "kind", Value: "sqlite"}, {Key: "path", Value: c.Datasource.Path}}
	} else {
		d := c.Datasource
		ds = jsjson.Object{{Key: "kind", Value: d.Kind}, {Key: "host", Value: d.Host}, {Key: "port", Value: d.Port}, {Key: "user", Value: d.User}, {Key: "password", Value: d.Password}, {Key: "database", Value: d.Database}}
	}
	a := c.Agent
	var pricing any
	if a.Pricing != nil {
		pricing = jsjson.Object{{Key: "inputPerMTok", Value: a.Pricing.InputPerMTok}, {Key: "outputPerMTok", Value: a.Pricing.OutputPerMTok}}
	}
	agent := jsjson.Object{{Key: "provider", Value: a.Provider}, {Key: "bin", Value: a.Bin}, {Key: "model", Value: ptr(a.Model)}, {Key: "callBudgetUsd", Value: a.CallBudgetUsd}, {Key: "requestBudgetUsd", Value: a.RequestBudget},
		{Key: "maxTurns", Value: a.MaxTurns}, {Key: "maxProbes", Value: a.MaxProbes}, {Key: "maxFixes", Value: a.MaxFixes}, {Key: "callTimeoutMs", Value: a.CallTimeoutMs}, {Key: "concurrency", Value: a.Concurrency},
		{Key: "dataMode", Value: a.DataMode}, {Key: "apiKey", Value: a.APIKey}, {Key: "baseUrl", Value: ptr(a.BaseURL)}, {Key: "pricing", Value: pricing}, {Key: "maxOutputTokens", Value: a.MaxOutputTokens}}
	var g any
	if c.GA4 != nil {
		g = c.GA4.JS()
	}
	return jsjson.Object{
		{Key: "name", Value: c.Name}, {Key: "language", Value: c.Language}, {Key: "datasource", Value: ds},
		{Key: "policy", Value: jsjson.Object{{Key: "readablePrefixes", Value: c.ReadablePrefixes}, {Key: "panelReadablePrefixes", Value: c.PanelPrefixes}}},
		{Key: "params", Value: c.Params}, {Key: "agent", Value: agent}, {Key: "run", Value: jsjson.Object{{Key: "heapLimitMb", Value: c.HeapLimitMb}}},
		{Key: "server", Value: jsjson.Object{{Key: "port", Value: c.Server.Port}, {Key: "auth", Value: c.Server.Auth}, {Key: "publicOrigin", Value: ptr(c.Server.PublicOrigin)}, {Key: "proxyHops", Value: c.Server.ProxyHops}, {Key: "auditRetentionDays", Value: c.Server.AuditRetentionDays}}},
		{Key: "outDir", Value: c.OutDir}, {Key: "ga4", Value: g},
	}
}

func ptr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
