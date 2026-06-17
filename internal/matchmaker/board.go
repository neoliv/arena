package matchmaker

// Compact Othello board for move validation and game-end detection.
// Bitboard: LSB = A1 (0), MSB = H8 (63). Row-major, little-endian.

type othelloBoard struct {
	black uint64
	white uint64
}

// Edge masks to prevent bitboard wrapping.
const (
	mmNotA    uint64 = 0xfefefefefefefefe
	mmNotH    uint64 = 0x7f7f7f7f7f7f7f7f
	mmNotRow0 uint64 = 0xffffffffffffff00
	mmNotRow7 uint64 = 0x00ffffffffffffff
)

func newBoard() othelloBoard {
	return othelloBoard{
		black: 1<<28 | 1<<35,
		white: 1<<27 | 1<<36,
	}
}

func (b *othelloBoard) legalMoves(player uint64) uint64 {
	var opp uint64
	if player == b.black {
		opp = b.white
	} else {
		opp = b.black
	}
	empty := ^(b.black | b.white)
	var moves uint64

	moves |= shiftFloodMM(player, opp, empty, 1, mmNotH)
	moves |= shiftFloodMM(player, opp, empty, -1, mmNotA)
	moves |= shiftFloodMM(player, opp, empty, 8, mmNotRow7)
	moves |= shiftFloodMM(player, opp, empty, -8, mmNotRow0)
	moves |= shiftFloodMM(player, opp, empty, 9, mmNotRow7&mmNotH)
	moves |= shiftFloodMM(player, opp, empty, -9, mmNotRow0&mmNotA)
	moves |= shiftFloodMM(player, opp, empty, -7, mmNotRow0&mmNotH)
	moves |= shiftFloodMM(player, opp, empty, 7, mmNotRow7&mmNotA)

	return moves & empty
}

func shiftFloodMM(player, opp, empty uint64, shift int, mask uint64) uint64 {
	var w uint64
	if shift > 0 {
		w = opp & ((player & mask) << shift)
		w |= opp & ((w & mask) << shift)
		w |= opp & ((w & mask) << shift)
		w |= opp & ((w & mask) << shift)
		return empty & ((w & mask) << shift)
	}
	shift = -shift
	w = opp & ((player & mask) >> shift)
	w |= opp & ((w & mask) >> shift)
	w |= opp & ((w & mask) >> shift)
	w |= opp & ((w & mask) >> shift)
	return empty & ((w & mask) >> shift)
}

func (b *othelloBoard) applyMove(player uint64, sq int) othelloBoard {
	if player == b.black {
		r := applyFlipMM(b.black, b.white, sq)
		return othelloBoard{black: r.me, white: r.opp}
	}
	r := applyFlipMM(b.white, b.black, sq)
	return othelloBoard{black: r.opp, white: r.me}
}

type flipResultMM struct{ me, opp uint64 }

func applyFlipMM(me, opp uint64, sq int) flipResultMM {
	bit := uint64(1) << sq
	me |= bit
	var flipped uint64

	dirs := []struct {
		shift int
		mask  uint64
	}{
		{1, mmNotH}, {-1, mmNotA},
		{8, mmNotRow7}, {-8, mmNotRow0},
		{9, mmNotRow7 & mmNotH}, {-9, mmNotRow0 & mmNotA},
		{-7, mmNotRow0 & mmNotH}, {7, mmNotRow7 & mmNotA},
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
	return flipResultMM{me, opp}
}

func (b *othelloBoard) isOver() bool {
	return b.legalMoves(b.black) == 0 && b.legalMoves(b.white) == 0
}

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

