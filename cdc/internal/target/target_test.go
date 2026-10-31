package target

import (
	"strings"
	"testing"

	"growth-lab-cdc/internal/schema"
)

func TestJSONColumnsMatchAsJSON(t *testing.T) {
	d := &schema.TableDef{Columns: []schema.Column{{Name: "id", DataType: "bigint"}, {Name: "body", DataType: "json"}, {Name: "day", DataType: "date"}}, PK: []int{0}}
	tt := New(nil, "src", "app", map[string]*schema.TableDef{"app.doc": d}, 1).tables["app.doc"]
	for _, q := range []string{tt.upd, tt.del} {
		if !strings.Contains(q, "`body` <=> CAST(? AS JSON)") || !strings.Contains(q, "`day` <=> ?") {
			t.Errorf("%s", q)
		}
	}
}
