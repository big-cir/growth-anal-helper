package coverage

import (
	"fmt"
	"strings"
)

// Signal is one coverage finding. Ask signals go back to the agent once; every signal adds a caveat.
type Signal struct {
	Rule   string // gap_dependent, gap_check_failed
	Ask    bool
	Agent  string
	Caveat string
}

// CheckInput is a panel that already passed the other checks.
type CheckInput struct {
	Tables []string
	// SameOnGapCopy reruns the panel on a gap copy and reports whether the rows are unchanged.
	SameOnGapCopy func(path string) (bool, error)
	Lang          string
}

// Check reruns a panel that reads event tables on each gap copy; a changed result depends on rows before that gap.
func (r *Resolved) Check(in CheckInput) []Signal {
	if len(r.Gaps) == 0 || !r.readsEvents(in.Tables) {
		return nil
	}
	failed := Signal{Rule: "gap_check_failed", Caveat: tr(in.Lang, "The data gap check could not run", "데이터 공백 확인을 실행하지 못했어요")}
	if r.GapError != "" || len(r.GapPaths) != len(r.Gaps) || in.SameOnGapCopy == nil {
		return []Signal{failed}
	}
	var changed []string
	for i, path := range r.GapPaths {
		same, err := in.SameOnGapCopy(path)
		if err != nil {
			return []Signal{failed}
		}
		if !same {
			changed = append(changed, fmt.Sprintf("%s ~ %s", r.Gaps[i][0][:10], r.Gaps[i][1][:10]))
		}
	}
	if len(changed) == 0 {
		return nil
	}
	gaps := strings.Join(changed, ", ")
	return []Signal{{Rule: "gap_dependent", Ask: true,
		Agent:  fmt.Sprintf("The result changes when event rows before the data gap (%s) are removed: the panel uses records from before the gap (for example lag(), first/last values or running totals across it, or periods before it). Bound it to after the gap, or send the same panel again if using pre-gap records is intended.", gaps),
		Caveat: tr(in.Lang, fmt.Sprintf("Uses records from before the data gap (%s)", gaps), fmt.Sprintf("데이터 공백(%s) 이전 기록이 결과에 쓰였어요", gaps))}}
}

func (r *Resolved) readsEvents(tables []string) bool {
	for _, t := range tables {
		for _, e := range r.Events {
			if t == e.Table {
				return true
			}
		}
	}
	return false
}

func tr(lang, en, ko string) string {
	if lang == "ko" {
		return ko
	}
	return en
}
