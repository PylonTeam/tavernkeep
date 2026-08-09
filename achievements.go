package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type AchievementRef struct {
	Name     string `json:"name" binding:"required"`
	GameMode string `json:"gameMode" binding:"required"`
}

type AchievementDef struct {
	Target uint32 `json:"target" binding:"required"`
	Reward uint32 `json:"reward" binding:"required"`
}

type Achievement struct {
	AchievementDef
	Progress  uint32     `json:"progress" binding:"required"`
	ClaimedAt *time.Time `json:"claimedAt,omitempty"`
}

func getAchievementsHandler(c *gin.Context) {
	id, hasID := c.Get("id")
	if !hasID {
		abortStatusError(c, http.StatusUnauthorized, ErrNoSteamAuth)
		return
	}

	type requestQuery struct {
		GameMode string `form:"gameMode" binding:"required"`
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

	txQueryAchievementDefsStmt := tx.Stmt(queryAchievementDefsStmt)
	defer txQueryAchievementDefsStmt.Close()
	txQueryAchievementsStmt := tx.Stmt(queryAchievementsStmt)
	defer txQueryAchievementsStmt.Close()

	rows, err := txQueryAchievementDefsStmt.Query(query.GameMode)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	achievements := map[string]Achievement{}
	for rows.Next() {
		var name string
		a := Achievement{}

		if err := rows.Scan(&name, &a.Target, &a.Reward); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		achievements[name] = a
	}

	if err := rows.Err(); err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	rows, err = txQueryAchievementsStmt.Query(query.GameMode, id)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var progress uint32
		var claimed *uint32
		var claimed_at *int64

		if err := rows.Scan(&name, &progress, &claimed, &claimed_at); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		if a, ok := achievements[name]; ok {
			a.Progress = progress

			if claimed != nil {
				a.Reward = *claimed
			}

			if claimed_at != nil {
				a.ClaimedAt = new(time.Time)
				*a.ClaimedAt = time.Unix(*claimed_at, 0)
			}

			achievements[name] = a
		}
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

	c.JSON(http.StatusOK, achievements)
}

type achievementProgressContext struct {
	txQueryAchievementDefStmt     *sql.Stmt
	txQueryAchievementStmt        *sql.Stmt
	txExecAchievementProgressStmt *sql.Stmt
}

func progressAchievement(ctx *achievementProgressContext, id uint64, aRef AchievementRef, delta *uint32, external bool) (Achievement, error) {
	if delta != nil && *delta == 0 {
		return Achievement{}, ErrAchievementNoDeltaZero
	}

	a := Achievement{}
	var noExtMut bool

	if err := ctx.txQueryAchievementDefStmt.QueryRow(aRef.Name, aRef.GameMode).Scan(&a.Target, &a.Reward, &noExtMut); err == sql.ErrNoRows {
		return Achievement{}, ErrAchievementDoesNotExist
	} else if err != nil {
		return Achievement{}, fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	if noExtMut && external {
		return Achievement{}, ErrAchievementNoExtMut
	}

	var claimed *uint32
	var claimed_at *int64
	if err := ctx.txQueryAchievementStmt.QueryRow(aRef.Name, aRef.GameMode, id).Scan(&a.Progress, &claimed, &claimed_at); err != nil && err != sql.ErrNoRows {
		return Achievement{}, fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	if claimed != nil || claimed_at != nil {
		return Achievement{}, ErrAchievementAlreadyClaimed
	}

	initialProgress := a.Progress
	if delta != nil {
		a.Progress = min(a.Target, a.Progress+*delta)
	} else {
		a.Progress = a.Target
	}

	if a.Progress != initialProgress {
		if _, err := ctx.txExecAchievementProgressStmt.Exec(aRef.Name, aRef.GameMode, id, a.Progress, a.Progress); err != nil {
			return Achievement{}, fmt.Errorf("%w: %w", ErrDatabase, err)
		}
	}

	return a, nil
}

func progressAchievementHandler(c *gin.Context) {
	type requestBody struct {
		ID          uint64         `json:"id,string" binding:"required"`
		Achievement AchievementRef `json:"achievement" binding:"required"`
		Delta       *uint32        `json:"delta,omitempty"`
	}

	var b requestBody
	if err := c.ShouldBindJSON(&b); err != nil {
		abortStatusError(c, http.StatusBadRequest, fmt.Errorf("%w: %w", ErrMalformedBody, err))
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

	ctx := achievementProgressContext{}
	ctx.txQueryAchievementDefStmt = tx.Stmt(queryAchievementDefStmt)
	defer ctx.txQueryAchievementDefStmt.Close()
	ctx.txQueryAchievementStmt = tx.Stmt(queryAchievementStmt)
	defer ctx.txQueryAchievementStmt.Close()
	ctx.txExecAchievementProgressStmt = tx.Stmt(execAchievementProgressStmt)
	defer ctx.txExecAchievementProgressStmt.Close()

	a, err := progressAchievement(&ctx, b.ID, b.Achievement, b.Delta, true)
	if err != nil {
		var httpCode int

		if errors.Is(err, ErrAchievementDoesNotExist) {
			httpCode = http.StatusNotFound
		} else if errors.Is(err, ErrAchievementNoDeltaZero) || errors.Is(err, ErrAchievementNoExtMut) || errors.Is(err, ErrAchievementAlreadyClaimed) {
			httpCode = http.StatusBadRequest
		} else {
			httpCode = http.StatusInternalServerError
			c.Error(err)
		}

		abortStatusError(c, httpCode, err)
		return
	}

	if err := tx.Commit(); err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusOK, a)
}

func claimAchievementHandler(c *gin.Context) {
	id, hasID := c.Get("id")
	if !hasID {
		abortStatusError(c, http.StatusUnauthorized, ErrNoSteamAuth)
		return
	}

	type requestBody struct {
		AchievementRef
	}
	var b requestBody
	if err := c.ShouldBindJSON(&b); err != nil {
		abortStatusError(c, http.StatusBadRequest, fmt.Errorf("%w: %w", ErrMalformedBody, err))
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

	txQueryAchievementDefStmt := tx.Stmt(queryAchievementDefStmt)
	defer txQueryAchievementDefStmt.Close()
	txExecAchievementClaimTx := tx.Stmt(execAchievementClaimStmt)
	defer txExecAchievementClaimTx.Close()
	txExecIncreaseGemsStmt := tx.Stmt(execIncreaseGemsStmt)
	defer txExecIncreaseGemsStmt.Close()

	a := Achievement{}
	var noExtMut bool

	if err := txQueryAchievementDefStmt.QueryRow(b.Name, b.GameMode).Scan(&a.Target, &a.Reward, &noExtMut); err == sql.ErrNoRows {
		abortStatusError(c, http.StatusNotFound, ErrAchievementDoesNotExist)
		return
	} else if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	var claimed_at int64
	if err = txExecAchievementClaimTx.QueryRow(a.Reward, b.Name, b.GameMode, id, a.Target).Scan(&claimed_at, &a.Progress); err == sql.ErrNoRows {
		c.Status(http.StatusBadRequest)
		return
	} else if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}
	a.ClaimedAt = new(time.Time)
	*a.ClaimedAt = time.Unix(claimed_at, 0)

	if a.Reward > 0 {
		if _, err = txExecIncreaseGemsStmt.Exec(id, a.Reward); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusOK, a)
}
