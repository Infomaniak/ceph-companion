// Package colors provides the ANSI color codes used for terminal output.
package colors

const (
	Reset  = "\033[0m"
	Red    = "\033[91m"
	Green  = "\033[92m"
	Yellow = "\033[93m"
	Purple = "\033[95m"
	Cyan   = "\033[96m"
	Bold   = "\033[1m"
)

// Wrap surrounds text with the given color and a trailing Reset.
func Wrap(text, color string) string {
	return color + text + Reset
}
