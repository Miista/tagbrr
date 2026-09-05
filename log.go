package main

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// newLogger builds the process-wide zerolog.Logger using ConsoleWriter —
// the same colored, human-readable format used by reaparr/diun elsewhere
// in this stack ("TIME | LEVEL | message key=value ..."). Level is
// configurable via LOG_LEVEL.
//
// Log call sites build a complete, readable sentence as the message;
// structured fields are trailing context, not the primary payload.
func newLogger(levelStr string) zerolog.Logger {
	level, err := zerolog.ParseLevel(strings.ToLower(levelStr))
	if err != nil {
		level = zerolog.InfoLevel
	}

	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "15:04:05",
		// Colors forced on: docker logs / Dozzle render ANSI fine,
		// matching the rest of the stack.
	}

	return zerolog.New(writer).Level(level).With().Timestamp().Logger()
}
