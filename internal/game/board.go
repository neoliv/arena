package game

import "strings"

// Compact Othello board for move validation and game-end detection.
//
// DESIGN DECISION: All validation logic uses the simplest possible algorithm:
// convert bitboards to an 8x8 integer array, walk all 8 compass directions
// step by step through opponent discs until hitting own disc or edge/empty.
// No bit tricks, no parallel prefix, no shift masks, no precomputed tables.
//
// This is NOT fast — it's trivially verifiable against the literal Othello
// rules by inspection. Speed is irrelevant for the matchmaker's validation
// board (at most ~60 moves per game, ~1ms each). Correctness is paramount.
//
// All operations (LegalMoves, ApplyMove, isOver) share the single applyFlip
// function. If applyFlip is correct, everything built on it is correct.

type Board struct {
	black uint64
	white uint64
}

// NewBoard returns the standard Othello starting position.
func NewBoard() Board {
	return Board{
		black: 1<<28 | 1<<35, // E4, D5
		white: 1<<27 | 1<<36, // D4, E5
	}
}

// grid returns the board as an 8x8 array: 0=empty, 1=black, 2=white.

// setGrid sets the bitboards from an 8x8 grid.

// 8 compass directions as (dRow, dCol) pairs.
var dirs = [8][2]int{
	{0, 1},   // E
	{-1, 1},  // NE
	{-1, 0},  // N
	{-1, -1}, // NW
	{0, -1},  // W
	{1, -1},  // SW
	{1, 0},   // S
	{1, 1},   // SE
}

// inBounds returns true if (row, col) is on the 8×8 board.
func inBounds(row, col int) bool {
	return row >= 0 && row < 8 && col >= 0 && col < 8
}

// flipResult holds the new bitboards after a move.
type flipResult struct{ me, opp uint64 }

// applyFlip places a disc of color 'me' at square 'sq' and flips opponent
// discs in all 8 directions. Uses simple 8×8 array walks — no bit tricks,
// no shift masks, trivially verifiable against the literal Othello rules.
func applyFlip(me, opp uint64, sq int) flipResult {
	// Build 8x8 grid: me discs = 1, opp discs = 2, empty = 0
	row, col := sq/8, sq%8
	var board [8][8]int
	for s := 0; s < 64; s++ {
		r, c := s/8, s%8
		if me&(1<<s) != 0 {
			board[r][c] = 1
		} else if opp&(1<<s) != 0 {
			board[r][c] = 2
		}
	}

	// Place the disc.
	board[row][col] = 1

	// Walk all 8 directions.
	for _, d := range dirs {
		dr, dc := d[0], d[1]

		// Step 1: must have opponent disc adjacent.
		r2, c2 := row+dr, col+dc
		if !inBounds(r2, c2) || board[r2][c2] != 2 {
			continue
		}

		// Step 2: walk further through opponent discs.
		var toFlip []int // list of (row*8+col) indices
		toFlip = append(toFlip, r2*8+c2)
		r, c := r2+dr, c2+dc
		for inBounds(r, c) && board[r][c] == 2 {
			toFlip = append(toFlip, r*8+c)
			r += dr
			c += dc
		}

		// Step 3: must end at own disc.
		if inBounds(r, c) && board[r][c] == 1 {
			for _, sq := range toFlip {
				board[sq/8][sq%8] = 1 // flip to player
			}
		}
	}

	// Convert back to bitboards.
	me, opp = 0, 0
	for s := 0; s < 64; s++ {
		r, c := s/8, s%8
		switch board[r][c] {
		case 1:
			me |= 1 << s
		case 2:
			opp |= 1 << s
		}
	}
	return flipResult{me, opp}
}

// LegalMoves returns a bitboard of legal move destinations for player.
func (b *Board) LegalMoves(player uint64) uint64 {
	opp := b.white
	if player == b.black {
		opp = b.white
	} else {
		opp = b.black
	}
	var moves uint64
	for sq := 0; sq < 64; sq++ {
		if b.black&(1<<sq) != 0 || b.white&(1<<sq) != 0 {
			continue
		}
		r := applyFlip(player, opp, sq)
		if r.me != (player | (1 << sq)) {
			moves |= 1 << sq
		}
	}
	return moves
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

// Black returns the black disc bitboard.
func (b *Board) Black() uint64 { return b.black }

// White returns the white disc bitboard.
func (b *Board) White() uint64 { return b.white }

// Result returns disc counts and game result string.
func (b *Board) Result() (int, int, string) {
	bc, wc := Popcount(b.black), Popcount(b.white)
	if bc > wc { return bc, wc, "1-0" }
	if wc > bc { return bc, wc, "0-1" }
	return bc, wc, "1/2"
}

// IsOver returns true if neither player has a legal move.
func (b *Board) IsOver() bool {
	return b.LegalMoves(b.black) == 0 && b.LegalMoves(b.white) == 0
}

// SqFromString converts an Othello square name (e.g., "F5") to a bit index.
func SqFromString(s string) int {
	if len(s) < 2 { return -1 }
	col, row := s[0], s[1]
	if col >= 'a' && col <= 'h' { col -= 32 }
	if col < 'A' || col > 'H' || row < '1' || row > '8' { return -1 }
	return int(col-'A') + int(row-'1')*8
}

// SqToString converts a bit index to an Othello square name.
func SqToString(sq int) string {
	if sq < 0 || sq >= 64 { return "??" }
	return string([]byte{byte(sq%8) + 'A', byte(sq/8) + '1'})
}

func Popcount(x uint64) int {
	c := 0
	for x != 0 { x &= x - 1; c++ }
	return c
}

// ParseColor returns "B", "W", or "" for the given color string.
func ParseColor(s string) string {
	switch strings.ToLower(s) {
	case "b", "black": return "B"
	case "w", "white": return "W"
	}
	return ""
}
