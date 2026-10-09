// Package contract loads the shared vectors in testdata/contracts that pin JS-compatible hashing and formatting.
package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// Dir is the vector folder for the current contract version.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "contracts", "v1")
}

// Load decodes one vector file into v.
func Load(name string, v any) error {
	b, err := os.ReadFile(filepath.Join(Dir(), name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Out is an expected result: a value or an error message.
type Out struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Error string          `json:"error"`
}
