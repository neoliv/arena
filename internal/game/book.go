package game

import (
	"strings"
)

// Opening represents one opening book line.
type Opening struct {
	Line  string // raw move string, e.g. "f5d6c3d3c4"
	Moves []string
}

// LoadBook parses opening lines from embedded book data or an external file.
// Lines starting with # are comments. Empty lines are skipped.
func LoadBook(data string) []Opening {
	var openings []Opening
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		moves := parseMoveList(line)
		if len(moves) == 0 {
			continue
		}
		openings = append(openings, Opening{Line: line, Moves: moves})
	}
	if len(openings) == 0 {
		// Fallback: empty opening (start from initial position)
		openings = append(openings, Opening{})
	}
	return openings
}

// parseMoveList splits a continuous move string (e.g. "f5d6c3d3") into
// individual 2-char moves. Returns uppercase moves.
func parseMoveList(line string) []string {
	if line == "" {
		return nil
	}
	var moves []string
	for i := 0; i < len(line); i += 2 {
		if i+1 < len(line) {
			moves = append(moves, strings.ToUpper(line[i:i+2]))
		}
	}
	return moves
}
