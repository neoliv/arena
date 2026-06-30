# Coach Adapters

Build wrappers for external Othello engines that don't natively support
the arena coach build convention (`make coach-build`).

## Layout

```
coach-adapters/
  builds.d/
    edax.yaml          # source: path to edax adapter
  edax/
    Makefile           # wraps ../../edax/src, produces coach-engine/
    players.d/
      edax-l1.yaml     # level 1 (weak baseline)
      edax-l5.yaml     # level 5 (moderate, ~2100 Elo)
      edax-l10.yaml    # level 10 (strong tournament, ~2300 Elo)
```

## Prerequisites

Clone upstream engines alongside this repo:

```bash
cd ~/dev/agent/othello-refs
git clone https://github.com/abulmo/edax-reversi edax
git clone git@github.com:neoliv/coach-adapters.git
```

**Edax data files**: The Edax evaluation data (`eval.dat`) is not included
in the source repo. It is downloaded automatically on first build (extracted
from the release tarball). If the download fails, download the tarball manually
from https://github.com/abulmo/edax-reversi/releases/v4.6 and extract
`data/eval.dat` to `~/dev/agent/othello-refs/edax/src/data/eval.dat`.

## Usage

Add to your coach's builds.d/:

```bash
cp ~/dev/agent/othello-refs/coach-adapters/builds.d/edax.yaml ~/coach/builds.d/
~/bin/coach-update
```

## Adding a new engine

1. Create `engine/Makefile` with `coach-build` target
2. Add `players.d/*.yaml` for each player variant  
3. Add a `builds.d/engine.yaml` entry
4. Test with `make coach-build && (cd coach-engine && ./binary -gtp)`
