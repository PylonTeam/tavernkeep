package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type Appearance struct {
	Gender          *uint32 `json:"gender" binding:"required"`
	ClothingStyle   *uint32 `json:"clothingStyle" binding:"required"`
	HairStyle       *uint32 `json:"hairStyle" binding:"required"`
	HairColor       *uint32 `json:"hairColor" binding:"required"`
	SkinColor       *uint32 `json:"skinColor" binding:"required"`
	EyeColor        *uint32 `json:"eyeColor" binding:"required"`
	ShirtColor      *uint32 `json:"shirtColor" binding:"required"`
	UnderShirtColor *uint32 `json:"underShirtColor" binding:"required"`
	PantsColor      *uint32 `json:"pantsColor" binding:"required"`
	ShoeColor       *uint32 `json:"shoeColor" binding:"required"`
}

func getProfileHandler(c *gin.Context) {
	if version := c.GetUint32("version"); version != 1 {
		abortStatusError(c, http.StatusBadRequest, ErrVersion)
		return
	}

	type requestQuery struct {
		ID uint64 `form:"id" binding:"required"`
	}
	var query requestQuery

	if err := c.ShouldBindQuery(&query); err != nil {
		abortStatusError(c, http.StatusBadRequest, fmt.Errorf("%w: %w", ErrMalformedQuery, err))
		return
	}

	id := query.ID

	type responseBody struct {
		ID        uint64            `json:"id,string"`
		Gems      int64             `json:"gems"`
		Character Appearance        `json:"character"`
		Equipment map[string]string `json:"equipment"`
	}
	profile := responseBody{
		ID: id,
	}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer tx.Rollback()

	// FIXME: MORE CONTEXT!!
	txQueryProfileStmt := tx.Stmt(queryProfileStmt)
	defer txQueryProfileStmt.Close()
	txQueryEquipmentStmt := tx.Stmt(queryEquipmentStmt)
	defer txQueryEquipmentStmt.Close()

	if err := txQueryProfileStmt.QueryRow(id).Scan(&profile.Gems, &profile.Character.Gender, &profile.Character.ClothingStyle, &profile.Character.HairStyle, &profile.Character.HairColor, &profile.Character.SkinColor, &profile.Character.EyeColor, &profile.Character.ShirtColor, &profile.Character.UnderShirtColor, &profile.Character.PantsColor, &profile.Character.ShoeColor); err == sql.ErrNoRows {
		abortStatusError(c, http.StatusNotFound, ErrNoProfile)
		return
	} else if err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	rows, err := txQueryEquipmentStmt.Query(id)
	if err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	profile.Equipment = map[string]string{}
	for rows.Next() {
		var prototype, name string
		if err := rows.Scan(&prototype, &name); err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		profile.Equipment[prototype] = name
	}

	if err := rows.Err(); err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, profile)
}

func equipItemHandler(c *gin.Context) {
	if version := c.GetUint32("version"); version != 1 {
		abortStatusError(c, http.StatusBadRequest, ErrVersion)
		return
	}

	id, hasID := c.Get("id")
	if !hasID {
		abortStatusError(c, http.StatusUnauthorized, ErrNoSteamAuth)
		return
	}

	type requestBody struct {
		// This is a ProductItemRef with a nullable name.
		Prototype string  `json:"prototype" binding:"required"`
		Name      *string `json:"name"`
	}
	var body requestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		abortStatusError(c, http.StatusBadRequest, fmt.Errorf("%w: %w", ErrMalformedBody, err))
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

	txExecEquipStmt := tx.Stmt(execEquipStmt)
	defer txExecEquipStmt.Close()

	_, err = txExecEquipStmt.Exec(body.Name, body.Prototype, id)
	if err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	if err := tx.Commit(); err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.Status(http.StatusNoContent)
}

func getInventoryHandler(c *gin.Context) {
	if version := c.GetUint32("version"); version != 1 {
		abortStatusError(c, http.StatusBadRequest, ErrVersion)
		return
	}

	id, hasID := c.Get("id")
	if !hasID {
		abortStatusError(c, http.StatusUnauthorized, ErrNoSteamAuth)
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

	txQueryInventoryStmt := tx.Stmt(queryInventoryStmt)
	defer txQueryInventoryStmt.Close()

	rows, err := txQueryInventoryStmt.Query(id)
	if err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	items := []Item{}
	for rows.Next() {
		var issued int64
		var item Item

		if err := rows.Scan(&item.Prototype, &item.Name, &issued, &item.PurchasePrice); err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		item.Issued = time.Unix(issued, 0)
		items = append(items, item)
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

	c.JSON(http.StatusOK, items)
}

func putCharacterHandler(c *gin.Context) {
	if version := c.GetUint32("version"); version != 1 {
		abortStatusError(c, http.StatusBadRequest, ErrVersion)
		return
	}

	id, hasID := c.Get("id")
	if !hasID {
		abortStatusError(c, http.StatusUnauthorized, ErrNoSteamAuth)
		return
	}

	var body Appearance
	if err := c.ShouldBindJSON(&body); err != nil {
		abortStatusError(c, http.StatusBadRequest, fmt.Errorf("%w: %w", ErrMalformedBody, err))
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

	txExecProfileUpsertChar := tx.Stmt(execProfileUpsertChar)
	defer txExecProfileUpsertChar.Close()

	_, err = txExecProfileUpsertChar.Exec(id, body.Gender, body.ClothingStyle, body.HairStyle, body.HairColor, body.SkinColor, body.EyeColor, body.ShirtColor, body.UnderShirtColor, body.PantsColor, body.ShoeColor)
	if err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	if execMoveLegacyGemsStmt != nil && deleteLegacyGemsStmt != nil {
		txExecMoveLegacyGemsStmt := tx.Stmt(execMoveLegacyGemsStmt)
		defer txExecMoveLegacyGemsStmt.Close()

		result, err := txExecMoveLegacyGemsStmt.Exec(id)
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		affected, err := result.RowsAffected()
		if err != nil {
			err = fmt.Errorf("%w: cannot get rows affected: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		if affected == 1 {
			txDeleteLegacyGemsStmt := tx.Stmt(deleteLegacyGemsStmt)
			defer txDeleteLegacyGemsStmt.Close()

			_, err = txDeleteLegacyGemsStmt.Exec(id)
			if err != nil {
				err = fmt.Errorf("%w: %w", ErrDatabase, err)
				c.Error(err)
				abortStatusError(c, http.StatusInternalServerError, err)
				return
			}
		}
	}

	if err := tx.Commit(); err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.Status(http.StatusNoContent)
}
