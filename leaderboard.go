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
	TeamPointsTotal uint32                      `json:"teamPointsTotal,omitempty"`
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

	type requestQuery struct {
		GameMode *string `form:"gameMode"`
	}
	var query requestQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		abortStatusError(c, http.StatusBadRequest, fmt.Errorf("%w: %w", ErrMalformedQuery, err))
		return
	}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	txQueryMatchAggregatePlayerStmt := tx.Stmt(queryMatchAggregatePlayerStmt)
	defer txQueryMatchAggregatePlayerStmt.Close()
	txQueryMatchAggregatePlayerStatStmt := tx.Stmt(queryMatchAggregatePlayerStatStmt)
	defer txQueryMatchAggregatePlayerStatStmt.Close()
	txQueryMatchAggregatePlayerItemStatStmt := tx.Stmt(queryMatchAggregatePlayerItemStatStmt)
	defer txQueryMatchAggregatePlayerItemStatStmt.Close()

	rows, err := txQueryMatchAggregatePlayerStmt.Query(query.GameMode)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	all := []LeaderboardEntry{}
	for rows.Next() {
		var entry LeaderboardEntry

		if err := rows.Scan(&entry.ID, &entry.Name, &entry.Stats.Kills, &entry.Stats.Deaths, &entry.Stats.Wins, &entry.Stats.Games); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		entry.Stats.Stats = map[string]uint32{}

		rows, err := txQueryMatchAggregatePlayerStatStmt.Query(query.GameMode, entry.ID)
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var key string
			var value uint32

			if err := rows.Scan(&key, &value); err != nil {
				err = fmt.Errorf("%w: %w", ErrDatabase, err)
				c.Error(err)
				abortStatusError(c, http.StatusInternalServerError, err)
				return
			}

			entry.Stats.Stats[key] = value
		}

		if err := rows.Err(); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		entry.Stats.ItemStats = map[string]map[int32]uint32{}
		rows, err = txQueryMatchAggregatePlayerItemStatStmt.Query(query.GameMode, entry.ID)
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var key string
			var item int32
			var value uint32

			if err := rows.Scan(&key, &item, &value); err != nil {
				err = fmt.Errorf("%w: %w", ErrDatabase, err)
				c.Error(err)
				abortStatusError(c, http.StatusInternalServerError, err)
				return
			}

			if _, exists := entry.Stats.ItemStats[key]; !exists {
				entry.Stats.ItemStats[key] = map[int32]uint32{}
			}

			entry.Stats.ItemStats[key][item] = value
		}

		if err := rows.Err(); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}
		defer rows.Close()

		if query.GameMode == nil || *query.GameMode == "pvpa" {
			txQueryMatchAggregatePlayerTeamPointsStmt := tx.Stmt(queryMatchAggregatePlayerTeamPointsStmt)
			defer txQueryMatchAggregatePlayerTeamPointsStmt.Close()

			if err := txQueryMatchAggregatePlayerTeamPointsStmt.QueryRow(entry.ID).Scan(&entry.Stats.TeamPointsTotal); err != nil {
				err = fmt.Errorf("%w: %w", ErrDatabase, err)
				c.Error(err)
				abortStatusError(c, http.StatusInternalServerError, err)
				return
			}
		}

		all = append(all, entry)
	}

	if err := rows.Err(); err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	if err := tx.Commit(); err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusOK, all)
}
