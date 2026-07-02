# Neural Reversi GTP adapter build for arena coach.
# Applies patch: JSON stats + depth overrides + GTP default + raw weight loading.
# Pre-decompresses zstd weights to avoid per-game decompression overhead.
NEURAL_SRC := $(OTHELLO_HOME)/ref/neural-reversi/src

.PHONY: coach-build
coach-build:
	@echo "Building Neural Reversi adapter..."
	rm -rf coach-engine
	@git -C $(NEURAL_SRC) checkout -- . 2>/dev/null || true
	# Apply patches (arena-stats + GTP fixes) to pristine source
	@if ! grep -q 'from_raw_reader' $(NEURAL_SRC)/crates/reversi-core/src/eval/network.rs 2>/dev/null; then \
		echo "Applying arena-stats.patch..."; \
		git -C $(NEURAL_SRC) apply $(CURDIR)/arena-stats.patch; \
	fi
	# Build CLI crate (cd to source dir first)
	cd $(NEURAL_SRC) && RUSTFLAGS="-C target-cpu=native" cargo build --release -p cli || { echo "ERROR: Neural Reversi build failed"; exit 1; }
	mkdir -p coach-engine/players.d
	cp $(NEURAL_SRC)/target/release/cli coach-engine/neural-reversi-gtp || { echo "ERROR: Neural Reversi binary not found"; exit 1; }
	# Pre-decompress weight files so engine loads raw binary (no zstd overhead)
	@for zst in $(NEURAL_SRC)/eval-e6bbc4f6.zst $(NEURAL_SRC)/eval_sm-e6bbc4f6.zst; do \
		if [ -f "$$zst" ]; then \
			bin=$$(basename "$$zst" .zst).bin; \
			echo "Decompressing $$zst -> coach-engine/$$bin..."; \
			zstd -d "$$zst" -o "coach-engine/$$bin" -f; \
		fi; \
	done
	cp players.d/*.yaml coach-engine/players.d/ 2>/dev/null || true
	@echo "coach-engine/ ready: $$(find coach-engine -type f | wc -l) files"

.PHONY: clean
clean:
	rm -rf coach-engine
