package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"

	_ "github.com/mattn/go-sqlite3"
)

var ErrNoSteamAuth = errors.New("no steam auth")
var ErrNoOfficialAuth = errors.New("no official auth")
var ErrDatabase = errors.New("database issue")
var ErrReplaySave = errors.New("issue saving replay")
var ErrMalformedBody = errors.New("malformed body")
var ErrMalformedQuery = errors.New("malformed query")
var ErrMalformedMatch = errors.New("malformed match")
var ErrAchievementDoesNotExist = errors.New("achievement does not exist")
var ErrAchievementNoDeltaZero = errors.New("delta must not be zero")
var ErrAchievementNoExtMut = errors.New("cannot externally mutate this achievement")
var ErrAchievementAlreadyClaimed = errors.New("achievement already claimed")
var ErrProductDoesNotExist = errors.New("product does not exist")
var ErrProductAlreadyOwned = errors.New("product already owned")
var ErrProductCannotAfford = errors.New("cannot afford product")
var ErrProductRequiresAccolade = errors.New("prerequisite accolade not in possession")
var ErrProductRequiresAchievement = errors.New("prerequisite achievement has not been claimed")
var ErrUnsupportedGameMode = errors.New("unsupported game mode")
var ErrVersion = errors.New("check version")

var (
	steamApiKey    string
	db             *sql.DB
	s3Client       *s3.Client
	s3ReplayBucket string
	s3Endpoint     string
)

func abortStatusError(c *gin.Context, httpCode int, err error) {
	c.AbortWithStatusJSON(httpCode, ErrorBody{ErrorText: err.Error()})
}

func readSteamApiKey() (string, error) {
	data, err := os.ReadFile("./secret/steam_api_key.txt")
	if err != nil {
		return "", fmt.Errorf("failed to read Steam API key: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}

type ErrorBody struct {
	ErrorText string `json:"errorText" binding:"required"`
}

func getAccoladesHandler(c *gin.Context) {
	if version := c.GetUint32("version"); version != 1 {
		abortStatusError(c, http.StatusBadRequest, ErrVersion)
		return
	}

	rows, err := queryAccoladesStmt.Query()
	if err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	accolades := map[string]string{}
	for rows.Next() {
		var name, owner string

		if err := rows.Scan(&name, &owner); err != nil {
			c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		accolades[name] = owner
	}

	if err := rows.Err(); err != nil {
		c.Error(fmt.Errorf("%w: %w", ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, accolades)
}

func errorHandler(c *gin.Context) {
	c.Next()

	if len(c.Errors) > 0 {
		log.Println("errors encountered during request handling:", strings.Join(c.Errors.Errors(), ","))
	}
}

func requireContentTypeJson(c *gin.Context) {
	if c.ContentType() != "application/json" {
		c.AbortWithStatus(http.StatusUnsupportedMediaType)
		return
	}

	c.Next()
}

func requireContentTypeMultipartForm(c *gin.Context) {
	if c.ContentType() != "multipart/form-data" {
		c.AbortWithStatus(http.StatusUnsupportedMediaType)
		return
	}

	c.Next()
}

func initS3() error {
	ctx := context.Background()

	sdkConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}

	s3Client = s3.NewFromConfig(sdkConfig)
	a, _ := url.Parse(*sdkConfig.BaseEndpoint)
	s3ReplayBucket = os.Getenv("TAVERNKEEP_REPLAY_BUCKET")
	s3Endpoint = fmt.Sprintf("%s.%s", s3ReplayBucket, a.Host)

	return nil
}

func requireVersionHandler(c *gin.Context) {
	version, hasVersion := c.Params.Get("v")
	if !hasVersion {
		abortStatusError(c, http.StatusNotFound, fmt.Errorf("%w: missing", ErrVersion))
		return
	}

	var versionNum uint32
	if _, err := fmt.Sscanf(version, "v%d", &versionNum); err != nil {
		abortStatusError(c, http.StatusNotFound, fmt.Errorf("%w: unscanned", ErrVersion))
		return
	}

	if versionNum == 0 {
		abortStatusError(c, http.StatusNotFound, fmt.Errorf("%w: invalid", ErrVersion))
		return
	}

	c.Set("version", versionNum)

	c.Next()
}

func main() {
	if err := initS3(); err != nil {
		log.Fatalln(fmt.Errorf("failed to initialize s3: %w", err))
	}

	var err error
	if steamApiKey, err = readSteamApiKey(); err != nil {
		log.Fatalln(fmt.Errorf("failed to read steam api key: %w", err))
	}

	db, err = sql.Open("sqlite3", "./data/tavernkeep.db?_txlock=immediate")
	if err != nil {
		log.Fatalln(fmt.Errorf("failed to open database: %w", err))
	}

	defer db.Close()

	db.SetMaxOpenConns(1)

	if _, err = db.Exec(dbSchema); err != nil {
		log.Fatalln(fmt.Errorf("failed to exec database schema: %w", err))
	}

	if err = prepareStatements(); err != nil {
		log.Fatalln(fmt.Errorf("failed to prepare statements: %w", err))
	}
	defer closeStatements()

	r := gin.Default()

	r.SetTrustedProxies([]string{"127.0.0.1"})

	r.GET("/shop/:v", requireVersionHandler, listProductsHandler)
	r.POST("/shop/purchase/:v", requireVersionHandler, requireContentTypeJson, steamAuthTicketHandler, purchaseProductHandler)

	r.GET("/profile/:v", requireVersionHandler, getProfileHandler)
	r.GET("/profile/inventory/:v", requireVersionHandler, steamAuthTicketHandler, getInventoryHandler)
	r.POST("/profile/equip/:v", requireVersionHandler, requireContentTypeJson, steamAuthTicketHandler, equipItemHandler)

	r.GET("/match/:v", requireVersionHandler, getMatchHandler)
	r.POST("/match/:v", requireVersionHandler, officialAuthHandler, recordMatchHandler)

	r.GET("/accolades/:v", requireVersionHandler, getAccoladesHandler)

	r.GET("/achievements/:v", requireVersionHandler, steamAuthTicketHandler, getAchievementsHandler)
	r.POST("/achievements/:v", requireVersionHandler, requireContentTypeJson, officialAuthHandler, progressAchievementHandler)
	r.POST("/achievements/claim/:v", requireVersionHandler, requireContentTypeJson, steamAuthTicketHandler, claimAchievementHandler)

	if err := r.Run(":8080"); err != nil {
		log.Fatalln(fmt.Errorf("unable to bind Gin engine to port 8080: %w", err))
	}
}
