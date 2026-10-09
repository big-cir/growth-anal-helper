package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitRoot is the repository root containing cwd, or "".
func GitRoot(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// realpathLoose resolves symlinks, also for paths that do not exist yet (via the nearest existing ancestor).
func realpathLoose(p string) string {
	abs, _ := filepath.Abs(p)
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return abs
	}
	return filepath.Join(realpathLoose(parent), filepath.Base(abs))
}

// isPublicExample: the repository's own examples/demo workspace.
func isPublicExample(dir string) bool {
	root := GitRoot(dir)
	if root == "" {
		return false
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !strings.HasPrefix(string(mod), "module growth-lab\n") {
		return false
	}
	return realpathLoose(dir) == realpathLoose(filepath.Join(root, "examples", "demo"))
}

// GuardedPaths must be git-ignored: the workspace itself unless it is the public example, and the output paths.
func GuardedPaths(ws *Workspace) []string {
	var out []string
	if !isPublicExample(ws.Dir) {
		out = append(out, ws.Dir)
	}
	return append(out, OutputPaths(ws.Dir, ws.Config.OutDir)...)
}

// FindUnignoredOutputs returns paths inside the repository that are not git-ignored.
func FindUnignoredOutputs(paths []string, cwd string) []string {
	root := GitRoot(cwd)
	if root == "" {
		return nil
	}
	var bad []string
	for _, p := range paths {
		rel, err := filepath.Rel(realpathLoose(root), realpathLoose(p))
		if err != nil {
			continue
		}
		if rel == "." {
			bad = append(bad, ".")
			continue
		}
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			continue
		}
		cmd := exec.Command("git", "check-ignore", "-q", "--no-index", filepath.ToSlash(rel)+"/")
		cmd.Dir = root
		if cmd.Run() != nil {
			bad = append(bad, rel)
		}
	}
	return bad
}

// AssertOutputsIgnored fails when an output path would be committed.
func AssertOutputsIgnored(paths []string, cwd string) error {
	if bad := FindUnignoredOutputs(paths, cwd); len(bad) > 0 {
		return &ConfigError{"output paths are not git-ignored: " + strings.Join(bad, ", ") + " (add them to .gitignore or keep the workspace outside the repository)"}
	}
	return nil
}
