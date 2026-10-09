// Package snapshot holds snapshot files, raw table DDL and the snapshot pipeline.
package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"

	"growth-lab/internal/jsjson"
)

// Current is current.json.
type Current struct {
	SnapshotID string `json:"snapshot_id"`
	File       string `json:"file"`
	AgentFile  string `json:"agent_file"`
}

// Dir is the snapshots folder under the output root.
func Dir(outRoot string) string { return filepath.Join(outRoot, "snapshots") }

// Files are the three files of one snapshot.
type Files struct{ Real, Agent, Map string }

// SnapshotFiles returns the file paths of a snapshot.
func SnapshotFiles(dir, id string) Files {
	return Files{Real: filepath.Join(dir, id+".sqlite"), Agent: filepath.Join(dir, id+".agent.sqlite"), Map: filepath.Join(dir, id+".pseudo-map.sqlite")}
}

// FsyncPath flushes a file or folder to disk.
func FsyncPath(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// ReadCurrent reads current.json with absolute file paths, or nil.
func ReadCurrent(dir string) (*Current, error) {
	b, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Current
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	c.File = filepath.Join(dir, c.File)
	c.AgentFile = filepath.Join(dir, c.AgentFile)
	return &c, nil
}

// WriteCurrent points current.json at a snapshot, atomically.
func WriteCurrent(dir, id string) error {
	tmp := filepath.Join(dir, "current.json.tmp")
	body := "{\n  \"snapshot_id\": " + quote(id) + ",\n  \"file\": " + quote(id+".sqlite") + ",\n  \"agent_file\": " + quote(id+".agent.sqlite") + "\n}\n"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	if err := FsyncPath(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "current.json")); err != nil {
		return err
	}
	return FsyncPath(dir)
}

func quote(s string) string { return jsjson.QuoteString(s) }
