package game

import "strings"

// Compact Othello board for move validation and game-end detection.
// Allows the match runner / SPRT tool to independently verify moves
// and detect passes without trusting the engine.

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

// LegalMoves returns a bitboard of legal move destinations for player.
func (b *Board) LegalMoves(player uint64) uint64 {
	var opp uint64
	if player == b.black {
		opp = b.white
	} else {
		opp = b.black
	}
	empty := ^(b.black | b.white)
	var moves, candidates uint64

	edge := uint64(0x7e7e7e7e7e7e7e7e)
	dirs := []struct{ shift int; mask uint64 }{
		{1, edge},   // E
		{7, edge},   // SW (<<7) / NE (>>7)
		{8, ^uint64(0)}, // S
		{9, edge},   // SE (<<9) / NW (>>9)
	}

	for _, d := range dirs {
		for _, sign := range []int{1, -1} {
			shift := d.shift * sign
			mask := opp & d.mask
			if shift > 0 {
				candidates = (player << shift) & mask
				for i := 0; i < 5; i++ {
					candidates |= (candidates << shift) & opp
				}
				moves |= (candidates << shift) & empty
			} else {
				shift = -shift
				candidates = (player >> shift) & mask
				for i := 0; i < 5; i++ {
					candidates |= (candidates >> shift) & opp
				}
				moves |= (candidates >> shift) & empty
			}
		}
	}
	return moves & empty
}

// ApplyMove applies a legal move and returns the new board.
func (b *Board) ApplyMove(player uint64, sq int) Board {
	if player == b.black {
		r := b.apply(b.black, b.white, sq)
		return Board{black: r.black, white: r.white}
	}
	r := b.apply(b.white, b.black, sq)
	return Board{black: r.white, white: r.black}
}

func (b Board) apply(me, opp uint64, sq int) Board {
	bit := uint64(1) << sq
	me |= bit
	edge := uint64(0x7e7e7e7e7e7e7e7e)
	var flipped uint64
	dirs := []struct{ shift int; mask uint64 }{
		{1, edge}, {7, edge}, {8, ^uint64(0)}, {9, edge},
	}
	for _, d := range dirs {
		for _, sign := range []int{1, -1} {
			shift := d.shift * sign
			var cand uint64
			if shift > 0 {
				mask := opp & d.mask
				if d.shift == 8 {
					mask = opp
				}
				cand = (bit << shift) & mask
				for i := 0; i < 7; i++ {
					cand |= (cand << shift) & opp
				}
				if (cand << shift) & me != 0 {
					flipped |= cand
				}
			} else {
				shift = -shift
				mask := opp & d.mask
				if d.shift == 8 {
					mask = opp
				}
				cand = (bit >> shift) & mask
				for i := 0; i < 7; i++ {
					cand |= (cand >> shift) & opp
				}
				if (cand >> shift) & me != 0 {
					flipped |= cand
				}
			}
		}
	}
	me |= flipped
	opp &^= flipped
	return Board{black: me, white: opp}
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
