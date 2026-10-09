package panels

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"growth-lab/internal/jsjson"
)

// SavedPanel is a saved panel file. Fields keep the file's keys in order; Spec is the parsed spec.
// Legacy: pre-dictionary format (no metric key), never recomputed, only regenerated.
type SavedPanel struct {
	Fields jsjson.Object
	Spec   Spec
	Legacy bool
}

// Str returns a string field ("" when missing).
func (p SavedPanel) Str(key string) string {
	v, _ := p.Fields.Get(key)
	s, _ := v.(string)
	return s
}

// Obj returns an object field.
func (p SavedPanel) Obj(key string) jsjson.Object {
	v, _ := p.Fields.Get(key)
	o, _ := v.(jsjson.Object)
	return o
}

// Set adds or replaces a field (a replaced key keeps its position).
func (p *SavedPanel) Set(key string, v any) {
	for i := range p.Fields {
		if p.Fields[i].Key == key {
			p.Fields[i].Value = v
			return
		}
	}
	p.Fields = append(p.Fields, jsjson.Member{Key: key, Value: v})
}

// JS returns the panel in its JSON form: parsed spec in place, legacy flag at the end.
func (p SavedPanel) JS() jsjson.Object {
	out := make(jsjson.Object, 0, len(p.Fields)+1)
	for _, m := range p.Fields {
		if m.Key == "legacy" {
			continue
		}
		if m.Key == "spec" {
			out = append(out, jsjson.Member{Key: "spec", Value: p.Spec.JS()})
			continue
		}
		out = append(out, m)
	}
	if p.Legacy {
		out = append(out, jsjson.Member{Key: "legacy", Value: true})
	}
	return out
}

var (
	panelIDRE   = regexp.MustCompile(`^[a-z0-9]{12}$`)
	panelFileRE = regexp.MustCompile(`^[a-z0-9]{12}\.json$`)
)

// Store reads and writes panels/<id>.json.
type Store struct{ Dir string }

// NewStore creates the folder if needed.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) file(id string) (string, error) {
	if !panelIDRE.MatchString(id) {
		return "", fmt.Errorf("invalid panel id: %s", id)
	}
	return filepath.Join(s.Dir, id+".json"), nil
}

// Get returns a panel, or nil when the id is invalid or missing.
func (s *Store) Get(id string) (*SavedPanel, error) {
	if !panelIDRE.MatchString(id) {
		return nil, nil
	}
	p, _ := s.file(id)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw, err := jsjson.Parse(string(b))
	if err != nil {
		return nil, err
	}
	fields, _ := raw.(jsjson.Object)
	specRaw, _ := fields.Get("spec")
	specObj, _ := specRaw.(jsjson.Object)
	_, hasMetric := specObj.Get("metric")
	spec, err := ParseSpec(specRaw)
	if err != nil {
		return nil, err
	}
	return &SavedPanel{Fields: fields, Spec: spec, Legacy: !hasMetric}, nil
}

// List returns all panels in creation order.
func (s *Store) List() ([]*SavedPanel, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	var out []*SavedPanel
	for _, e := range entries {
		if !panelFileRE.MatchString(e.Name()) {
			continue
		}
		p, err := s.Get(e.Name()[:12])
		if err != nil {
			return nil, err
		}
		if p != nil {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Str("created_at") < out[j].Str("created_at") })
	return out, nil
}

// FileText is what Write stores for a panel.
func FileText(p SavedPanel) string {
	var spec jsjson.Object
	j := p.Spec.JS()
	for _, m := range j {
		switch m.Key {
		case "metric":
			if !p.Legacy {
				spec = append(spec, m)
			}
		case "display":
			spec = append(spec, jsjson.Member{Key: "display", Value: DisplayJSON(p.Spec.Display)})
		default:
			spec = append(spec, m)
		}
	}
	out := jsjson.Object{}
	for _, m := range p.Fields {
		if m.Key == "legacy" {
			continue
		}
		if m.Key == "spec" {
			out = append(out, jsjson.Member{Key: "spec", Value: spec})
			continue
		}
		out = append(out, m)
	}
	if _, ok := p.Fields.Get("spec"); !ok {
		out = append(out, jsjson.Member{Key: "spec", Value: spec})
	}
	return jsjson.Indent(out, 1) + "\n"
}

// Write saves a panel atomically.
func (s *Store) Write(p SavedPanel) error {
	target, err := s.file(p.Str("id"))
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", target, os.Getpid())
	if err := os.WriteFile(tmp, []byte(FileText(p)), 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// Delete removes a panel; false when it did not exist.
func (s *Store) Delete(id string) (bool, error) {
	p, err := s.file(id)
	if err != nil {
		return false, err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
