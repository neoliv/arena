package matchmaker

// Minimal Othello board for independent move validation and game-end detection.
// The matchmaker uses this to verify moves and detect when the game is over,
// rather than relying on engines to report passes correctly.

type othelloBoard struct {
	black uint64
	white uint64
}

func newBoard() othelloBoard {
	// Standard starting position
	return othelloBoard{
		black: 1<<27 | 1<<36, // D5, E4
		white: 1<<28 | 1<<35, // D4, E5
	}
}

// legalMoves returns a bitboard of legal moves for the given player.
func (b *othelloBoard) legalMoves(player uint64) uint64 {
	opp := b.white
	if player == b.white {
		opp = b.black
	}
	empty := ^(b.black | b.white)
	var moves, candidates uint64

	// Kogge-Stone: compute legal moves by checking all 8 directions.
	dirs := []int{1, 7, 8, 9} // horizontal, diagonal up, vertical, diagonal down
	for _, d := range dirs {
		for _, sign := range []int{1, -1} {
			shift := d * sign
			var mask uint64
			if shift > 0 {
				// Prevent wrapping
				mask = opp & 0x7e7e7e7e7e7e7e7e
				if d == 8 {
					mask = opp
				}
				candidates = (player << shift) & mask
				for i := 0; i < 5; i++ {
					candidates |= (candidates << shift) & opp
				}
				moves |= (candidates << shift) & empty
			} else if shift < 0 {
				shift = -shift
				mask = opp & 0x7e7e7e7e7e7e7e7e
				if d == 8 {
					mask = opp
				}
				candidates = (player >> shift) & mask
				for i := 0; i < 5; i++ {
					candidates |= (candidates >> shift) & opp
				}
				moves |= (candidates >> shift) & empty
			}
		}
	}
	// Also check down-right and up-left (9 and -9)
	for _, shift := range []int{9, -9} {
		mask := opp & 0x7e7e7e7e7e7e7e7e
		if shift > 0 {
			candidates = (player << shift) & mask
			for i := 0; i < 5; i++ {
				candidates |= (candidates << shift) & opp
			}
			moves |= (candidates << shift) & empty
		} else {
			candidates = (player >> (-shift)) & mask
			for i := 0; i < 5; i++ {
				candidates |= (candidates >> (-shift)) & opp
			}
			moves |= (candidates >> (-shift)) & empty
		}
	}
	return moves & empty
}

// applyMove applies a move for the given player. Returns the new board.
// Caller must ensure the move is legal.
func (b *othelloBoard) applyMove(player uint64, sq int) othelloBoard {
	if player == b.black {
		return b.apply(b.black, b.white, sq)
	}
	return b.apply(b.white, b.black, sq)
}

func (b othelloBoard) apply(me, opp uint64, sq int) othelloBoard {
	bit := uint64(1) << sq
	me |= bit
	var flipped uint64
	for _, d := range []int{1, 7, 8, 9} {
		for _, sign := range []int{1, -1} {
			shift := d * sign
			var cand uint64
			if shift > 0 {
				cand = (bit << shift) & opp
				for i := 0; i < 7; i++ {
					cand |= (cand << shift) & opp
				}
				if (cand << shift) & me != 0 {
					flipped |= cand
				}
			} else {
				shift = -shift
				cand = (bit >> shift) & opp
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
	return othelloBoard{black: me, white: opp}
}

// sqFromString converts an Othello square name (e.g., "F5") to a bit index.
func sqFromString(s string) int {
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

// isOver returns true if neither player has a legal move.
func (b *othelloBoard) isOver() bool {
	return b.legalMoves(b.black) == 0 && b.legalMoves(b.white) == 0
}
