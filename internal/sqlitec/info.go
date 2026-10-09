package sqlitec

// Info is the SQLite build the engine runs on.
type Info struct {
	Version        string   `json:"version"`
	CompileOptions []string `json:"compile_options"`
}

// ReadInfo opens an in-memory database and reads the version and compile options.
func ReadInfo() (Info, error) {
	db, err := Open(":memory:", false)
	if err != nil {
		return Info{}, err
	}
	defer db.Close()
	v, err := db.Query("SELECT sqlite_version()")
	if err != nil {
		return Info{}, err
	}
	info := Info{Version: v[0][0].Text}
	opts, err := db.Query("PRAGMA compile_options")
	if err != nil {
		return Info{}, err
	}
	for _, r := range opts {
		info.CompileOptions = append(info.CompileOptions, r[0].Text)
	}
	return info, nil
}
