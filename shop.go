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

	Price       uint32  `json:"price" binding:"required"`
	Accolade    *string `json:"accolade,omitempty"`
	Achievement *string `json:"achievement,omitempty"`
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

	rows, err := queryProductsStmt.Query()
	if err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	products := []Product{}
	var product Product
	for rows.Next() {
		if err := rows.Scan(&product.Prototype, &product.Name, &product.Price, &product.Accolade, &product.Achievement); err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}
		products = append(products, product)
	}

	if err := rows.Err(); err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
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
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
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
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	if count > 0 {
		abortStatusError(c, http.StatusConflict, ErrProductAlreadyOwned)
		return
	}

	var accolade, achievement *string
	err = txQueryProductStmt.QueryRow(item.Prototype, item.Name).Scan(&item.PurchasePrice, &accolade, &achievement)

	if err == sql.ErrNoRows {
		abortStatusError(c, http.StatusNotFound, ErrProductDoesNotExist)
		return
	} else if err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	if accolade != nil {
		txQueryAccoladeStmt := tx.Stmt(queryAccoladeStmt)
		defer txQueryAccoladeStmt.Close()

		var owner string
		if err = txQueryAccoladeStmt.QueryRow(*accolade).Scan(&owner); err == sql.ErrNoRows || id != owner {
			abortStatusError(c, http.StatusForbidden, ErrProductRequiresAccolade)
			return
		} else if err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}
	}

	if *item.PurchasePrice > 0 {
		result, err := txExecReduceGemsStmt.Exec(item.PurchasePrice, item.PurchasePrice, id)
		if err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		affected, err := result.RowsAffected()
		if err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		if affected == 0 {
			abortStatusError(c, http.StatusForbidden, ErrProductCannotAfford)
			return
		}
	}

	var issued int64
	if err := txExecInsertItemStmt.QueryRow(item.Prototype, item.Name, id, item.PurchasePrice, accolade).Scan(&issued); err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	item.Issued = time.Unix(issued, 0)

	if err := tx.Commit(); err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, item)
}
