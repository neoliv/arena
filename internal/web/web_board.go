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

	svg.WriteString(`<svg viewBox="-36 -24 836 860" role="img" aria-label="Othello board`)
	fmt.Fprintf(&svg, `: Black %d, White %d" style="display:block">`, bCount, wCount)

	// Board background
	svg.WriteString(`<rect x="0" y="0" width="800" height="800" fill="#1a5c3a" rx="8"/>`)

	// Column labels (A-H) — at top of board
	for col := 0; col < 8; col++ {
		fmt.Fprintf(&svg, `<text x="%d" y="-4" text-anchor="middle" font-family="system-ui,sans-serif" font-size="24" font-weight="700" fill="#fff">%c</text>`,
			col*100+50, 'A'+col)
	}
	// Row labels (1-8) — 1 at top, 8 at bottom
	for row := 0; row < 8; row++ {
		fmt.Fprintf(&svg, `<text x="-4" y="%d" text-anchor="end" font-family="system-ui,sans-serif" font-size="24" font-weight="700" fill="#fff">%d</text>`,
			row*100+64, row+1)
	}

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
	fmt.Fprintf(&svg, `<text x="400" y="835" text-anchor="middle" font-family="system-ui,sans-serif" font-size="22" fill="#22d3ee" font-weight="600">B:%d  <tspan fill="#d4c4a8">W:%d</tspan></text>`,
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
    // Highlight chart bar for current ply
    var bars=document.querySelectorAll('rect[data-board-idx]');
    for(var i=0;i<bars.length;i++){
      var m=parseInt(bars[i].getAttribute('data-board-idx'))===idx;
      bars[i].style.stroke=m?'#ff0':'none';
      bars[i].style.strokeWidth=m?'2':'0';
    }
    // Move triangle marker under current bar
    var tri=document.getElementById('ply-triangle');
    if(tri){
      var bar=document.querySelector('rect[data-board-idx="'+idx+'"]');
      if(bar){
        var oldT=tri.getAttribute('transform')||'';
        var yM=oldT.match(/translate\([^,]+,([^)]+)\)/);
        tri.setAttribute('transform','translate('+(parseInt(bar.getAttribute('x'))+2)+','+(yM?yM[1]:'0')+')');
        tri.style.display='block';
      }else{tri.style.display='none';}
    }
    // Update move text + stats from chart tooltip
    var tt=document.querySelector('[data-board-idx="'+idx+'"] title');
    if(tt){
      var parts=tt.textContent.split(':');
      var mp=(parts[0]||'').split(' ');
      var mt=document.getElementById('mv-text');
      var ms=document.getElementById('mv-score');
      var mst=document.getElementById('mv-stats');
      if(mt)mt.textContent=mp.length>1?mp[1]:'';
      if(ms){
        var bar2=document.querySelector('rect[data-board-idx="'+idx+'"]');
        var sc=bar2?parseInt(bar2.getAttribute('data-score')):0;
        if(!isNaN(sc)){
          ms.textContent=(sc>0?'+':'')+sc;
          ms.style.color=sc>0?'#4caf50':sc<0?'#f44336':'var(--muted)';
        }
      }
      if(mst)mst.textContent=parts.length>1?parts[1].trim():'';
    } else {
      // No chart bar for this ply (opening move) — read from board data
      var bd=document.querySelector('#board-data [data-idx="'+idx+'"]');
      if(bd){
        var mv=bd.getAttribute('data-move')||'';
        var mt=document.getElementById('mv-text');
        var ms=document.getElementById('mv-score');
        var mst=document.getElementById('mv-stats');
        if(mt)mt.textContent=mv;
        if(ms){ms.textContent='';ms.style.color='var(--muted)';}
        if(mst)mst.textContent='';
      }
    }
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

  // Arrow buttons
  var btnPrev=document.getElementById('btn-prev');
  var btnNext=document.getElementById('btn-next');
  btnPrev&&btnPrev.addEventListener('click',function(e){e.stopPropagation();locked=true;show(curIdx-1);});
  btnNext&&btnNext.addEventListener('click',function(e){e.stopPropagation();locked=true;show(curIdx+1);});

  // Arrow keys
  document.addEventListener('keydown',function(e){
    if(e.key==='ArrowLeft'){e.preventDefault();locked=true;show(curIdx-1);}
    else if(e.key==='ArrowRight'){e.preventDefault();locked=true;show(curIdx+1);}
  });
})();
</script>`
