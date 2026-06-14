// Package version provides build version information.
package version

// Version is the current arena version. Bump on releases.
const Version = "0.2.0"

// PrintVersion returns the version banner string.
func PrintVersion(binary string) string {
	return binary + " v" + Version + "  (Othello Arena — distributed match framework)\n"
}
