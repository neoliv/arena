package game

import "testing"

func TestLegalMovesOnFullBoard(t *testing.T) {
    // Simulate a full board: 17 black, 47 white, 0 empties
    s := "WWWWWWWWBWWWWWBWBWWBBBWWBWBWBWWWBWWWBWWWBWWWWBWWBWWBBWWWWWWWWWWB"
    var black, white uint64
    for i := 0; i < 64; i++ {
        switch s[i] {
        case 'B': black |= 1 << i
        case 'W': white |= 1 << i
        }
    }
    b := Board{black: black, white: white}
    
    bl := b.LegalMoves(black)
    wl := b.LegalMoves(white)
    t.Logf("Board: B=%d W=%d empties=%d", Popcount(black), Popcount(white), 64-Popcount(black|white))
    t.Logf("LegalMoves(black)=%d LegalMoves(white)=%d IsOver=%v", bl, wl, b.IsOver())
    
    if bl != 0 {
        t.Errorf("LegalMoves(black) = %d on a full board, expected 0", bl)
    }
    if wl != 0 {
        t.Errorf("LegalMoves(white) = %d on a full board, expected 0", wl)
    }
    if !b.IsOver() {
        t.Error("IsOver() should be true on a full board")
    }
    
    // Also verify that LegalMoves never returns non-zero for occupied squares
    for sq := 0; sq < 64; sq++ {
        if black&(1<<sq) != 0 || white&(1<<sq) != 0 {
            continue
        }
        // This is an empty square - check consistency
        r := applyFlip(black, white, sq)
        if r.me == (black | (1 << sq)) {
            t.Logf("  empty sq %d: no flips (illegal move)", sq)
        } else {
            t.Logf("  empty sq %d: flips happened, me=%d opp=%d", sq, r.me, r.opp)
        }
    }
}
