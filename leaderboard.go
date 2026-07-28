package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

type PlayerStats struct {
	Kills           uint32                      `json:"kills" binding:"required"`
	Deaths          uint32                      `json:"deaths" binding:"required"`
	Wins            uint32                      `json:"wins" binding:"required"`
	Games           uint32                      `json:"games" binding:"required"`
	TeamPointsTotal uint32                      `json:"teamPointsTotal" binding:"required"`
	Stats           map[string]uint32           `json:"stats" binding:"required"`
	ItemStats       map[string]map[int32]uint32 `json:"itemStats" binding:"required"`
}

type LeaderboardEntry struct {
	ID    uint64      `json:"id,string" binding:"required"`
	Name  *string     `json:"name"`
	Stats PlayerStats `json:"stats" binding:"required"`
}

func calcLeaderboardHandler(c *gin.Context) {
	if version := c.GetUint32("version"); version != 1 {
		abortStatusError(c, http.StatusBadRequest, ErrVersion)
		return
	}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer tx.Rollback()

	txQueryMatchAggregatePlayerStmt := tx.Stmt(queryMatchAggregatePlayerStmt)
	defer txQueryMatchAggregatePlayerStmt.Close()
	txQueryMatchAggregatePlayerStatStmt := tx.Stmt(queryMatchAggregatePlayerStatStmt)
	defer txQueryMatchAggregatePlayerStatStmt.Close()
	txQueryMatchAggregatePlayerItemStatStmt := tx.Stmt(queryMatchAggregatePlayerItemStatStmt)
	defer txQueryMatchAggregatePlayerItemStatStmt.Close()
	txQueryMatchAggregatePlayerTeamPointsStmt := tx.Stmt(queryMatchAggregatePlayerTeamPointsStmt)
	defer txQueryMatchAggregatePlayerTeamPointsStmt.Close()

	rows, err := txQueryMatchAggregatePlayerStmt.Query()
	if err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	all := []LeaderboardEntry{}
	for rows.Next() {
		var entry LeaderboardEntry

		if err := rows.Scan(&entry.ID, &entry.Name, &entry.Stats.Kills, &entry.Stats.Deaths, &entry.Stats.Wins, &entry.Stats.Games); err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		entry.Stats.Stats = map[string]uint32{}

		rows, err := txQueryMatchAggregatePlayerStatStmt.Query(entry.ID)
		if err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var key string
			var value uint32

			if err := rows.Scan(&key, &value); err != nil {
				c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
				abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
				return
			}

			entry.Stats.Stats[key] = value
		}

		if err := rows.Err(); err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		entry.Stats.ItemStats = map[string]map[int32]uint32{}
		rows, err = txQueryMatchAggregatePlayerItemStatStmt.Query(entry.ID)
		if err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var key string
			var item int32
			var value uint32

			if err := rows.Scan(&key, &item, &value); err != nil {
				c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
				abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
				return
			}

			if _, exists := entry.Stats.ItemStats[key]; !exists {
				entry.Stats.ItemStats[key] = map[int32]uint32{}
			}

			entry.Stats.ItemStats[key][item] = value
		}

		if err := rows.Err(); err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		if err := txQueryMatchAggregatePlayerTeamPointsStmt.QueryRow(entry.ID).Scan(&entry.Stats.TeamPointsTotal); err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}
		defer rows.Close()

		all = append(all, entry)
	}

	if err := rows.Err(); err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	if err := tx.Commit(); err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, all)
}
