// Package publiccheck finds content that must not reach a public repository: generic rules plus the workspace denylist (public-denylist.txt).
// Findings never print the matched text.
package publiccheck

import (
	"bytes"
	"fmt"
	"growth-lab/internal/jsstr"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"growth-lab/internal/workspace"
)

// Finding is one hit; Line is 0 when there is no line number.
type Finding struct {
	Where string
	Line  int
	Rule  string
}

// Rule tests one line.
type Rule struct {
	ID   string
	Test func(line string) bool
}

// ci spells a word case-insensitively for ASCII only (as a JavaScript /i regex does for these patterns).
func ci(word string) string {
	var b strings.Builder
	for _, c := range word {
		if c >= 'a' && c <= 'z' {
			b.WriteString("[" + string(c) + string(c-32) + "]")
		} else {
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

var (
	g1      = regexp.MustCompile(`-{5}BEGIN [A-Z ]*PRIVATE KEY-{5}`)
	g2a     = regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)
	g2b     = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)
	g2c     = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)
	g3a     = regexp.MustCompile(`[A-Za-z0-9-]+\.(` + ci("amazonaws") + `|` + ci("rds") + `)\.` + ci("com") + `\b`)
	g3b     = regexp.MustCompile(`\.` + ci("rds") + `\.[a-zA-Z]`)
	g3c     = regexp.MustCompile(`\.(` + ci("compute") + `|` + ci("ec2") + `)\.` + ci("internal") + `\b`)
	g5      = regexp.MustCompile(`(^|[^A-Za-z0-9_.~])/(Users|home)/[^/` + jsstr.SpaceChars + `'"` + "`" + `]+`)
	g6a     = regexp.MustCompile(`\b(` + ci("mysql") + `|` + ci("postgres") + `(` + ci("ql") + `)?|` + ci("mongodb") + `(\+` + ci("srv") + `)?|` + ci("redis") + `)[:]//[^` + jsstr.SpaceChars + `'"` + "`" + `]+@`)
	g6b     = regexp.MustCompile(`\b` + ci("jdbc") + `[:][a-zA-Z]`)
	tokenRE = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)
	lowerRE = regexp.MustCompile(`[a-z]`)
	upperRE = regexp.MustCompile(`[A-Z]`)
	digitRE = regexp.MustCompile(`[0-9]`)
)

var genericRules = []Rule{
	{"G1 private key", g1.MatchString},
	{"G2 cloud key", func(l string) bool { return g2a.MatchString(l) || g2b.MatchString(l) || g2c.MatchString(l) }},
	{"G3 cloud host", func(l string) bool { return g3a.MatchString(l) || g3b.MatchString(l) || g3c.MatchString(l) }},
	{"G4 IPv4", findIPv4},
	{"G5 absolute home path", g5.MatchString},
	{"G6 DB connection string", func(l string) bool { return g6a.MatchString(l) || g6b.MatchString(l) }},
	{"G7 high-entropy token", findHighEntropy},
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// findIPv4 matches /(?<![\d.])(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})(?![\d.])/g, skipping loopback, 0.0.0.x and documentation ranges.
func findIPv4(line string) bool {
	for i := 0; i < len(line); {
		if !isDigit(line[i]) || (i > 0 && (isDigit(line[i-1]) || line[i-1] == '.')) {
			i++
			continue
		}
		var o [4]int
		j := i
		ok := true
		for k := 0; k < 4 && ok; k++ {
			s := j
			for j < len(line) && isDigit(line[j]) {
				j++
			}
			n := j - s
			if n < 1 || n > 3 {
				ok = false
				break
			}
			o[k], _ = strconv.Atoi(line[s:j])
			if k < 3 {
				if j >= len(line) || line[j] != '.' {
					ok = false
					break
				}
				j++
			}
		}
		if !ok || (j < len(line) && (isDigit(line[j]) || line[j] == '.')) {
			i++
			continue
		}
		i = j
		if o[0] > 255 || o[1] > 255 || o[2] > 255 || o[3] > 255 {
			continue
		}
		a, b, c := o[0], o[1], o[2]
		if a == 127 || (a == 0 && b == 0 && c == 0) {
			continue
		}
		if (a == 192 && b == 0 && c == 2) || (a == 198 && b == 51 && c == 100) || (a == 203 && b == 0 && c == 113) {
			continue
		}
		return true
	}
	return false
}

func entropy(s string) float64 {
	var order []byte
	counts := map[byte]int{}
	for i := 0; i < len(s); i++ {
		if counts[s[i]] == 0 {
			order = append(order, s[i])
		}
		counts[s[i]]++
	}
	h := 0.0
	for _, c := range order {
		p := float64(counts[c]) / float64(len(s))
		h -= p * math.Log2(p)
	}
	return h
}

func findHighEntropy(line string) bool {
	for _, t := range tokenRE.FindAllString(line, -1) {
		if !lowerRE.MatchString(t) || !upperRE.MatchString(t) || !digitRE.MatchString(t) {
			continue
		}
		if entropy(t) >= 4.5 {
			return true
		}
	}
	return false
}

// LoadDenylist reads the denylist: one word per line, # comments, case-insensitive.
func LoadDenylist(file string) ([]string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range regexp.MustCompile(`\r?\n`).Split(string(b), -1) {
		l = jsstr.Trim(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, strings.ToLower(l))
	}
	return out, nil
}

// MakeRules returns the generic rules plus one rule per denylisted word.
func MakeRules(denylist []string) []Rule {
	rules := append([]Rule(nil), genericRules...)
	for i, w := range denylist {
		word := w
		rules = append(rules, Rule{fmt.Sprintf("D%d denylisted word", i+1), func(l string) bool { return strings.Contains(strings.ToLower(l), word) }})
	}
	return rules
}

var (
	goSumFile = regexp.MustCompile(`(^|[` + jsstr.SpaceChars + `/])go\.sum$`)
	goSumLine = regexp.MustCompile(`^[^` + jsstr.SpaceChars + `]+ v[^` + jsstr.SpaceChars + `]+ h1:[A-Za-z0-9+/]{43}=$`)
	controlRE = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]+`)
)

// ScanText checks line by line. Binary content is checked with control characters turned into line breaks.
func ScanText(where, text string, rules []Rule) []Finding {
	binary := strings.Contains(text, "\x00")
	body, label := text, where
	if binary {
		body = controlRE.ReplaceAllString(text, "\n")
		label = where + " (binary)"
	}
	goSum := !binary && goSumFile.MatchString(where)
	var out []Finding
	for i, line := range strings.Split(body, "\n") {
		for _, r := range rules {
			if goSum && strings.HasPrefix(r.ID, "G7") && goSumLine.MatchString(line) {
				continue
			}
			if r.Test(line) {
				n := i + 1
				if binary {
					n = 0
				}
				out = append(out, Finding{label, n, r.ID})
			}
		}
	}
	return out
}

// ScanPath checks a path name.
func ScanPath(path string, rules []Rule) []Finding {
	var out []Finding
	for _, r := range rules {
		if r.Test(path) {
			out = append(out, Finding{"(path) " + path, 0, r.ID})
		}
	}
	return out
}

func git(cwd string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Command failed: git %s\n%s", strings.Join(args, " "), stderr.String())
	}
	return out, nil
}

func nulList(b []byte) []string {
	var out []string
	for _, s := range strings.Split(string(b), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ScanWorkingSet checks tracked and untracked file contents, staged contents and path names.
func ScanWorkingSet(repo string, rules []Rule) ([]Finding, error) {
	var findings []Finding
	tracked, err := git(repo, nil, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	untracked, err := git(repo, nil, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range append(nulList(tracked), nulList(untracked)...) {
		if seen[f] {
			continue
		}
		seen[f] = true
		findings = append(findings, ScanPath(f, rules)...)
		if b, err := os.ReadFile(filepath.Join(repo, f)); err == nil {
			findings = append(findings, ScanText(f, string(b), rules)...)
		} else if fi, serr := os.Stat(filepath.Join(repo, f)); serr == nil && fi.IsDir() {
			return nil, fmt.Errorf("EISDIR: illegal operation on a directory, read")
		}
	}
	staged, err := git(repo, nil, "diff", "--cached", "--name-only", "-z", "--diff-filter=ACMR")
	if err != nil {
		return nil, err
	}
	for _, f := range nulList(staged) {
		findings = append(findings, ScanPath(f, rules)...)
		body, err := git(repo, nil, "show", ":"+f)
		if err != nil {
			return nil, err
		}
		findings = append(findings, ScanText("(staged) "+f, string(body), rules)...)
	}
	return findings, nil
}

// ScanHistory checks all commit messages, blob contents and paths.
func ScanHistory(repo string, rules []Rule) ([]Finding, error) {
	var findings []Finding
	list, err := git(repo, nil, "rev-list", "--all")
	if err != nil {
		return nil, err
	}
	var commits []string
	for _, c := range strings.Split(string(list), "\n") {
		if c != "" {
			commits = append(commits, c)
		}
	}
	if len(commits) == 0 {
		return findings, nil
	}
	for _, sha := range commits {
		msg, err := git(repo, nil, "log", "-1", "--format=%B", sha)
		if err != nil {
			return nil, err
		}
		findings = append(findings, ScanText("(commit message) "+sha[:10], string(msg), rules)...)
	}
	names, err := git(repo, nil, "log", "--all", "--format=", "--name-only", "-z", "--no-renames")
	if err != nil {
		return nil, err
	}
	seenPath := map[string]bool{}
	for _, p := range strings.Split(string(names), "\x00") {
		p = jsstr.Trim(p)
		if p == "" || seenPath[p] {
			continue
		}
		seenPath[p] = true
		findings = append(findings, ScanPath(p, rules)...)
	}
	objects, err := git(repo, nil, "rev-list", "--objects", "--all")
	if err != nil {
		return nil, err
	}
	var order []string
	pathOf := map[string]string{}
	for _, line := range strings.Split(string(objects), "\n") {
		if line == "" {
			continue
		}
		sha, p, _ := strings.Cut(line, " ")
		prev, has := pathOf[sha]
		if !has {
			order = append(order, sha)
		}
		if !has || (p != "" && prev == "") {
			pathOf[sha] = p
		}
	}
	out, err := git(repo, []byte(strings.Join(order, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	for pos := 0; pos < len(out); {
		nl := bytes.IndexByte(out[pos:], '\n')
		if nl < 0 {
			break
		}
		nl += pos
		parts := strings.Split(string(out[pos:nl]), " ")
		if len(parts) < 3 {
			break
		}
		size, err := strconv.Atoi(parts[2])
		if err != nil {
			break
		}
		body := out[nl+1 : min(nl+1+size, len(out))]
		pos = nl + 1 + size + 1
		if parts[1] == "blob" {
			where := pathOf[parts[0]]
			if where == "" {
				where = parts[0][:10]
			}
			findings = append(findings, ScanText("(history) "+where, string(body), rules)...)
		}
	}
	return dedupe(findings), nil
}

func dedupe(fs []Finding) []Finding {
	seen := map[Finding]bool{}
	var out []Finding
	for _, f := range fs {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// Options for Run.
type Options struct {
	Repo, DenylistFile       string
	History, RequireDenylist bool
}

// Result: OK when there are no findings.
type Result struct {
	Findings []Finding
	Warnings []string
	OK       bool
}

// Run checks the working set (and history with History).
func Run(o Options) (Result, error) {
	var warnings []string
	var denylist []string
	if _, err := os.Stat(o.DenylistFile); err == nil {
		d, err := LoadDenylist(o.DenylistFile)
		if err != nil {
			return Result{}, err
		}
		denylist = d
		if len(denylist) == 0 {
			if o.RequireDenylist {
				return Result{Warnings: []string{"Denylist is empty (the hook needs at least one entry)"}}, nil
			}
			warnings = append(warnings, "Denylist is empty, so the denylist check is skipped")
			denylist = nil
		}
	} else if o.RequireDenylist {
		return Result{Warnings: []string{"No denylist (required for the hook): public-denylist.txt in the workspace"}}, nil
	} else {
		warnings = append(warnings, "No denylist, so only the generic checks run")
	}
	rules := MakeRules(denylist)
	findings, err := ScanWorkingSet(o.Repo, rules)
	if err != nil {
		return Result{}, err
	}
	if o.History {
		h, err := ScanHistory(o.Repo, rules)
		if err != nil {
			return Result{}, err
		}
		findings = append(findings, h...)
	}
	findings = dedupe(findings)
	return Result{Findings: findings, Warnings: warnings, OK: len(findings) == 0}, nil
}

// Format prints a finding.
func Format(f Finding) string {
	if f.Line == 0 {
		return f.Where + "  [" + f.Rule + "]"
	}
	return fmt.Sprintf("%s:%d  [%s]", f.Where, f.Line, f.Rule)
}

// Command is `public-check [--history] [--require-denylist]`, run from cwd.
func Command(args []string, cwd string, getenv func(string) string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a != "--history" && a != "--require-denylist" {
			fmt.Fprintf(stderr, "Unknown option: %s\n", a)
			return 2
		}
	}
	repo := workspace.GitRoot(cwd)
	if repo == "" {
		fmt.Fprintln(stderr, "Run this inside a git repository")
		return 2
	}
	has := func(f string) bool {
		for _, a := range args {
			if a == f {
				return true
			}
		}
		return false
	}
	r, err := Run(Options{Repo: repo, DenylistFile: filepath.Join(workspace.FindDir(getenv, repo), "public-denylist.txt"), History: has("--history"), RequireDenylist: has("--require-denylist")})
	if err != nil {
		fmt.Fprintf(stderr, "public-check failed: %s\n", err.Error())
		return 1
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(stderr, "Warning: %s\n", w)
	}
	for _, f := range r.Findings {
		fmt.Fprintln(stderr, Format(f))
	}
	if !r.OK {
		fmt.Fprintf(stderr, "public-check failed: %d finding(s)\n", len(r.Findings))
		return 1
	}
	suffix := ""
	if has("--history") {
		suffix = " (including history)"
	}
	fmt.Fprintf(stdout, "public-check passed%s\n", suffix)
	return 0
}
