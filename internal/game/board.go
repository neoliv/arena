package game

import "strings"

// Compact Othello board for move validation and game-end detection.
// Bitboard: LSB = A1 (0), MSB = H8 (63). Row-major, little-endian.

type Board struct {
	black uint64
	white uint64
}

// Edge masks to prevent bitboard wrapping. Each byte corresponds to one
// row (bits 0-7 = row 0, ..., bits 56-63 = row 7).
const (
	notA    uint64 = 0xfefefefefefefefe // exclude file A (bit 0 of each byte)
	notH    uint64 = 0x7f7f7f7f7f7f7f7f // exclude file H (bit 7 of each byte)
	notRow0 uint64 = 0xffffffffffffff00 // exclude row 0
	notRow7 uint64 = 0x00ffffffffffffff // exclude row 7
)

// NewBoard returns the standard Othello starting position.
func NewBoard() Board {
	return Board{
		black: 1<<28 | 1<<35, // E4, D5
		white: 1<<27 | 1<<36, // D4, E5
	}
}

// LegalMoves returns a bitboard of legal move destinations for player.
// Reference implementation: for each empty square, walk all 8 directions
// through opponent discs until hitting own disc (legal) or edge/empty (not).
// Trivially verifiable against the literal Othello rules — no bit tricks,
// no iteration counts to get wrong.
func (b *Board) LegalMoves(player uint64) uint64 {
	var opp uint64
	if player == b.black {
		opp = b.white
	} else {
		opp = b.black
	}
	empty := ^(b.black | b.white)
	var moves uint64
	for sq := 0; sq < 64; sq++ {
		if empty&(1<<sq) == 0 {
			continue
		}
		if isLegalMove(player, opp, sq) {
			moves |= 1 << sq
		}
	}
	return moves
}

// dir8 is the 8 compass direction offsets (row, col).
var dir8 = [8][2]int{{0, 1}, {1, 1}, {1, 0}, {1, -1}, {0, -1}, {-1, -1}, {-1, 0}, {-1, 1}}

// isLegalMove checks if placing a disc at sq is legal for player.
// Walks each direction through opponent discs; legal if the line is
// capped by the player's own disc with at least one opponent between.
func isLegalMove(player, opp uint64, sq int) bool {
	r, c := sq/8, sq%8
	for _, d := range dir8 {
		nr, nc := r+d[0], c+d[1]
		foundOpp := false
		for nr >= 0 && nr < 8 && nc >= 0 && nc < 8 {
			idx := nr*8 + nc
			if opp&(1<<idx) != 0 {
				foundOpp = true
				nr += d[0]
				nc += d[1]
				continue
			}
			if foundOpp && player&(1<<idx) != 0 {
				return true
			}
			break
		}
	}
	return false
}

// ApplyMove applies a legal move and returns the new board.
func (b *Board) ApplyMove(player uint64, sq int) Board {
	if player == b.black {
		r := applyFlip(b.black, b.white, sq)
		return Board{black: r.me, white: r.opp}
	}
	r := applyFlip(b.white, b.black, sq)
	return Board{black: r.opp, white: r.me}
}

type flipResult struct{ me, opp uint64 }

func applyFlip(me, opp uint64, sq int) flipResult {
	bit := uint64(1) << sq
	me |= bit
	var flipped uint64

	// For each direction, walk from the placed disc through opponent
	// discs until hitting own disc (capture) or empty/edge (no capture).
	dirs := []struct{ shift int; mask uint64 }{
		{1, notH},                  // E
		{-1, notA},                 // W
		{8, notRow7},              // S
		{-8, notRow0},             // N
		{9, notRow7 & notH},       // SE
		{-9, notRow0 & notA},      // NW
		{-7, notRow0 & notH},      // NE
		{7, notRow7 & notA},       // SW
	}

	for _, d := range dirs {
		mask := opp & d.mask
		if mask == 0 {
			continue
		}
		var cand uint64
		if d.shift > 0 {
			cand = (bit << d.shift) & mask
			for i := 0; i < 7; i++ {
				cand |= (cand << d.shift) & opp
			}
			if (cand << d.shift) & me != 0 {
				flipped |= cand
			}
		} else {
			sh := -d.shift
			cand = (bit >> sh) & mask
			for i := 0; i < 7; i++ {
				cand |= (cand >> sh) & opp
			}
			if (cand >> sh) & me != 0 {
				flipped |= cand
			}
		}
	}
	me |= flipped
	opp &^= flipped
	return flipResult{me, opp}
}

// IsOver returns true if neither player has a legal move.
func (b *Board) IsOver() bool {
	return b.LegalMoves(b.black) == 0 && b.LegalMoves(b.white) == 0
}

// Result computes the final game result from the board.
func (b *Board) Result() (blackCount, whiteCount int, result string) {
	bc := popcount(b.black)
	wc := popcount(b.white)
	if bc > wc {
		return bc, wc, "1-0"
	}
	if wc > bc {
		return bc, wc, "0-1"
	}
	return bc, wc, "1/2"
}

// SqFromString converts an Othello square name (e.g., "F5") to a bit index.
func SqFromString(s string) int {
	if len(s) < 2 {
		return -1
	}
	col := s[0]
	row := s[1]
	if col >= 'a' && col <= 'h' {
		col -= 32
	}
	if col < 'A' || col > 'H' || row < '1' || row > '8' {
		return -1
	}
	return int(col-'A') + int(row-'1')*8
}

// SqToString converts a bit index to an Othello square name.
func SqToString(sq int) string {
	if sq < 0 || sq >= 64 {
		return "??"
	}
	col := byte(sq%8) + 'A'
	row := byte(sq/8) + '1'
	return string([]byte{col, row})
}

func popcount(x uint64) int {
	c := 0
	for x != 0 {
		x &= x - 1
		c++
	}
	return c
}

// ParseColor returns "B", "W", or "" for the given color string.
func ParseColor(s string) string {
	switch strings.ToLower(s) {
	case "b", "black":
		return "B"
	case "w", "white":
		return "W"
	}
	return ""
}
