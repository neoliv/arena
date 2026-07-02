# Edax GTP adapter build for arena coach.
# Compiles Edax as a library and links our GTP adapter with JSON stats.
EDAX_SRC := $(OTHELLO_HOME)/ref/edax/src
DATA_URL := https://github.com/abulmo/edax-reversi/releases/download/v4.6
TARBALL  := $(EDAX_SRC)/data/edax-release.tar.gz

$(EDAX_SRC)/data/eval.dat:
	@echo "Downloading Edax evaluation data..."
	@mkdir -p $(EDAX_SRC)/data
	@curl -sL "$(DATA_URL)/edax-4.6-linux-x86.tar.gz" -o $(TARBALL) || { \
		echo "ERROR: Failed to download Edax release tarball."; \
		echo "Download manually from $(DATA_URL)"; \
		echo "Extract data/eval.dat to $(EDAX_SRC)/data/"; \
		rm -f $(TARBALL); exit 1; }
	@tar -xzf $(TARBALL) -C $(EDAX_SRC)/data data/eval.dat --strip-components=1 2>/dev/null || { \
		echo "ERROR: Failed to extract eval.dat from tarball."; \
		rm -f $(TARBALL); exit 1; }
	@rm -f $(TARBALL)
	@echo "eval.dat ready."

# Use Edax's own build system but compile adapter.c INSTEAD of edax.c+gtp.c.
# First copy adapter.c, then compile with the same all.c approach.
.PHONY: coach-build
coach-build: $(EDAX_SRC)/data/eval.dat
	@echo "Building Edax adapter..."
	rm -rf coach-engine
	mkdir -p coach-engine/players.d coach-engine/data
	# Copy adapter to edax src dir, then compile edax-style
	cp $(CURDIR)/adapter.c $(EDAX_SRC)/src/adapter_main.c
	rm -f $(EDAX_SRC)/bin/lEdax-native-adapter  # force clean rebuild
	cd $(EDAX_SRC)/src && CC=gcc BUILD=optimize ARCH=native $(MAKE) adapter-build || { echo "ERROR: Edax build failed"; exit 1; }
	cp $(EDAX_SRC)/bin/lEdax-native-adapter coach-engine/edax || { echo "ERROR: Edax binary not found"; exit 1; }
	cp $(EDAX_SRC)/data/eval.dat coach-engine/data/
	cp players.d/*.yaml coach-engine/players.d/ 2>/dev/null || true
	@echo "coach-engine/ ready: $$(find coach-engine -type f | wc -l) files"

.PHONY: clean
clean:
	rm -rf coach-engine $(EDAX_SRC)/data/eval.dat $(TARBALL)
