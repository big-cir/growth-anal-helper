package collect

import (
	"os"
	"regexp"
	"sort"
	"strings"

	"growth-lab/internal/jsjson"
)

// Roles maps "table.column" to its role, in insertion order.
type Roles struct {
	keys []string
	m    map[string]Role
}

// NewRoles returns an empty map.
func NewRoles() *Roles { return &Roles{m: map[string]Role{}} }

// Set adds or replaces a role (a replaced key keeps its position).
func (r *Roles) Set(k string, v Role) {
	if _, ok := r.m[k]; !ok {
		r.keys = append(r.keys, k)
	}
	r.m[k] = v
}

// Get returns the role of a column.
func (r *Roles) Get(k string) (Role, bool) {
	v, ok := r.m[k]
	return v, ok
}

// Keys returns the columns in insertion order.
func (r *Roles) Keys() []string { return append([]string(nil), r.keys...) }

// Len is the number of columns.
func (r *Roles) Len() int { return len(r.keys) }

// FinalizeError is a snapshot finalize error.
type FinalizeError struct{ Msg string }

func (e *FinalizeError) Error() string { return e.Msg }

// AllRoles combines the collected columns and the derived column declarations.
func AllRoles(specs []TableSpec, derived *Roles) (*Roles, error) {
	m := NewRoles()
	for _, t := range specs {
		for _, c := range t.Columns {
			m.Set(t.Target+"."+c.As, c.Role)
		}
	}
	for _, k := range derived.Keys() {
		if !strings.HasPrefix(k, "d_") {
			return nil, &FinalizeError{"derived-columns.json: only d_* columns can be declared: " + k}
		}
		v, _ := derived.Get(k)
		m.Set(k, v)
	}
	return m, nil
}

// RolesHash hashes the roles sorted by column.
func RolesHash(roles *Roles) string {
	keys := roles.Keys()
	sort.Strings(keys)
	arr := make([]any, len(keys))
	for i, k := range keys {
		v, _ := roles.Get(k)
		arr[i] = []any{k, v.JS()}
	}
	return sha(jsjson.MustStringify(arr))
}

// ParamsHash hashes the workspace params sorted by key.
func ParamsHash(params jsjson.Object) string {
	keys := make([]string, len(params))
	for i, m := range params {
		keys[i] = m.Key
	}
	sort.Strings(keys)
	arr := make([]any, len(keys))
	for i, k := range keys {
		v, _ := params.Get(k)
		arr[i] = []any{k, v}
	}
	return sha(jsjson.MustStringify(arr))
}

var derivedKeyRE = regexp.MustCompile(`^d_[A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_]*$`)

// ParseDerivedRoles validates derived-columns.json content.
func ParseDerivedRoles(raw any) (*Roles, error) {
	o, ok := raw.(jsjson.Object)
	if !ok {
		o = jsjson.Object{}
	}
	m := NewRoles()
	for _, e := range o {
		if !derivedKeyRE.MatchString(e.Key) {
			return nil, &FinalizeError{"derived-columns.json: invalid column name " + e.Key}
		}
		switch v := e.Value.(type) {
		case string:
			if v == "ordinary" || v == "private" {
				m.Set(e.Key, Role{Name: v})
				continue
			}
		case jsjson.Object:
			if len(v) == 1 && v[0].Key == "identifier" {
				if dom, ok := v[0].Value.(string); ok {
					m.Set(e.Key, Role{Identifier: dom})
					continue
				}
			}
		}
		return nil, &FinalizeError{"derived-columns.json: the role of " + e.Key + ` must be "ordinary" | "private" | { "identifier": "<domain>" }`}
	}
	return m, nil
}

// LoadDerivedRoles reads derived-columns.json.
func LoadDerivedRoles(file string) (*Roles, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &FinalizeError{"role declaration file not found: " + file}
		}
		return nil, err
	}
	raw, err := jsjson.Parse(string(b))
	if err != nil {
		return nil, err
	}
	return ParseDerivedRoles(raw)
}

// JS returns the roles as an ordered object.
func (r *Roles) JS() jsjson.Object {
	o := make(jsjson.Object, 0, len(r.keys))
	for _, k := range r.keys {
		o = append(o, jsjson.Member{Key: k, Value: r.m[k].JS()})
	}
	return o
}
