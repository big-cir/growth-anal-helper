// Package ga4 imports GA4 Data API reports. This file: the ga4 key of workspace.json.
package ga4

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"growth-lab/internal/jsjson"
)

// ConfigError is a GA4 settings error.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return e.Msg }

// Connection is the GA4 property and service account key.
type Connection struct {
	PropertyID string
	TimeZone   string
	KeyFile    string
}

// JS returns the connection in its stored JSON form.
func (c Connection) JS() jsjson.Object {
	return jsjson.Object{{Key: "propertyId", Value: c.PropertyID}, {Key: "timeZone", Value: c.TimeZone}, {Key: "keyFile", Value: c.KeyFile}}
}

var (
	propertyRE = regexp.MustCompile(`^\d{1,20}$`)
	offsetRE   = regexp.MustCompile(`^[+-](?:[01]\d|2[0-3])(?::?[0-5]\d)?$`)
)

// ValidTimeZone accepts what Intl.DateTimeFormat accepts: IANA names (any case) and UTC offsets.
func ValidTimeZone(tz string) bool {
	if tz == "" {
		return false
	}
	if offsetRE.MatchString(tz) {
		return true
	}
	if strings.EqualFold(tz, "local") {
		return false
	}
	if _, err := time.LoadLocation(tz); err == nil {
		return true
	}
	if name, ok := zoneNames()[strings.ToLower(tz)]; ok {
		_, err := time.LoadLocation(name)
		return err == nil
	}
	return false
}

var (
	zonesOnce sync.Once
	zones     map[string]string
)

// zoneNames maps lowercase zone names to their spelling, from the system zone database.
func zoneNames() map[string]string {
	zonesOnce.Do(func() {
		zones = map[string]string{}
		root := "/usr/share/zoneinfo"
		if d := os.Getenv("ZONEINFO"); d != "" {
			root = d
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			zones[strings.ToLower(filepath.ToSlash(rel))] = filepath.ToSlash(rel)
			return nil
		})
	})
	return zones
}

// ParseConnection validates the ga4 key of workspace.json. resolvePath makes key_file absolute.
func ParseConnection(raw any, resolvePath func(string) string) (Connection, error) {
	o, ok := raw.(jsjson.Object)
	if !ok {
		return Connection{}, &ConfigError{"must be an object"}
	}
	for _, m := range o {
		if m.Key != "property_id" && m.Key != "time_zone" && m.Key != "key_file" {
			return Connection{}, &ConfigError{"unknown key " + m.Key}
		}
	}
	pid, _ := o.Get("property_id")
	if s, ok := pid.(string); !ok || !propertyRE.MatchString(s) {
		return Connection{}, &ConfigError{"property_id must be a numeric string"}
	}
	tz, _ := o.Get("time_zone")
	if s, ok := tz.(string); !ok || !ValidTimeZone(s) {
		return Connection{}, &ConfigError{"time_zone must be an IANA time zone (e.g. America/Los_Angeles)"}
	}
	kf, _ := o.Get("key_file")
	if s, ok := kf.(string); !ok || s == "" {
		return Connection{}, &ConfigError{"key_file must be the path of the service account key file"}
	}
	return Connection{PropertyID: pid.(string), TimeZone: tz.(string), KeyFile: resolvePath(kf.(string))}, nil
}
