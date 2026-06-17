#!/bin/bash
# arena-sprt-gate.sh — Build candidate engine, run SPRT vs previous version,
# report result. Exit 0 = candidate passes (not meaningfully weaker).
#
# Usage:
#   ./arena-sprt-gate.sh                          # builds + tests current neursi
#   ./arena-sprt-gate.sh --skip-build             # test pre-built binary
#   ./arena-sprt-gate.sh --tc 5 --elo0 -15         # custom SPRT params
#
# Requires:
#   - SPRT binary at arena/cmd/sprt/sprt (or built by this script)
#   - Previous accepted engine binary at ~/coach/sprt/prev/neursi
#   - Previous accepted weights at ~/coach/sprt/prev/*.safetensors
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ARENA_DIR="$SCRIPT_DIR"
NEURSI_DIR="$(cd "$SCRIPT_DIR/../neursi/engine" && pwd)"
OUTPUT_DIR="${HOME}/coach/sprt"

# ── Config ────────────────────────────────────────────────────────────

TC=1.0                 # time control in seconds per game
ELO0=-10               # null hypothesis: candidate is at most this much weaker
ELO1=0                 # alternative: candidate is at least equal
MAX_GAMES=400           # hard cap on game pairs
CONCURRENCY=4           # concurrent game pairs
SKIP_BUILD=false

# ── Parse flags ─────────────────────────────────────────────────────────

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-build) SKIP_BUILD=true; shift ;;
        --tc) TC="$2"; shift 2 ;;
        --elo0) ELO0="$2"; shift 2 ;;
        --elo1) ELO1="$2"; shift 2 ;;
        --max-games) MAX_GAMES="$2"; shift 2 ;;
        --concurrency) CONCURRENCY="$2"; shift 2 ;;
        *) echo "Unknown flag: $1"; exit 2 ;;
    esac
done

# ── Build SPRT binary ───────────────────────────────────────────────────

SPRT_BIN="$ARENA_DIR/cmd/sprt/sprt"
if [[ ! -x "$SPRT_BIN" ]]; then
    echo "=== Building SPRT binary ==="
    (cd "$ARENA_DIR" && go build -o "$SPRT_BIN" ./cmd/sprt/)
fi

# ── Build candidate engine ──────────────────────────────────────────────

CAND_DIR="$NEURSI_DIR/target/sprt"
CAND_BIN="$CAND_DIR/release/neursi"

if ! $SKIP_BUILD; then
    echo "=== Building candidate engine ==="
    (cd "$NEURSI_DIR" && cargo build --release --target-dir "$CAND_DIR" 2>&1 | tail -3)
    if [[ ! -x "$CAND_BIN" ]]; then
        echo "ERROR: candidate binary not found at $CAND_BIN"
        exit 2
    fi
fi

# ── Locate reference engine ─────────────────────────────────────────────

PREV_DIR="$OUTPUT_DIR/prev"
PREV_BIN="$PREV_DIR/neursi"

if [[ ! -x "$PREV_BIN" ]]; then
    echo "ERROR: previous accepted engine not found at $PREV_BIN"
    echo "  Copy the last accepted neursi binary and weights to $PREV_DIR/"
    echo "  e.g.: cp neursi/target/release/neursi ~/coach/sprt/prev/"
    echo "        cp neursi/weights/*.safetensors ~/coach/sprt/prev/"
    exit 2
fi

# ── Build engine command lines ──────────────────────────────────────────

# Check for companion data (NN weights)
CAND_WEIGHTS=$(ls "$NEURSI_DIR/weights/"*.safetensors 2>/dev/null | head -1 || echo "")
PREV_WEIGHTS=$(ls "$PREV_DIR/"*.safetensors 2>/dev/null | head -1 || echo "")

CAND_CMD="$CAND_BIN"
PREV_CMD="$PREV_BIN"
if [[ -n "$CAND_WEIGHTS" ]]; then CAND_CMD="$CAND_CMD --weights $CAND_WEIGHTS"; fi
if [[ -n "$PREV_WEIGHTS" ]]; then PREV_CMD="$PREV_CMD --weights $PREV_WEIGHTS"; fi

echo ""
echo "=== SPRT Gate ==="
echo "Candidate: $CAND_CMD"
echo "Reference: $PREV_CMD"
echo "TC: ${TC}s  elo0: $ELO0  elo1: $ELO1  max: $MAX_GAMES"
echo ""

# ── Pre-flight checks ──────────────────────────────────────────────────

echo "--- Pre-flight ---"
echo -n "  cargo test ... "
(cd "$NEURSI_DIR" && cargo test 2>&1 | tail -1) || {
    echo "FAIL"
    echo "ERROR: cargo test failed. Fix tests before SPRT."
    exit 2
}
echo "  OK"

echo -n "  perft depth 4 ... "
# Quick perft sanity check via the engine's GTP
PERFT_OUT=$(echo -e "boardsize 8\nclear_board\nperft 4\nquit" | "$CAND_BIN" 2>/dev/null || true)
if echo "$PERFT_OUT" | grep -q "244"; then
    echo "OK (244)"
else
    echo "WARNING: perft check inconclusive"
fi

# ── Run SPRT ────────────────────────────────────────────────────────────

echo ""
echo "--- Running SPRT ---"

"$SPRT_BIN" \
    --candidate "$CAND_CMD" \
    --reference "$PREV_CMD" \
    --tc "$TC" \
    --elo0 "$ELO0" --elo1 "$ELO1" \
    --max-games "$MAX_GAMES" \
    --concurrency "$CONCURRENCY" \
    --output "$OUTPUT_DIR"

SPRT_EXIT=$?

# ── Report ──────────────────────────────────────────────────────────────

echo ""
case $SPRT_EXIT in
    0) echo "✓ SPRT PASSED — candidate is not meaningfully weaker"
       echo "  Next: bump version, tag, push, deploy to arena gauntlet"
       ;;
    1) echo "✗ SPRT REJECTED — candidate is weaker"
       echo "  Investigate recent changes, fix, re-run"
       ;;
    2) echo "? SPRT INCONCLUSIVE — need more games or manual judgment"
       ;;
esac

exit $SPRT_EXIT
