package agent

import (
	"growth-lab/internal/i18n"
	"strings"

	"growth-lab/internal/assets"
	"growth-lab/internal/panels"
	"growth-lab/internal/sensitive"
)

var (
	summarySchemaText = assets.SummarySchema
	summaryGuideText  = assets.SummaryGuide
)

// SummarySchemaArg is the compact summary schema passed to the agent.
var SummarySchemaArg = mustCompact(summarySchemaText)

// SummaryGuide is the summary guide with the language and units filled in.
func SummaryGuide(lang string) string {
	u := panels.ClaimUnits(lang)
	r := strings.NewReplacer("{{LANGUAGE}}", LanguageNames[lang])
	s := r.Replace(summaryGuideText)
	s = strings.ReplaceAll(s, "{{DIFF_UNIT}}", u["diff"])
	return strings.ReplaceAll(s, "{{RATIO_UNIT}}", u["ratio"])
}

// SummaryOutcome is ok with the text, or failed with a message.
type SummaryOutcome struct {
	OK      bool
	Text    string
	Message string
}

// SummarizeInput: Rows are the result from the pseudonymized copy; Call runs one agent call.
type SummarizeInput struct {
	Spec     panels.Spec
	Columns  []panels.Column
	Rows     []panels.Row
	Outbound *Outbound
	Lang     string
	Call     func(input, sessionID string) CallResult
}

// Summarize writes the long description of a panel being saved: a new session, retried once if the check fails.
func Summarize(o SummarizeInput) (SummaryOutcome, error) {
	input, err := o.Outbound.Summarize(o.Spec, panels.SummaryResult(o.Columns, o.Rows))
	if err != nil {
		return SummaryOutcome{}, err
	}
	sessionID := ""
	last := ""
	for attempt := 0; attempt < 2; attempt++ {
		res := o.Call(input, sessionID)
		if !res.OK {
			return SummaryOutcome{Message: res.Message}, nil
		}
		sessionID = res.SessionID
		var problems []string
		draft, perr := panels.ParseSummary(res.Structured)
		if perr != nil {
			if _, ok := perr.(*panels.SummaryError); !ok {
				return SummaryOutcome{}, perr
			}
			problems = []string{perr.Error()}
		} else {
			check := panels.CheckSummary(draft, o.Spec, o.Columns, o.Rows, o.Lang)
			if check.OK {
				if sensitive.SecretShape(check.Text) != "" || sensitive.SecretAssignment(check.Text) {
					return SummaryOutcome{Message: i18n.In(o.Lang, "The description looked like it contained a secret, so it was discarded", "설명에 비밀처럼 보이는 값이 있어 버렸어요")}, nil
				}
				return SummaryOutcome{OK: true, Text: check.Text}, nil
			}
			problems = check.Problems
		}
		last = strings.Join(problems, "; ")
		if input, err = o.Outbound.SummaryRetry(problems); err != nil {
			return SummaryOutcome{}, err
		}
	}
	return SummaryOutcome{Message: i18n.In(o.Lang, "Description check failed: "+last, "설명 검사 실패: "+last)}, nil
}
