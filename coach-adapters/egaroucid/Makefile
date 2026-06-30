# Egaroucid GTP adapter build for arena coach.
# Applies arena-stats.patch to add JSON stats output and GTP default mode.
EGAROUCID_SRC := $(CURDIR)/../../egaroucid/src

.PHONY: coach-build
coach-build:
	@echo "Building Egaroucid adapter..."
	rm -rf coach-engine
	# Apply patches (arena-stats + GTP fixes) to pristine source
	@if ! grep -q 'arena-stats v1' $(EGAROUCID_SRC)/src/console/gtp_command.hpp 2>/dev/null; then \
		echo "Applying arena-stats.patch..."; \
		patch -p1 -d $(EGAROUCID_SRC) < $(CURDIR)/arena-stats.patch; \
	fi
	@if ! grep -q 'Auto-pass for opponent' $(EGAROUCID_SRC)/src/console/gtp_command.hpp 2>/dev/null; then \
		echo "Applying gtp-fixes.patch..."; \
		patch -p1 -d $(EGAROUCID_SRC) < $(CURDIR)/gtp-fixes.patch; \
	fi
	# Force rebuild via cmake (cd to source dir first)
	rm -rf $(EGAROUCID_SRC)/build_coach
	cd $(EGAROUCID_SRC) && cmake -B build_coach -DCMAKE_BUILD_TYPE=Release -DBUILD_CONSOLE=ON -DBUILD_GUI=OFF -DBUILD_ENGINE_LIB=OFF -DBUILD_SHARED_LIBS=OFF && cmake --build build_coach --target Egaroucid_for_Console.out -j$$(nproc) || { echo "ERROR: Egaroucid build failed"; exit 1; }
	mkdir -p coach-engine/players.d
	cp $(EGAROUCID_SRC)/bin/Egaroucid_for_Console.out coach-engine/egaroucid-gtp || { echo "ERROR: Egaroucid binary not found"; exit 1; }
	cp players.d/*.yaml coach-engine/players.d/ 2>/dev/null || true
	cp -r $(EGAROUCID_SRC)/bin/resources coach-engine/ 2>/dev/null || true
	@echo "coach-engine/ ready: $$(find coach-engine -type f | wc -l) files"

.PHONY: clean
clean:
	rm -rf coach-engine $(EGAROUCID_SRC)/build_coach
