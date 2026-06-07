package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/ptr"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"

	"github.com/golang-jwt/jwt/v5"

	_ "github.com/mattn/go-sqlite3"
)

// Prototype and Name form a unique reference to an ambiguous/arbitrary definition.
// The backend need not care about the entire definition.
type DefinitionReference struct {
	Prototype string `json:"prototype" binding:"required"`
	Name      string `json:"name" binding:"required"`
}

type Product struct {
	DefinitionReference

	Price       uint32  `json:"price" binding:"required"`
	Accolade    *string `json:"accolade,omitempty"`
	Achievement *string `json:"achievement,omitempty"`
}

type Profile struct {
	ID        uint64            `json:"id,string" binding:"required"`
	Gems      int64             `json:"gems" binding:"required"`
	Equipment map[string]string `json:"equipment" binding:"required"`
}

// Item represents an owned item.
type Item struct {
	DefinitionReference

	Issued        time.Time `json:"issued" binding:"required"`
	PurchasePrice *uint32   `json:"purchasePrice,omitempty"`
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

// FIXME: why when unmarshalling Match does this one not respect required bindings?
type Player struct {
	Name string `json:"name" binding:"required"`
	Team uint32 `json:"team" binding:"required"`

	Reward uint32 `json:"reward" binding:"required"`

	Kills  int32 `json:"kills" binding:"required"`
	Deaths int32 `json:"deaths" binding:"required"`
}

type Team struct {
	Points int `json:"points" binding:"required"`
	// TODO: implement first blood bosses here
	Bosses []int `json:"bosses" binding:"required"`
}

type Match struct {
	Start    time.Time `json:"start" binding:"required"`
	End      time.Time `json:"end" binding:"required"`
	GameMode string    `json:"gameMode" binding:"required"`

	Players map[uint64]Player `json:"players" binding:"required"`
	Metrics map[string]string `json:"metrics,omitempty" binding:"required"`

	// Assume PvP Adventure
	Teams []*Team `json:"teams" binding:"required"`
}

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
var ErrUnsupportedGameMode = errors.New("unsupported game mode")

func (m *Match) UnmarshalJSON(b []byte) error {
	type aux Match
	if err := json.Unmarshal(b, (*aux)(m)); err != nil {
		return err
	}

	if len(m.Players) == 0 {
		return fmt.Errorf("%w: no players", ErrMalformedMatch)
	}

	if m.GameMode == "pvpa" {
		if len(m.Teams) == 0 {
			return fmt.Errorf("%w: no teams", ErrMalformedMatch)
		}

		if m.Teams[0] != nil {
			return fmt.Errorf("%w: team None must be null", ErrMalformedMatch)
		}
	} else {
		return fmt.Errorf("%w: unknown game mode", ErrMalformedMatch)
	}

	return nil
}

type OfficialMatch struct {
	ID               int64     `json:"id" binding:"required"`
	Recorded         time.Time `json:"recorded" binding:"required"`
	OfficialIdentity string    `json:"officialIdentity" binding:"required"`
	ReplayUrl        *string   `json:"replayUrl,omitempty"`

	Match
}

type AuthenticateUserTicket struct {
	Response struct {
		Params struct {
			Result          string `json:"result"`
			SteamID         uint64 `json:"steamid,string"`
			OwnerSteamID    uint64 `json:"ownersteamid,string"`
			VACBanned       bool   `json:"vacbanned"`
			PublisherBanned bool   `json:"publisherbanned"`
		} `json:"params"`
		Error *struct {
			ErrorCode int    `json:"errorcode"`
			ErrorDesc string `json:"errordesc"`
		} `json:"error"`
	} `json:"response"`
}

var (
	steamApiKey          string
	db                   *sql.DB
	s3Client             *s3.Client
	replayBucket         string
	replayOriginEndpoint string
)

func readSteamApiKey() (string, error) {
	data, err := os.ReadFile("./secret/steam_api_key.txt")
	if err != nil {
		return "", fmt.Errorf("failed to read Steam API key: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}

func jwtCachedAuthHandler(c *gin.Context) {
	session := sessions.Default(c)
	maybeToken := session.Get("jwt")
	if maybeToken != nil {
		token, err := jwt.ParseWithClaims(maybeToken.(string), &jwt.RegisteredClaims{}, func(t *jwt.Token) (any, error) {
			return []byte("CWfpquCHUohhn0qaUseA11MICY3lCqII"), nil
		})

		if err != nil {
			log.Println(err)
			return
		}

		sub, err := token.Claims.GetSubject()
		if err != nil {
			log.Println(err)
			return
		}

		c.Set("id", sub)
	}

	c.Next()
}

type OfficialIdentity struct {
	Subject     string
	Fingerprint string
}

func officialAuthHandler(c *gin.Context) {
	verified, hasVerified := c.Request.Header["Official-Verified"]
	if !hasVerified {
		abortStatusError(c, http.StatusUnauthorized, ErrNoOfficialAuth)
		return
	}

	if verified[0] != "SUCCESS" {
		abortStatusError(c, http.StatusUnauthorized, fmt.Errorf("%w: unable to verify: %s", ErrNoOfficialAuth, verified[0]))
		return
	}

	subject, hasSubject := c.Request.Header["Official-Identity-Subject"]
	fingerprint, hasFingerprint := c.Request.Header["Official-Identity-Fingerprint"]
	if !hasSubject || !hasFingerprint {
		c.Error(fmt.Errorf("%w: missing identity headers from proxy?", ErrNoOfficialAuth))
		abortStatusError(c, http.StatusUnauthorized, ErrNoOfficialAuth)
		return
	}

	c.Set("official", OfficialIdentity{Subject: subject[0], Fingerprint: fingerprint[0]})

	c.Next()
}

type ErrorBody struct {
	ErrorText string `json:"errorText" binding:"required"`
}

func abortStatusError(c *gin.Context, httpCode int, err error) {
	c.AbortWithStatusJSON(httpCode, ErrorBody{ErrorText: err.Error()})
}

func steamAuthTicketHandler(c *gin.Context) {
	if _, hasID := c.Get("id"); !hasID {
		if ticket, hasTicket := c.Request.Header["Ticket"]; hasTicket {
			authUserTicketUrl, err := url.Parse("https://api.steampowered.com/ISteamUserAuth/AuthenticateUserTicket/v1")
			if err != nil {
				panic("couldn't parse authenticate user ticket URL")
			}

			q := url.Values{}
			// FIXME: maybe using x-webapi-key header is nicer
			q.Add("key", steamApiKey)
			// the ticket is only valid for this appid (tModLoader, NOT Terraria)
			q.Add("appid", "1281930")
			q.Add("ticket", ticket[0])
			// (optionally if we provide it here like we do) the ticket identity must match this. that was set by our code creating the ticket on the client.
			q.Add("identity", "api.tpvpa.terraria.sh")
			authUserTicketUrl.RawQuery = q.Encode()

			res, err := http.Get(authUserTicketUrl.String())
			if err != nil {
				abortErr := fmt.Errorf("%w: unable to make authentication request to Steam", ErrNoSteamAuth)
				c.Error(errors.Join(abortErr, err))
				abortStatusError(c, http.StatusServiceUnavailable, abortErr)
				return
			}
			defer res.Body.Close()

			if res.StatusCode != http.StatusOK {
				abortStatusError(c, http.StatusUnauthorized, fmt.Errorf("%w: unsuccessful authentication response from Steam", ErrNoSteamAuth))
				return
			}

			authUserTicketRes := AuthenticateUserTicket{}
			json.NewDecoder(res.Body).Decode(&authUserTicketRes)

			if authUserTicketRes.Response.Error != nil {
				abortStatusError(c, http.StatusUnauthorized, fmt.Errorf("%w: authentication failed: %d (%s)", ErrNoSteamAuth, authUserTicketRes.Response.Error.ErrorCode, authUserTicketRes.Response.Error.ErrorDesc))
				return
			}

			if result := authUserTicketRes.Response.Params.Result; result != "OK" {
				abortStatusError(c, http.StatusUnauthorized, fmt.Errorf("%w: authentication failed: %s", ErrNoSteamAuth, result))
				return
			}

			// key := []byte("CWfpquCHUohhn0qaUseA11MICY3lCqII")
			// t := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
			// 	Subject:   authUserTicketRes.Response.Params.SteamID,
			// 	ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute * 1)),
			// })
			// s, err := t.SignedString(key)

			// session := sessions.Default(c)
			// session.Set("jwt", s)
			// session.Save()

			c.Set("id", authUserTicketRes.Response.Params.SteamID)
		}
	}

	c.Next()
}

func officialAuthMiddleware() gin.HandlerFunc {
	return officialAuthHandler
}

func steamAuthTicketMiddleware() gin.HandlerFunc {
	return steamAuthTicketHandler
}

func jwtCachedAuthMiddleware() gin.HandlerFunc {
	return jwtCachedAuthHandler
}

type GetProfileQuery struct {
	ID uint64 `form:"id" binding:"required"`
}

func getProfileHandler(c *gin.Context) {
	var query GetProfileQuery

	if err := c.ShouldBindQuery(&query); err != nil {
		abortStatusError(c, http.StatusBadRequest, errors.Join(ErrMalformedQuery, err))
		return
	}

	id := query.ID

	profile := Profile{
		ID: id,
	}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer tx.Rollback()

	// FIXME: MORE CONTEXT!!
	txQueryGemsStmt := tx.Stmt(queryGemsStmt)
	defer txQueryGemsStmt.Close()
	txQueryEquipmentStmt := tx.Stmt(queryEquipmentStmt)
	defer txQueryEquipmentStmt.Close()

	if err := txQueryGemsStmt.QueryRow(id).Scan(&profile.Gems); err != nil && err != sql.ErrNoRows {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	rows, err := txQueryEquipmentStmt.Query(id)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	profile.Equipment = map[string]string{}
	for rows.Next() {
		var prototype, name string
		if err := rows.Scan(&prototype, &name); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		profile.Equipment[prototype] = name
	}

	if err := rows.Err(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, profile)
}

func listProductsHandler(c *gin.Context) {
	rows, err := queryProductsStmt.Query()
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	products := []Product{}
	var product Product
	for rows.Next() {
		if err := rows.Scan(&product.Prototype, &product.Name, &product.Price, &product.Accolade, &product.Achievement); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}
		products = append(products, product)
	}

	if err := rows.Err(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, products)
}

type PostPurchaseBody struct {
	DefinitionReference
}

func purchaseHandler(c *gin.Context) {
	id, hasID := c.Get("id")
	if !hasID {
		abortStatusError(c, http.StatusUnauthorized, ErrNoSteamAuth)
		return
	}

	var body PostPurchaseBody
	if err := c.ShouldBindJSON(&body); err != nil {
		abortStatusError(c, http.StatusBadRequest, errors.Join(ErrMalformedBody, err))
		return
	}

	item := Item{DefinitionReference: body.DefinitionReference}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
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
		c.Error(errors.Join(ErrDatabase, err))
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
		c.Error(errors.Join(ErrDatabase, err))
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
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}
	}

	if *item.PurchasePrice > 0 {
		result, err := txExecReduceGemsStmt.Exec(item.PurchasePrice, item.PurchasePrice, id)
		if err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		affected, err := result.RowsAffected()
		if err != nil {
			c.Error(errors.Join(ErrDatabase, err))
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
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	item.Issued = time.Unix(issued, 0)

	if err := tx.Commit(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, item)
}

type PostProfileEquip struct {
	Prototype string  `json:"prototype" binding:"required"`
	Name      *string `json:"name"`
}

func equipItemHandler(c *gin.Context) {
	id, hasID := c.Get("id")
	if !hasID {
		abortStatusError(c, http.StatusUnauthorized, ErrNoSteamAuth)
		return
	}

	var postProfileEquip PostProfileEquip
	if err := c.ShouldBindJSON(&postProfileEquip); err != nil {
		abortStatusError(c, http.StatusBadRequest, errors.Join(ErrMalformedBody, err))
		return
	}

	_, err := execEquipStmt.Exec(postProfileEquip.Name, postProfileEquip.Prototype, id)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.Status(http.StatusNoContent)
}

func getInventoryHandler(c *gin.Context) {
	id, hasID := c.Get("id")
	if !hasID {
		abortStatusError(c, http.StatusUnauthorized, ErrNoSteamAuth)
		return
	}

	rows, err := queryInventoryStmt.Query(id)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	items := []Item{}
	for rows.Next() {
		var issued int64
		var item Item

		if err := rows.Scan(&item.Prototype, &item.Name, &issued, &item.PurchasePrice); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		item.Issued = time.Unix(issued, 0)
		items = append(items, item)
	}

	if err := rows.Err(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, items)
}

type GetMatchQuery struct {
	ID uint64 `form:"id" binding:"required"`
}

func getMatchHandler(c *gin.Context) {
	var query GetMatchQuery

	if err := c.ShouldBindQuery(&query); err != nil {
		abortStatusError(c, http.StatusBadRequest, errors.Join(ErrMalformedQuery, err))
		return
	}

	id := query.ID

	var matches = []OfficialMatch{}
	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer tx.Rollback()

	// FIXME: MORE CONTEXT!!
	txQueryMatchesStmt := tx.Stmt(queryMatchesStmt)
	defer txQueryMatchesStmt.Close()

	// query all match ids that this player has been involved in.
	rows, err := txQueryMatchesStmt.Query(id)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	txQueryMatchStmt := tx.Stmt(queryMatchStmt)
	defer txQueryMatchStmt.Close()
	txQueryMatchPlayerStmt := tx.Stmt(queryMatchPlayerStmt)
	defer txQueryMatchPlayerStmt.Close()

	for rows.Next() {
		var match OfficialMatch
		if err := rows.Scan(&match.ID); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		var start, end, recorded int64
		if err := txQueryMatchStmt.QueryRow(match.ID).Scan(&start, &end, &match.GameMode, &recorded, &match.ReplayUrl, &match.OfficialIdentity); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		match.Start = time.Unix(start, 0)
		match.End = time.Unix(end, 0)
		match.Recorded = time.Unix(recorded, 0)

		rows, err := txQueryMatchPlayerStmt.Query(match.ID)
		if err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}
		defer rows.Close()

		match.Players = map[uint64]Player{}
		for rows.Next() {
			var id uint64
			var player Player

			if err := rows.Scan(&id, &player.Name, &player.Team, &player.Reward, &player.Kills, &player.Deaths); err != nil {
				c.Error(errors.Join(ErrDatabase, err))
				abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
				return
			}

			match.Players[id] = player
		}

		if err := rows.Err(); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		// Assume PvP Adventure
		txQueryMatchAdventureTeam := tx.Stmt(queryMatchAdventureTeam)
		defer txQueryMatchAdventureTeam.Close()
		txQueryMatchAdventureTeamBoss := tx.Stmt(queryMatchAdventureTeamBoss)
		defer txQueryMatchAdventureTeamBoss.Close()

		rows, err = txQueryMatchAdventureTeam.Query(match.ID)
		if err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var teamNumber, points int

			if err := rows.Scan(&teamNumber, &points); err != nil {
				c.Error(errors.Join(ErrDatabase, err))
				abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
				return
			}

			// first iteration will have the largest index, because this is in descending order,
			// so use that to initialize our length.
			if match.Teams == nil {
				match.Teams = make([]*Team, teamNumber+1)
			}

			team := new(Team)
			team.Points = points
			team.Bosses = []int{}
			match.Teams[teamNumber] = team
		}

		if err := rows.Err(); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		rows, err = txQueryMatchAdventureTeamBoss.Query(match.ID)
		if err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		defer rows.Close()
		for rows.Next() {
			var teamNumber, id int
			// TODO: Make use of first blood for bosses
			var firstBlood bool

			if err := rows.Scan(&teamNumber, &id, &firstBlood); err != nil {
				c.Error(errors.Join(ErrDatabase, err))
				abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
				return
			}

			match.Teams[teamNumber].Bosses = append(match.Teams[teamNumber].Bosses, id)
		}

		if err := rows.Err(); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		matches = append(matches, match)
	}

	if err := rows.Err(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, matches)
}

type PostRecordMatchBody struct {
	Replay  *multipart.FileHeader `form:"replay" binding:"required"`
	Payload string                `form:"payload" binding:"required"`
}

func recordMatchPayload(c *gin.Context, tx *sql.Tx, m Match, official OfficialIdentity) (OfficialMatch, error) {
	mRes := OfficialMatch{Match: m, OfficialIdentity: official.Subject}

	// FIXME: MORE CONTEXT!!
	txExecInsertMatchStmt := tx.Stmt(execInsertMatchStmt)
	defer txExecInsertMatchStmt.Close()
	txExecInsertMatchPlayerStmt := tx.Stmt(execInsertMatchPlayerStmt)
	defer txExecInsertMatchPlayerStmt.Close()
	txExecIncreaseGemsStmt := tx.Stmt(execIncreaseGemsStmt)
	defer txExecIncreaseGemsStmt.Close()
	txExecInsertMatchMetricStmt := tx.Stmt(execInsertMatchMetricStmt)
	defer txExecInsertMatchMetricStmt.Close()

	// Assume PvP Adventure
	txExecInsertMatchAdventureTeam := tx.Stmt(execInsertMatchAdventureTeam)
	defer txExecInsertMatchAdventureTeam.Close()
	txExecInsertMatchAdventureTeamBoss := tx.Stmt(execInsertMatchAdventureTeamBoss)
	defer txExecInsertMatchAdventureTeamBoss.Close()

	var recorded int64
	if err := txExecInsertMatchStmt.QueryRow(m.Start.Unix(), m.End.Unix(), m.GameMode, official.Subject, official.Fingerprint).Scan(&mRes.ID, &recorded); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return OfficialMatch{}, err
	}

	mRes.Recorded = time.Unix(recorded, 0)

	for id, player := range m.Players {
		if _, err := txExecInsertMatchPlayerStmt.Exec(mRes.ID, id, player.Name, player.Team, player.Reward, player.Kills, player.Deaths); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return OfficialMatch{}, err
		}

		if player.Reward > 0 {
			if _, err := txExecIncreaseGemsStmt.Exec(id, player.Reward); err != nil {
				c.Error(errors.Join(ErrDatabase, err))
				abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
				return OfficialMatch{}, err
			}

		}
	}

	for key, value := range m.Metrics {
		if _, err := txExecInsertMatchMetricStmt.Exec(mRes.ID, key, value); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return OfficialMatch{}, err
		}
	}

	// Assume PvP Adventure
	for i, team := range m.Teams[1:] {
		if team == nil {
			continue
		}

		teamNumber := i + 1

		if _, err := txExecInsertMatchAdventureTeam.Exec(mRes.ID, teamNumber, team.Points); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return OfficialMatch{}, err
		}

		for _, boss := range team.Bosses {
			// TODO: implement first blood bosses here!
			if _, err := txExecInsertMatchAdventureTeamBoss.Exec(mRes.ID, teamNumber, boss, false); err != nil {
				c.Error(errors.Join(ErrDatabase, err))
				abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
				return OfficialMatch{}, err
			}
		}
	}

	return mRes, nil
}

func recordMatchHandler(c *gin.Context) {
	// TODO: i don't believe this check is needed and it could be removed
	officialAny, hasOfficial := c.Get("official")
	if !hasOfficial {
		abortStatusError(c, http.StatusUnauthorized, ErrNoOfficialAuth)
		return
	}

	official := officialAny.(OfficialIdentity)

	var m Match
	if err := c.ShouldBind(&m); err != nil {
		abortStatusError(c, http.StatusBadRequest, errors.Join(ErrMalformedBody, err))
		return
	}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	defer tx.Rollback()

	mRes, err := recordMatchPayload(c, tx, m, official)
	if err != nil {
		return
	}

	if err := tx.Commit(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, mRes)
}

func recordMatchReplayHandler(c *gin.Context) {
	// TODO: i don't believe this check is needed and it could be removed
	officialAny, hasOfficial := c.Get("official")
	if !hasOfficial {
		abortStatusError(c, http.StatusUnauthorized, ErrNoOfficialAuth)
		return
	}

	official := officialAny.(OfficialIdentity)

	var body PostRecordMatchBody
	if err := c.ShouldBind(&body); err != nil {
		abortStatusError(c, http.StatusBadRequest, errors.Join(ErrMalformedBody, err))
		return
	}

	// TODO: this should be required to have Content-Type: application/json
	// 		 it seems that this behavior is not possible to implement with gin at the moment.
	var m Match
	if err := json.Unmarshal([]byte(body.Payload), &m); err != nil {
		abortStatusError(c, http.StatusBadRequest, errors.Join(ErrMalformedBody, err))
		return
	}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	defer tx.Rollback()

	txExecUpdateMatchReplayStmt := tx.Stmt(execUpdateMatchReplayStmt)
	defer txExecUpdateMatchReplayStmt.Close()

	mRes, err := recordMatchPayload(c, tx, m, official)
	if err != nil {
		return
	}

	replay, err := body.Replay.Open()
	if err != nil {
		c.Error(errors.Join(ErrReplaySave, err))
		abortStatusError(c, http.StatusInternalServerError, ErrReplaySave)
		return
	}
	defer replay.Close()

	var fileGameMode string
	switch mRes.GameMode {
	case "pvpa":
		fileGameMode = "TPVPA"
	default:
		c.Error(fmt.Errorf("%w: using provided game mode for replay file name (%s)", ErrUnsupportedGameMode, mRes.GameMode))
		fileGameMode = mRes.GameMode
	}

	// FIXME: nil context? BAD!!!
	_, err = s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: &replayBucket,
		Key:    aws.String(fmt.Sprintf("%s %d.reese", fileGameMode, mRes.ID)),
		Body:   replay,
		ACL:    types.ObjectCannedACLPublicRead,
	})

	// FIXME: put legal hold/lock

	if err != nil {
		c.Error(errors.Join(ErrReplaySave, err))
		abortStatusError(c, http.StatusInternalServerError, ErrReplaySave)
		return
	}

	replayUrl := fmt.Sprintf("%s/%d.reese", replayOriginEndpoint, mRes.ID)
	if _, err := txExecUpdateMatchReplayStmt.Exec(replayUrl, mRes.ID); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	// FIXME: need to undo the replay upload then!
	if err := tx.Commit(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	mRes.ReplayUrl = ptr.String(replayUrl)

	c.JSON(http.StatusOK, mRes)
}

func getAccoladesHandler(c *gin.Context) {
	rows, err := queryAccoladesStmt.Query()
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	accolades := map[string]string{}
	for rows.Next() {
		var name, owner string

		if err := rows.Scan(&name, &owner); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		accolades[name] = owner
	}

	if err := rows.Err(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, accolades)
}

type GetAchievementsQuery struct {
	GameMode string `form:"gameMode" binding:"required"`
}

func getAchievementsHandler(c *gin.Context) {
	id, hasID := c.Get("id")
	if !hasID {
		abortStatusError(c, http.StatusUnauthorized, ErrNoSteamAuth)
		return
	}

	var query GetAchievementsQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		abortStatusError(c, http.StatusBadRequest, errors.Join(ErrMalformedQuery, err))
		return
	}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer tx.Rollback()

	txQueryAchievementDefsStmt := tx.Stmt(queryAchievementDefsStmt)
	defer txQueryAchievementDefsStmt.Close()
	txQueryAchievementsStmt := tx.Stmt(queryAchievementsStmt)
	defer txQueryAchievementsStmt.Close()

	rows, err := txQueryAchievementDefsStmt.Query(query.GameMode)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	achievements := map[string]Achievement{}
	for rows.Next() {
		var name string
		a := Achievement{}

		if err := rows.Scan(&name, &a.Target, &a.Reward); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}

		achievements[name] = a
	}

	if err := rows.Err(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	rows, err = txQueryAchievementsStmt.Query(query.GameMode, id)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var progress uint32
		var claimed *uint32
		var claimed_at *int64

		if err := rows.Scan(&name, &progress, &claimed, &claimed_at); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
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
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	if err := tx.Commit(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, achievements)
}

type PostAchievementProgressBody struct {
	ID          uint64 `json:"id,string" binding:"required"`
	Achievement struct {
		Name     string `json:"name" binding:"required"`
		GameMode string `json:"gameMode" binding:"required"`
	} `json:"achievement" binding:"required"`
	Delta *uint32 `json:"delta,omitempty"`
}

func progressAchievementHandler(c *gin.Context) {
	var b PostAchievementProgressBody
	if err := c.ShouldBindJSON(&b); err != nil {
		abortStatusError(c, http.StatusBadRequest, errors.Join(ErrMalformedBody, err))
		return
	}

	if b.Delta != nil && *b.Delta == 0 {
		abortStatusError(c, http.StatusBadRequest, ErrAchievementNoDeltaZero)
		return
	}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer tx.Rollback()

	txQueryAchievementDefStmt := tx.Stmt(queryAchievementDefStmt)
	defer txQueryAchievementDefStmt.Close()
	txQueryAchievementStmt := tx.Stmt(queryAchievementStmt)
	defer txQueryAchievementStmt.Close()
	txExecAchievementProgress := tx.Stmt(execAchievementProgress)
	defer txExecAchievementProgress.Close()

	a := Achievement{}
	var noExtMut bool

	if err := txQueryAchievementDefStmt.QueryRow(b.Achievement.Name, b.Achievement.GameMode).Scan(&a.Target, &a.Reward, &noExtMut); err == sql.ErrNoRows {
		abortStatusError(c, http.StatusNotFound, ErrAchievementDoesNotExist)
		return
	} else if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	if noExtMut {
		abortStatusError(c, http.StatusBadRequest, ErrAchievementNoExtMut)
		return
	}

	var claimed *uint32
	var claimed_at *int64
	if err := txQueryAchievementStmt.QueryRow(b.Achievement.Name, b.Achievement.GameMode, b.ID).Scan(&a.Progress, &claimed, &claimed_at); err != nil && err != sql.ErrNoRows {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	if claimed != nil || claimed_at != nil {
		abortStatusError(c, http.StatusBadRequest, ErrAchievementAlreadyClaimed)
		return
	}

	initialProgress := a.Progress
	if b.Delta != nil {
		a.Progress = min(a.Target, a.Progress+*b.Delta)
	} else {
		a.Progress = a.Target
	}

	if a.Progress != initialProgress {
		if _, err = txExecAchievementProgress.Exec(b.Achievement.Name, b.Achievement.GameMode, b.ID, a.Progress, a.Progress); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, a)
}

type PostAchievementClaimBody struct {
	Name     string `json:"name" binding:"required"`
	GameMode string `json:"gameMode" binding:"required"`
}

func claimAchievementHandler(c *gin.Context) {
	id, hasID := c.Get("id")
	if !hasID {
		abortStatusError(c, http.StatusUnauthorized, ErrNoSteamAuth)
		return
	}

	var b PostAchievementClaimBody
	if err := c.ShouldBindJSON(&b); err != nil {
		abortStatusError(c, http.StatusBadRequest, errors.Join(ErrMalformedBody, err))
		return
	}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	defer tx.Rollback()

	txQueryAchievementDefStmt := tx.Stmt(queryAchievementDefStmt)
	defer txQueryAchievementDefStmt.Close()
	txExecAchievementClaim := tx.Stmt(execAchievementClaim)
	defer txExecAchievementClaim.Close()
	txExecIncreaseGemsStmt := tx.Stmt(execIncreaseGemsStmt)
	defer txExecIncreaseGemsStmt.Close()

	a := Achievement{}
	var noExtMut bool

	if err := txQueryAchievementDefStmt.QueryRow(b.Name, b.GameMode).Scan(&a.Target, &a.Reward, &noExtMut); err == sql.ErrNoRows {
		abortStatusError(c, http.StatusNotFound, ErrAchievementDoesNotExist)
		return
	} else if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	var claimed_at int64
	if err = txExecAchievementClaim.QueryRow(a.Reward, b.Name, b.GameMode, id, a.Target).Scan(&claimed_at, &a.Progress); err == sql.ErrNoRows {
		c.Status(http.StatusBadRequest)
		return
	} else if err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}
	a.ClaimedAt = new(time.Time)
	*a.ClaimedAt = time.Unix(claimed_at, 0)

	if a.Reward > 0 {
		if _, err = txExecIncreaseGemsStmt.Exec(id, a.Reward); err != nil {
			c.Error(errors.Join(ErrDatabase, err))
			abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		c.Error(errors.Join(ErrDatabase, err))
		abortStatusError(c, http.StatusInternalServerError, ErrDatabase)
		return
	}

	c.JSON(http.StatusOK, a)
}

const dbSchema = `
pragma journal_mode = WAL;
pragma busy_timeout = 5000;
pragma synchronous = normal;
pragma mmap_size = 30000000000;

CREATE TABLE IF NOT EXISTS gem (
    id TEXT NOT NULL,
    count INTEGER NOT NULL DEFAULT 0,
    CONSTRAINT pk PRIMARY KEY (id),
    CONSTRAINT count_check CHECK (count >= 0)
);

CREATE TABLE IF NOT EXISTS product (
    prototype TEXT NOT NULL,
    name TEXT NOT NULL,
    price INTEGER NOT NULL,
    accolade TEXT,
    achievement TEXT,
    available INTEGER NOT NULL DEFAULT FALSE,
    CONSTRAINT pk PRIMARY KEY (prototype, name),
    CONSTRAINT price_check CHECK (price >= 0),
    CONSTRAINT availabile_check CHECK (available IN (FALSE, TRUE))
);

CREATE TABLE IF NOT EXISTS item (
    prototype TEXT NOT NULL,
    name TEXT NOT NULL,
    equipped INTEGER NOT NULL DEFAULT FALSE,
    owner TEXT NOT NULL,
    issued INTEGER NOT NULL DEFAULT (unixepoch()),
    purchase_price INTEGER,
    accolade TEXT,
    CONSTRAINT pk PRIMARY KEY (prototype, name, owner),
    CONSTRAINT equipped_check CHECK (equipped IN (FALSE, TRUE)),
    CONSTRAINT purchase_price_check CHECK (purchase_price >= 0)
);

CREATE TABLE IF NOT EXISTS match (
	id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
	start INTEGER NOT NULL,
	end INTEGER NOT NULL,
	game_mode TEXT NOT NULL,
	recorded INTEGER NOT NULL DEFAULT (unixepoch()),
	replay_url TEXT,
	official_identity_subject TEXT NOT NULL,
	official_identity_fingerprint TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS match_player (
	match_id INTEGER NOT NULL,
	id TEXT NOT NULL,
	name TEXT NOT NULL,
	team INTEGER NOT NULL,
	reward INTEGER NOT NULL DEFAULT 0,
	kills INTEGER NOT NULL,
	deaths INTEGER NOT NULL,
	CONSTRAINT pk PRIMARY KEY (match_id, id),
	CONSTRAINT reward_check CHECK (reward >= 0)
);

CREATE TABLE IF NOT EXISTS match_pvpa_team (
	match_id INTEGER NOT NULL,
	team INTEGER NOT NULL,
	points INTEGER NOT NULL,
	CONSTRAINT pk PRIMARY KEY (match_id, team),
	CONSTRAINT team_check CHECK (team > 0),
	CONSTRAINT points_check CHECK (points >= 0)
);

CREATE TABLE IF NOT EXISTS match_pvpa_team_boss (
	match_id INTEGER NOT NULL,
	team INTEGER NOT NULL,
	id INTEGER NOT NULL,
	first_blood INTEGER NOT NULL,
	CONSTRAINT pk PRIMARY KEY (match_id, team, id),
	CONSTRAINT team_check CHECK (team > 0)
);

CREATE TABLE IF NOT EXISTS match_metric (
	match_id INTEGER NOT NULL,
	"key" TEXT NOT NULL,
	value TEXT NOT NULL,
	CONSTRAINT pk PRIMARY KEY (match_id, "key")
);

CREATE TABLE IF NOT EXISTS accolade (
	name TEXT NOT NULL,
	owner TEXT NOT NULL,
	CONSTRAINT pk PRIMARY KEY (name, owner)
);

CREATE TABLE IF NOT EXISTS achievement_def (
	name TEXT NOT NULL,
	game_mode TEXT NOT NULL,
	target INTEGER NOT NULL,
	reward INTEGER NOT NULL,
	no_ext_mut INTEGER NOT NULL DEFAULT FALSE,
	CONSTRAINT pk PRIMARY KEY (name, game_mode),
	CONSTRAINT target_check CHECK (target > 0),
	CONSTRAINT reward_check CHECK (reward >= 0),
	CONSTRAINT no_ext_mut_check CHECK (no_ext_mut IN (FALSE, TRUE))
);

CREATE TABLE IF NOT EXISTS achievement (
	name TEXT NOT NULL,
	game_mode TEXT NOT NULL,
	id TEXT NOT NULL,
	progress INTEGER NOT NULL,
	claimed INTEGER,
	claimed_at INTEGER,
	CONSTRAINT pk PRIMARY KEY (name, game_mode, id),
	CONSTRAINT progress_check CHECK (progress > 0)
);

CREATE INDEX IF NOT EXISTS idx_item_owner ON item(owner);
CREATE INDEX IF NOT EXISTS idx_product_available ON product(available);

CREATE INDEX IF NOT EXISTS idx_accolade_owner ON accolade(owner);

CREATE INDEX IF NOT EXISTS idx_achievement_def_game_mode ON achievement_def(game_mode);
CREATE INDEX IF NOT EXISTS idx_achievement_game_mode_id ON achievement(game_mode, id);
`

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
	replayBucket = os.Getenv("TAVERNKEEP_REPLAY_BUCKET")
	replayOriginEndpoint = os.Getenv("TAVERNKEEP_REPLAY_ORIGIN_ENDPOINT")

	ctx := context.Background()

	sdkConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}

	s3Client = s3.NewFromConfig(sdkConfig)

	return nil
}

func main() {
	if err := initS3(); err != nil {
		log.Fatalln(err)
	}

	var err error

	if steamApiKey, err = readSteamApiKey(); err != nil {
		log.Fatalln(err)
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

	store := cookie.NewStore([]byte("EHcBa7zSQPhKoVfirITVMxubVwDO2t1L"), []byte("rcLrOhy3BZNS1FEBu4MKJsQR6yLn2JEZ"))
	store.Options(sessions.Options{
		Path:     "/",
		Domain:   "api.tpvpa.terraria.sh",
		Secure:   true,
		HttpOnly: true,
	})

	r := gin.Default()

	// r.Use(sessions.Sessions("session", store))

	// r.Use(jwtCachedAuthMiddleware())

	r.SetTrustedProxies([]string{"127.0.0.1"})

	r.GET("/shop/v1", listProductsHandler)
	r.POST("/shop/purchase/v1", requireContentTypeJson, steamAuthTicketHandler, purchaseHandler)

	r.GET("/profile/v1", getProfileHandler)
	r.GET("/profile/inventory/v1", steamAuthTicketHandler, getInventoryHandler)
	r.POST("/profile/equip/v1", requireContentTypeJson, steamAuthTicketHandler, equipItemHandler)

	r.GET("/match/v1", getMatchHandler)
	r.POST("/match/v1", requireContentTypeJson, officialAuthHandler, recordMatchHandler)
	r.POST("/match/v2", requireContentTypeMultipartForm, officialAuthHandler, recordMatchReplayHandler)

	r.GET("/accolades/v1", getAccoladesHandler)

	r.GET("/achievements/v1", steamAuthTicketHandler, getAchievementsHandler)
	r.POST("/achievements/v1", requireContentTypeJson, officialAuthHandler, progressAchievementHandler)
	r.POST("/achievements/claim/v1", requireContentTypeJson, steamAuthTicketHandler, claimAchievementHandler)

	if err := r.Run(":8080"); err != nil {
		log.Fatalln(fmt.Errorf("unable to bind Gin engine to port 8080: %w", err))
	}
}
