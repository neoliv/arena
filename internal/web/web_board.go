package web

import (
	"fmt"
	"strings"

	"github.com/neoliv/arena/internal/game"
)

// boardState holds the board position after a single move (opening or engine).
type boardState struct {
	board  game.Board // authoritative framework board
	lastSq int        // bit index of the last move, -1 for PASS or initial
	side   string     // "b" or "w" — who just moved
}

// renderBoardSVG returns an inline SVG rendering of the board at 800×800
// viewBox. lastSq >= 0 highlights the most recent move with a yellow border.
func renderBoardSVG(b game.Board, lastSq int) string {
	var svg strings.Builder
	blk, wht := b.Black(), b.White()
	bCount, wCount := game.Popcount(blk), game.Popcount(wht)

	svg.WriteString(`<svg viewBox="0 0 800 800" role="img" aria-label="Othello board`)
	fmt.Fprintf(&svg, `: Black %d, White %d" style="display:block">`, bCount, wCount)

	// Board background
	svg.WriteString(`<rect x="0" y="0" width="800" height="800" fill="#1a5c3a" rx="8"/>`)

	// 64 squares
	for row := 0; row < 8; row++ {
		for col := 0; col < 8; col++ {
			x := col * 100
			y := row * 100
			bg := "#2a6a3a"
			if (row+col)%2 == 1 {
				bg = "#225a30"
			}
			fmt.Fprintf(&svg, `<rect x="%d" y="%d" width="100" height="100" fill="%s"/>`, x, y, bg)
		}
	}

	// Discs
	for sq := 0; sq < 64; sq++ {
		row, col := sq/8, sq%8
		cx := col*100 + 50
		cy := row*100 + 50
		if blk&(1<<sq) != 0 {
			fmt.Fprintf(&svg, `<circle cx="%d" cy="%d" r="38" fill="#22d3ee"/>`, cx, cy)
		} else if wht&(1<<sq) != 0 {
			fmt.Fprintf(&svg, `<circle cx="%d" cy="%d" r="38" fill="#d4c4a8"/>`, cx, cy)
		}
	}

	// Last-move highlight
	if lastSq >= 0 && lastSq < 64 {
		col, row := lastSq%8, lastSq/8
		fmt.Fprintf(&svg, `<rect x="%d" y="%d" width="100" height="100" fill="none" stroke="#ffeb3b" stroke-width="4"/>`,
			col*100, row*100)
	}

	// Disc count
	fmt.Fprintf(&svg, `<text x="400" y="780" text-anchor="middle" font-family="system-ui,sans-serif" font-size="22" fill="#22d3ee" font-weight="600">B:%d  <tspan fill="#d4c4a8">W:%d</tspan></text>`,
		bCount, wCount)

	svg.WriteString(`</svg>`)
	return svg.String()
}

// boardInteractionJS is the JavaScript snippet for hovering move rows to
// swap the board SVG and clicking to lock/unlock the view.
const boardInteractionJS = `<script>
(function(){
  var rows=document.querySelectorAll('tr.filter-row[data-board-idx]');
  var cont=document.getElementById('board-container');
  var label=document.getElementById('board-label');
  var dataEl=document.getElementById('board-data');
  var viewer=document.getElementById('board-viewer');
  var defIdx=viewer?parseInt(viewer.getAttribute('data-default-idx')):-1;
  var locked=false;
  function show(idx){
    var d=dataEl?dataEl.querySelector('[data-idx="'+idx+'"]'):null;
    if(!d)return;
    cont.innerHTML=d.innerHTML;
    var ply=parseInt(idx)+1;
    label.textContent='Move: '+ply+(locked?' (locked — click again to unlock)':'');
  }
  rows.forEach(function(r){
    r.addEventListener('mouseenter',function(){if(!locked)show(parseInt(this.getAttribute('data-board-idx')));});
  });
  viewer&&viewer.addEventListener('mouseleave',function(){if(!locked&&defIdx>=0)show(defIdx);});
  rows.forEach(function(r){
    r.addEventListener('click',function(){
      var idx=parseInt(this.getAttribute('data-board-idx'));
      if(locked&&cont.getAttribute('data-locked-idx')==idx){
        locked=false;cont.removeAttribute('data-locked-idx');show(defIdx);
      }else{locked=true;cont.setAttribute('data-locked-idx',idx);show(idx);}
    });
  });
})();
</script>`
