package panels

import (
	"crypto/sha256"
	"encoding/hex"
	"growth-lab/internal/jsstr"
	"regexp"
	"sort"
	"strings"

	"growth-lab/internal/jsjson"
	"growth-lab/internal/query"
	"growth-lab/internal/sqllint"
)

// RendererVersion is bumped when the screen renderer changes.
const RendererVersion = 2

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

var blankLinesRE = regexp.MustCompile(`\n{3,}`)

// NormalizeSQL drops trailing spaces and extra blank lines.
func NormalizeSQL(sql string) string {
	lines := strings.Split(sql, "\n")
	for i, l := range lines {
		lines[i] = jsstr.TrimEnd(l)
	}
	return jsstr.Trim(blankLinesRE.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// SemanticHash: same hash, same panel.
func SemanticHash(spec Spec, paramsHash string) string {
	return sha(jsjson.MustStringify([]any{NormalizeSQL(spec.SQL), paramsHash, DisplayJSON(spec.Display), definitionJS(spec.Definition), answersJS(spec.Answers), strPtr(spec.Metric)}))
}

// SchemaVersion hashes derived SQL and roles.
func SchemaVersion(derivedSQL, rolesHash string) string { return sha(derivedSQL + "\n" + rolesHash) }

func jsSorted(xs []string) []string {
	out := append([]string{}, xs...)
	sort.SliceStable(out, func(a, b int) bool { return jsstr.Less(out[a], out[b]) })
	return out
}

// PolicyVersion hashes the read policy and rule versions.
func PolicyVersion(readablePrefixes, panelReadablePrefixes []string) string {
	return sha(jsjson.MustStringify([]any{jsSorted(readablePrefixes), jsSorted(panelReadablePrefixes), jsSorted(query.AllowedFunctions), sqllint.RulesVersion, PatternContractVersion}))
}

// SeedDoc is a seed panel file's name and text.
type SeedDoc struct{ Name, Text string }

// DocsVersion hashes the guide, seed panels and metric dictionary.
func DocsVersion(guide string, seeds []SeedDoc, metrics string) string {
	s := append([]SeedDoc{}, seeds...)
	sort.SliceStable(s, func(a, b int) bool { return jsstr.Less(s[a].Name, s[b].Name) })
	pairs := make([]any, len(s))
	for i, p := range s {
		pairs[i] = []string{p.Name, p.Text}
	}
	return sha(jsjson.MustStringify([]any{guide, pairs, metrics}))
}

// PromptVersion hashes the engine guide and action schema.
func PromptVersion(engineGuide, actionSchema string) string {
	return sha(jsjson.MustStringify([]any{engineGuide, actionSchema}))
}

// ContextVersion identifies everything the agent context depends on.
type ContextVersion struct {
	SnapshotID, SchemaVersion, PolicyVersion, DocsVersion, PromptVersion string
}

// Key joins the versions.
func (v ContextVersion) Key() string {
	return strings.Join([]string{v.SnapshotID, v.SchemaVersion, v.PolicyVersion, v.DocsVersion, v.PromptVersion}, ":")
}

// ResultCacheKey identifies a computed result.
func ResultCacheKey(semantic string, ctx ContextVersion) string {
	return sha(jsjson.MustStringify([]any{semantic, ctx.Key(), PatternContractVersion, RendererVersion}))
}
