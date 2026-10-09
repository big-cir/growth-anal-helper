package publiccheck

import (
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"growth-lab/internal/contract"
	"growth-lab/internal/jsjson"
)

type op struct {
	Write   string `json:"write"`
	Content string `json:"content"`
	Rm      string `json:"rm"`
	Add     string `json:"add"`
	Commit  string `json:"commit"`
	AddAll  bool   `json:"addAll"`
}

var gitEnv = []string{"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@a", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@a", "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z"}

func unhex(t *testing.T, s string) string {
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func buildRepo(t *testing.T, dir string, ops []op) {
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), gitEnv...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "core.autocrlf", "false")
	for _, o := range ops {
		switch {
		case o.Write != "":
			p := filepath.Join(dir, unhex(t, o.Write))
			if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(unhex(t, o.Content)), 0o666); err != nil {
				t.Fatal(err)
			}
		case o.Rm != "":
			os.Remove(filepath.Join(dir, o.Rm))
		case o.Add != "":
			git("add", o.Add)
		case o.AddAll:
			git("add", "-A")
		case o.Commit != "":
			git("commit", "-q", "-m", unhex(t, o.Commit))
		}
	}
}

func TestContract(t *testing.T) {
	var v []struct {
		Name     string  `json:"name"`
		Denylist *string `json:"denylist"`
		Ops      []op    `json:"ops"`
		Results  []struct {
			Options struct {
				History         bool `json:"history"`
				RequireDenylist bool `json:"requireDenylist"`
			} `json:"options"`
			Out string `json:"out"`
		} `json:"results"`
	}
	if err := contract.Load("misc-public.json", &v); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range v {
		root := t.TempDir()
		repo := filepath.Join(root, "repo")
		os.Mkdir(repo, 0o777)
		buildRepo(t, repo, r.Ops)
		deny := filepath.Join(root, "public-denylist.txt")
		if r.Denylist != nil {
			os.WriteFile(deny, []byte(unhex(t, *r.Denylist)), 0o666)
		}
		for _, x := range r.Results {
			res, err := Run(Options{Repo: repo, DenylistFile: deny, History: x.Options.History, RequireDenylist: x.Options.RequireDenylist})
			if err != nil {
				t.Fatal(err)
			}
			findings := make([]string, len(res.Findings))
			for i, f := range res.Findings {
				findings[i] = Format(f)
			}
			warnings := res.Warnings
			if warnings == nil {
				warnings = []string{}
			}
			got := jsjson.MustStringify(jsjson.Object{{Key: "findings", Value: findings}, {Key: "warnings", Value: warnings}, {Key: "ok", Value: res.OK}})
			if want := unhex(t, x.Out); got != want {
				t.Errorf("%s %+v:\n got %s\nwant %s", r.Name, x.Options, got, want)
			}
			n++
		}
		// the command prints warnings and findings to stderr, the verdict to stdout
		var out, errb bytes.Buffer
		env := map[string]string{"GROWTH_LAB_WORKSPACE": root}
		code := Command([]string{}, filepath.Join(repo), func(k string) string { return env[k] }, &out, &errb)
		res, _ := Run(Options{Repo: repo, DenylistFile: deny})
		want := ""
		for _, w := range res.Warnings {
			want += "Warning: " + w + "\n"
		}
		for _, f := range res.Findings {
			want += Format(f) + "\n"
		}
		if res.OK {
			if code != 0 || out.String() != "public-check passed\n" {
				t.Errorf("%s: command %d %q", r.Name, code, out.String())
			}
		} else {
			want += "public-check failed: " + itoa(len(res.Findings)) + " finding(s)\n"
			if code != 1 {
				t.Errorf("%s: exit %d", r.Name, code)
			}
		}
		if errb.String() != want {
			t.Errorf("%s: stderr\n%s\nwant\n%s", r.Name, errb.String(), want)
		}
	}
	var out, errb bytes.Buffer
	if code := Command([]string{"--nope"}, ".", os.Getenv, &out, &errb); code != 2 || errb.String() != "Unknown option: --nope\n" {
		t.Errorf("unknown option: %d %q", code, errb.String())
	}
	if code := Command(nil, t.TempDir(), os.Getenv, &out, &errb); code != 2 || !strings.HasSuffix(errb.String(), "Run this inside a git repository\n") {
		t.Errorf("outside a repo: %d %q", code, errb.String())
	}
	t.Logf("%d runs", n)
}

func itoa(n int) string { return strings.TrimSpace(jsjson.Number(float64(n))) }

// Every rule category catches its sample and leaves ordinary code alone (samples are built at run time).
func TestRuleCategories(t *testing.T) {
	j := func(p ...string) string { return strings.Join(p, "") }
	hits := map[string][]string{
		"G1 private key":          {j("-----BEGIN ", "RSA PRIVATE KEY-----"), j("x -----BEGIN ", "PRIVATE KEY----- y")},
		"G2 cloud key":            {j("AKIA", "ABCDEFGHIJKLMNOP"), j("ghp_", strings.Repeat("a", 36)), j("sk-", "abcdefghijklmnopqrstu")},
		"G3 cloud host":           {j("db.rds", ".amazonaws.com"), j("a.RDS", ".x"), j("h.ec2", ".internal")},
		"G4 IPv4":                 {j("10.1", ".2.3"), j("x", "8.8.", "4.4"), j("[172.", "16.0.1]")},
		"G5 absolute home path":   {j("/Use", "rs/alice/x"), j(" /ho", "me/bob")},
		"G6 DB connection string": {j("mysql:", "//u:p@h"), j("MongoDB+srv:", "//a@b"), j("jdbc", ":postgresql")},
		"G7 high-entropy token":   {j("t = aZ3kQ9pL2mX8vB4nR7", "sT1yU6wE5oI0hG3jF2dK")},
		"D1 denylisted word":      {"Made by ACME corp", "acme"},
	}
	clean := []string{"server.listen(4170, '127.0.0.1')", "version 1.2.3", "0.0.0.0", j("192.0.", "2.10"), j("1.2.", "3.4.5"), "999.1.1.1", "sha256 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "path ./home/readme", j("mysql:", "//localhost/db"), "AKIAshort"}
	rules := MakeRules([]string{"acme"})
	for rule, lines := range hits {
		for _, l := range lines {
			f := ScanText("f", l, rules)
			found := false
			for _, x := range f {
				if x.Rule == rule {
					found = true
				}
			}
			if !found {
				t.Errorf("%s missed %q (%v)", rule, l, f)
			}
		}
	}
	for _, l := range clean {
		if f := ScanText("f", l, rules); len(f) > 0 {
			t.Errorf("false positive on %q: %v", l, f)
		}
	}
	// binary content: no line numbers, control characters split lines
	f := ScanText("b.bin", j("\x00\x01AKIA", "ABCDEFGHIJKLMNOP\x02"), rules)
	if len(f) != 1 || f[0].Where != "b.bin (binary)" || f[0].Line != 0 {
		t.Errorf("binary: %v", f)
	}
	// paths are checked too
	if f := ScanPath(j("docs/acme", "-plan.md"), rules); len(f) != 1 || f[0].Where != "(path) docs/acme-plan.md" {
		t.Errorf("path: %v", f)
	}
}
