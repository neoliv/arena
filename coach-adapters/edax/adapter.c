/*
 * edax-adapter — GTP adapter for Edax 4.6 with JSON stats + board tracing.
 * Links directly against Edax search library. No stdout/stderr parsing.
 *
 * Args: -d N (midgame depth) -es N (ES threshold) -nobook -book <file> -t N -g N
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdbool.h>
#include <stdint.h>
#include <time.h>
#include <unistd.h>

#include "board.h"
#include "search.h"
#include "play.h"
#include "book.h"
#include "util.h"
#include "options.h"
#include "ui.h"

static Play  edax_play[1];
static Book  edax_book[1];
static Board edax_board[1];
static UI    edax_ui[1];

static int   mid_depth     = 0;
static int   end_threshold = 15;
static char *book_file     = NULL;
static bool  no_book       = false;
static bool  timer_set     = false;
static int64_t game_time_ms = 0;
static int64_t total_think_ms = 0;
static int   game_id       = 0;   // -g N  for trace correlation

static void gtp_loop(void);

int main(int argc, char **argv) {
    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "-d") && i + 1 < argc) mid_depth = atoi(argv[++i]);
        else if (!strcmp(argv[i], "-es") && i + 1 < argc) end_threshold = atoi(argv[++i]);
        else if (!strcmp(argv[i], "-book") && i + 1 < argc) book_file = argv[++i];
        else if (!strcmp(argv[i], "-nobook")) no_book = true;
        else if (!strcmp(argv[i], "-t") && i + 1 < argc) { game_time_ms = (int64_t)(atof(argv[++i]) * 1000.0); timer_set = true; }
        else if (!strcmp(argv[i], "-g") && i + 1 < argc) game_id = atoi(argv[++i]);
    }
    g_midgame_to_endgame = end_threshold;
    options_bound();
    edge_stability_init();
    statistics_init();
    eval_open(options.eval_file);
    search_global_init();
    options.book_file = book_file;
    if (mid_depth > 0) {
        for (int e = 0; e <= 60; e++) { LEVEL[mid_depth][e].depth = mid_depth; LEVEL[mid_depth][e].selectivity = 0; }
        options.level = mid_depth;
    }
    if (!no_book) {
        if (!book_file) book_file = "data/book.dat";
        book_init(edax_book); book_load(edax_book, book_file);
    }
    board_init(edax_board); board_init(&edax_play->initial_board);
    memset(edax_ui, 0, sizeof(UI));
    edax_play->book = no_book ? NULL : edax_book;
    options.book_allowed = !no_book;
    play_init(edax_play, edax_play->book);
    if (no_book) { options.book_allowed = false; options.book_randomness = 0; }
    gtp_loop();
    return 0;
}

static void gtp_reply(const char *s) { printf("= %s\n\n", s); fflush(stdout); }
static void gtp_error(const char *s) { printf("? %s\n\n", s); fflush(stdout); }

static char move_str[4];
static char *sq_to_str(int sq) {
    if (sq < 0 || sq >= 64) return "PASS";
    move_str[0] = (char)('A' + (sq % 8)); move_str[1] = (char)('1' + (sq / 8)); move_str[2] = '\0';
    return move_str;
}

static void emit_stats(int64_t elapsed_ms, bool timed_out) {
    Result *r = &edax_play->result;
    fprintf(stdout, "# arena-stats v1: {\"nodes\":%" PRIu64 ",\"depth\":%d,\"score\":%d,\"time_ms\":%lld,\"timeout\":%s}\n",
            r->n_nodes, r->depth, r->score, (long long)elapsed_ms, timed_out ? "true" : "false");
    fflush(stdout);
}

/// Dump board state as `# board:<game_id>:<64-char> key=value`
/// 64-char string: '.' = empty, 'B' = player disc, 'W' = opponent disc
/// Iterates A1,B1,…,H1, A2,B2,…,H2, … A8,…,H8 (square order 0–63).
static void dump_board(const char *label) {
    const Board *b = &edax_play->board;
    char buf[128];
    int pos = 0;
    for (int sq = 0; sq < 64; sq++) {
        if (b->player & (1ULL << sq))      buf[pos++] = 'B';
        else if (b->opponent & (1ULL << sq)) buf[pos++] = 'W';
        else                                 buf[pos++] = '.';
    }
    buf[pos] = '\0';
    const char *side = (edax_play->player == 0) ? "BLACK" : "WHITE";
    fprintf(stdout, "# board:%d:%s label=%s side=%s\n", game_id, buf, label, side);
    fflush(stdout);
}

static void dump_incoming(const char *cmd) {
    fprintf(stderr, "[adapter gid=%d] << %s\n", game_id, cmd);
}

static void gtp_loop(void) {
    char line[4096];
    while (fgets(line, sizeof(line), stdin)) {
        size_t len = strlen(line);
        while (len > 0 && (line[len-1] == '\n' || line[len-1] == '\r')) line[--len] = '\0';
        if (len == 0 || line[0] == '#') continue;
        char *cmd = line;
        { char *p = line; while (*p >= '0' && *p <= '9') p++; if (p > line && *p == ' ') { *p = '\0'; cmd = p + 1; } }
        dump_incoming(cmd);
        if (!strcmp(cmd, "boardsize 8")) { gtp_reply(""); }
        else if (!strcmp(cmd, "clear_board")) {
            board_init(edax_board); edax_play->initial_board = *edax_board;
            play_init(edax_play, edax_play->book);
            if (mid_depth > 0) options.level = mid_depth;
            if (no_book) { options.book_allowed = false; options.book_randomness = 0; }
            total_think_ms = 0;
            dump_board("after clear_board");
            gtp_reply("");
        } else if (!strcmp(cmd, "quit")) { gtp_reply(""); break; }
        else if (!strcmp(cmd, "name")) { gtp_reply("Edax 4.6 (adapter)"); }
        else if (!strcmp(cmd, "version")) { gtp_reply("4.6"); }
        else if (!strncmp(cmd, "play ", 5)) {
            // Original Edax logic: verify color matches current player.
            char pcolor = cmd[5];
            int expected = (pcolor == 'b' || pcolor == 'B') ? 0 : (pcolor == 'w' || pcolor == 'W') ? 1 : -1;
            if (expected < 0) { gtp_error("syntax error (wrong or missing color)"); continue; }
            if (expected != edax_play->player) {
                if (play_must_pass(edax_play)) {
                    play_move(edax_play, PASS);
                } else {
                    gtp_error(edax_play->player == 0 ? "wrong color, expected b" : "wrong color, expected w");
                    continue;
                }
            }
            int ok = play_user_move(edax_play, cmd + 7);
            dump_board(cmd);
            if (!ok) { gtp_error("illegal move"); continue; }
            gtp_reply("");
        } else if (!strncmp(cmd, "genmove ", 8)) {
            if (play_must_pass(edax_play)) {
                play_move(edax_play, PASS);  // apply pass to flip player (original Edax does this)
                dump_board("forced pass");
                gtp_reply("PASS"); continue;
            }
            if (mid_depth > 0) options.level = mid_depth;
            struct timespec t0, t1;
            int stdout_backup = dup(STDOUT_FILENO);
            freopen("/dev/null", "w", stdout);
            clock_gettime(CLOCK_MONOTONIC, &t0); play_go(edax_play, true); clock_gettime(CLOCK_MONOTONIC, &t1);
            fflush(stdout);
            dup2(stdout_backup, STDOUT_FILENO);
            close(stdout_backup);
            int64_t elapsed_ms = (t1.tv_sec - t0.tv_sec) * 1000 + (t1.tv_nsec - t0.tv_nsec) / 1000000;
            Move *last = play_get_last_move(edax_play);
            total_think_ms += elapsed_ms;
            bool timeout = timer_set && total_think_ms > game_time_ms * 105 / 100;
            emit_stats(elapsed_ms, timeout);
            if (last->x == PASS) {
                play_move(edax_play, PASS);  // apply pass to flip player after search-returned pass
            } else if (play_must_pass(edax_play)) {
                // Opponent has no legal moves after our move → auto-pass
                // to keep internal player in sync with the game flow.
                play_move(edax_play, PASS);
            }
            char buf[64]; snprintf(buf, sizeof(buf), "after genmove %s", cmd + 8);
            dump_board(buf);
            gtp_reply(sq_to_str(last->x));
        } else if (!strncmp(cmd, "game_time ", 10)) { game_time_ms = (int64_t)(atof(cmd + 10) * 1000.0); timer_set = true; gtp_reply(""); }
        else { gtp_error("unknown command"); }
    }
}
