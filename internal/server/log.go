package server

import (
	"io"
	"log/slog"
	"os"
)

// SetupLogger sets the default runtime logger: text on w, or JSON when GROWTH_LAB_LOG_FORMAT=json.
func SetupLogger(w io.Writer) {
	var h slog.Handler
	if os.Getenv("GROWTH_LAB_LOG_FORMAT") == "json" {
		h = slog.NewJSONHandler(w, nil)
	} else {
		h = slog.NewTextHandler(w, nil)
	}
	slog.SetDefault(slog.New(h))
}
