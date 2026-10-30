package schema

import "testing"

func TestEnumValues(t *testing.T) {
	v, err := enumValues("enum('DE','it''s','KO')")
	if err != nil || len(v) != 3 || v[1] != "it's" {
		t.Errorf("enum %v %v", v, err)
	}
}

func TestAddIndexSQL(t *testing.T) {
	got := AddIndexSQL("stage", "t", IndexDef{Name: "k", Unique: true, Visible: false, Parts: []indexPart{{Column: "a", SubPart: "10"}, {Column: "b", Desc: true}}})
	if want := "ALTER TABLE `stage`.`t` ADD UNIQUE INDEX `k` (`a`(10), `b` DESC) INVISIBLE"; got != want {
		t.Errorf("got %s", got)
	}
}
