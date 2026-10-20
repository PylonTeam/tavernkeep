package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func officialAuthTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "official-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	oldDB, oldQuery := db, queryOfficialStmt
	db = conn
	var query *sql.Stmt
	t.Cleanup(func() {
		if query != nil {
			query.Close()
		}
		conn.Close()
		db, queryOfficialStmt = oldDB, oldQuery
	})
	if _, err := conn.Exec(`CREATE TABLE official (name TEXT NOT NULL, key TEXT NOT NULL);
		INSERT INTO official (name, key) VALUES ('Test official server', 'fabricated-test-key')`); err != nil {
		t.Fatal(err)
	}
	query, err = conn.Prepare("SELECT name FROM official WHERE key = ?")
	if err != nil {
		t.Fatal(err)
	}
	queryOfficialStmt = query
	return conn
}

func TestOfficialAuthReleasesConnectionBeforeHandler(t *testing.T) {
	conn := officialAuthTestDatabase(t)
	router := gin.New()
	called := false
	var handlerErr error
	var identity OfficialIdentity
	router.POST("/match/v1", officialAuthHandler, func(c *gin.Context) {
		called = true
		identity = c.MustGet("official").(OfficialIdentity)
		ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
		defer cancel()
		var tx *sql.Tx
		tx, handlerErr = conn.BeginTx(ctx, nil)
		if handlerErr != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/match/v1", nil)
	request.Header.Set("Authorization", "Bearer fabricated-test-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if !called {
		t.Fatal("authenticated request did not reach the downstream handler")
	}
	if handlerErr != nil {
		t.Fatalf("downstream transaction could not acquire the sole database connection: %v", handlerErr)
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if identity.Subject != "Test official server" || identity.Fingerprint != "fabricated-test-key" {
		t.Fatal("official identity was not preserved for the downstream handler")
	}
}

func TestOfficialAuthRejectsInvalidCredentials(t *testing.T) {
	officialAuthTestDatabase(t)
	for _, header := range []string{"", "Basic fabricated-test-key", "Bearer unknown-test-key"} {
		called := false
		router := gin.New()
		router.POST("/match/v1", officialAuthHandler, func(c *gin.Context) {
			called = true
			c.Status(http.StatusNoContent)
		})
		request := httptest.NewRequest(http.MethodPost, "/match/v1", nil)
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
		}
		if called {
			t.Fatal("unauthorized request reached the downstream handler")
		}
	}
}
