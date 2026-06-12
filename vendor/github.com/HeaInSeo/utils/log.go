package utils

import (
	"log/slog"
	"os"
)

// Log is the shared structured logger for this package.
// Callers can replace it with a custom *slog.Logger if needed.
var Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
