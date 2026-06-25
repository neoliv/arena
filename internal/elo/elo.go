// Package elo implements the Elo rating system for Othello engines.
package elo

import "math"

// ConfidenceInterval returns the half-width of a 95% confidence interval
// for an engine's Elo rating based on the number of games played.
// Uses a simplified approximation: CI ≈ 400 / sqrt(games).
func ConfidenceInterval(rating float64, games int) float64 {
	if games < 1 {
		games = 1
	}
	return 400.0 / math.Sqrt(float64(games))
}

// K is the Elo K-factor for established engines.
const K = 16.0

// ProvisionalK is the K-factor for engines with fewer than 20 rated games.
const ProvisionalK = 32.0

// ProvisionalGames is the number of games before an engine is "established."
const ProvisionalGames = 20

// WinProbability returns the probability that engine A beats engine B.
func WinProbability(ratingA, ratingB float64) float64 {
	return 1.0 / (1.0 + math.Pow(10.0, (ratingB-ratingA)/400.0))
}

// Update computes new ratings after a single game result.
// scoreA: 1.0 = A win, 0.5 = draw, 0.0 = A loss.
func Update(ratingA, ratingB float64, scoreA float64, gamesPlayed int) (newA, newB float64) {
	k := K
	if gamesPlayed < ProvisionalGames {
		k = ProvisionalK
	}
	expectedA := WinProbability(ratingA, ratingB)
	delta := k * (scoreA - expectedA)
	return ratingA + delta, ratingB - delta
}

// Rating represents a point-in-time Elo rating.
type Rating struct {
	EngineID   int     `json:"engine_id"`
	EngineName string  `json:"engine_name"`
	Version    string  `json:"version"`
	Elo        float64 `json:"elo"`
	Games      int     `json:"games"`
	Wins       int     `json:"wins"`
	Losses     int     `json:"losses"`
	Draws      int     `json:"draws"`
}

// HistoryEntry records an Elo change from one match.
type HistoryEntry struct {
	ID           int     `json:"id"`
	EngineID     int     `json:"engine_id"`
	OpponentID   int     `json:"opponent_id"`
	MatchID      int     `json:"match_id"`
	RatingBefore float64 `json:"rating_before"`
	RatingAfter  float64 `json:"rating_after"`
	Games        int     `json:"games"`
	Wins         int     `json:"wins"`
	Losses       int     `json:"losses"`
	Draws        int     `json:"draws"`
}
