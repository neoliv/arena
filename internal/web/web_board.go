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

// boardInteractionJS adds board navigation via click on chart bars or move rows,
// left/right arrow keys when the board is focused, and a ply counter display.
const boardInteractionJS = `<script>
(function(){
  var cont=document.getElementById('board-container');
  var plyCtr=document.getElementById('ply-counter');
  var label=document.getElementById('board-label');
  var dataEl=document.getElementById('board-data');
  var viewer=document.getElementById('board-viewer');
  var defIdx=viewer?parseInt(viewer.getAttribute('data-default-idx')):-1;
  var maxIdx=defIdx;
  var curIdx=defIdx;
  var locked=false;

  function show(idx){
    idx=Math.max(0,Math.min(maxIdx,idx));
    var d=dataEl?dataEl.querySelector('[data-idx="'+idx+'"]'):null;
    if(!d)return;
    cont.innerHTML=d.innerHTML;
    curIdx=idx;
    var ply=idx+1;
    if(plyCtr)plyCtr.textContent=ply;
    if(label)label.textContent='Move '+ply+'/'+(maxIdx+1)+(locked?' (locked)':'');
  }

  // Click on any element with data-board-idx (chart bars + move rows)
  document.addEventListener('click',function(e){
    var el=e.target.closest('[data-board-idx]');
    if(!el)return;
    var idx=parseInt(el.getAttribute('data-board-idx'));
    if(isNaN(idx))return;
    if(locked&&curIdx===idx){locked=false;show(defIdx);}
    else{locked=true;show(idx);}
  });

  // Hover on move rows shows board without locking
  document.addEventListener('mouseover',function(e){
    if(locked)return;
    var el=e.target.closest('tr.filter-row[data-board-idx]');
    if(!el)return;
    var idx=parseInt(el.getAttribute('data-board-idx'));
    if(!isNaN(idx))show(idx);
  });

  // Mouseleave from viewer restores default
  viewer&&viewer.addEventListener('mouseleave',function(){if(!locked)show(defIdx);});

  // Arrow keys when board container is focused
  cont&&cont.addEventListener('keydown',function(e){
    if(e.key==='ArrowLeft'){e.preventDefault();show(curIdx-1);}
    else if(e.key==='ArrowRight'){e.preventDefault();show(curIdx+1);}
  });

  // Click on board itself focuses it for arrow keys
  cont&&cont.addEventListener('click',function(e){
    if(!e.target.closest('[data-board-idx]'))cont.focus();
  });
})();
</script>`
