package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Prototype and Name form a unique reference to an ambiguous/arbitrary definition.
// The backend need not care about the entire definition.
type ProductItemRef struct {
	Prototype string `json:"prototype" binding:"required"`
	Name      string `json:"name" binding:"required"`
}

type Product struct {
	ProductItemRef

	Price       uint32          `json:"price" binding:"required"`
	Accolade    *string         `json:"accolade,omitempty"`
	Achievement *AchievementRef `json:"achievement,omitempty"`
}

// Item represents an owned item.
type Item struct {
	ProductItemRef

	Issued        time.Time `json:"issued" binding:"required"`
	PurchasePrice *uint32   `json:"purchasePrice,omitempty"`
}

func listProductsHandler(c *gin.Context) {
	if version := c.GetUint32("version"); version != 1 {
		abortStatusError(c, http.StatusBadRequest, ErrVersion)
		return
	}

	type requestQuery struct {
		Prototype *string `form:"prototype"`
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

	txQueryProductsStmt := tx.Stmt(queryProductsStmt)
	defer txQueryProductsStmt.Close()

	rows, err := txQueryProductsStmt.Query(query.Prototype)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	products := []Product{}
	for rows.Next() {
		var product Product
		var achieveGameMode, achieveName *string

		if err := rows.Scan(&product.Prototype, &product.Name, &product.Price, &product.Accolade, &achieveGameMode, &achieveName); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		if achieveGameMode != nil && achieveName != nil {
			product.Achievement = &AchievementRef{
				Name:     *achieveName,
				GameMode: *achieveGameMode,
			}
		}

		products = append(products, product)
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

	c.JSON(http.StatusOK, products)
}

func purchaseProductHandler(c *gin.Context) {
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
		ProductItemRef
	}
	var body requestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		abortStatusError(c, http.StatusBadRequest, fmt.Errorf("%w: %w", ErrMalformedBody, err))
		return
	}

	item := Item{ProductItemRef: body.ProductItemRef}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	// FIXME: MORE CONTEXT!!
	txQueryInventoryItemCountStmt := tx.Stmt(queryInventoryItemCountStmt)
	defer txQueryInventoryItemCountStmt.Close()
	txQueryProductStmt := tx.Stmt(queryProductStmt)
	defer txQueryProductStmt.Close()
	txExecReduceGemsStmt := tx.Stmt(execReduceGemsStmt)
	defer txExecReduceGemsStmt.Close()
	txExecInsertItemStmt := tx.Stmt(execInsertItemStmt)
	defer txExecInsertItemStmt.Close()

	var count int
	err = txQueryInventoryItemCountStmt.QueryRow(item.Prototype, item.Name, id).Scan(&count)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	if count > 0 {
		abortStatusError(c, http.StatusConflict, ErrProductAlreadyOwned)
		return
	}

	var accolade, achieve_game_mode, achieve_name *string
	err = txQueryProductStmt.QueryRow(item.Prototype, item.Name).Scan(&item.PurchasePrice, &accolade, &achieve_game_mode, &achieve_name)

	if err == sql.ErrNoRows {
		abortStatusError(c, http.StatusNotFound, ErrProductDoesNotExist)
		return
	} else if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	if accolade != nil {
		txQueryAccoladeStmt := tx.Stmt(queryAccoladeStmt)
		defer txQueryAccoladeStmt.Close()

		var count int
		if err = txQueryAccoladeStmt.QueryRow(*accolade, id).Scan(&count); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		if count <= 0 {
			abortStatusError(c, http.StatusForbidden, ErrProductRequiresAccolade)
			return
		}
	}

	if achieve_name != nil && achieve_game_mode != nil {
		txQueryAchievementClaimedStmt := tx.Stmt(queryAchievementClaimedStmt)
		defer txQueryAchievementClaimedStmt.Close()

		var count int
		if err = txQueryAchievementClaimedStmt.QueryRow(achieve_name, achieve_game_mode, id).Scan(&count); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		if count <= 0 {
			abortStatusError(c, http.StatusForbidden, ErrProductRequiresAchievement)
			return
		}
	}

	if *item.PurchasePrice > 0 {
		result, err := txExecReduceGemsStmt.Exec(item.PurchasePrice, id)
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

		if affected == 0 {
			abortStatusError(c, http.StatusForbidden, ErrProductCannotAfford)
			return
		}
	}

	var issued int64
	if err := txExecInsertItemStmt.QueryRow(item.Prototype, item.Name, id, item.PurchasePrice, accolade).Scan(&issued); err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	item.Issued = time.Unix(issued, 0)

	if err := tx.Commit(); err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusCreated, item)
}
