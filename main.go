package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

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

	Price uint32 `json:"price" binding:"required"`
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

var ErrMalformedMatch = errors.New("malformed match")

var permittedMatchGameModes = []string{"pvpa"}

func (m *Match) UnmarshalJSON(b []byte) error {
	type aux Match
	if err := json.Unmarshal(b, (*aux)(m)); err != nil {
		return err
	}

	if len(m.Players) == 0 {
		return ErrMalformedMatch
	}

	// NOTE: this is PvP Adventure specific
	if len(m.Teams) == 0 || m.Teams[0] != nil {
		return ErrMalformedMatch
	}

	if !slices.Contains(permittedMatchGameModes, m.GameMode) {
		return ErrMalformedMatch
	}

	return nil
}

type OfficialMatch struct {
	ID               int64     `json:"id" binding:"required"`
	Recorded         time.Time `json:"recorded" binding:"required"`
	OfficialIdentity string    `json:"officialIdentity" binding:"required"`

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
	steamApiKey string
	db          *sql.DB

	queryGemsStmt               *sql.Stmt
	queryProductsStmt           *sql.Stmt
	queryProductStmt            *sql.Stmt
	queryEquipmentStmt          *sql.Stmt
	queryInventoryStmt          *sql.Stmt
	queryInventoryItemCountStmt *sql.Stmt
	queryAccoladesStmt          *sql.Stmt

	execEquipStmt      *sql.Stmt
	execReduceGemsStmt *sql.Stmt
	execInsertItemStmt *sql.Stmt

	queryMatchStmt              *sql.Stmt
	queryMatchesStmt            *sql.Stmt
	queryMatchPlayerStmt        *sql.Stmt
	queryMatchAdventureTeam     *sql.Stmt
	queryMatchAdventureTeamBoss *sql.Stmt

	execInsertMatchStmt              *sql.Stmt
	execInsertMatchPlayerStmt        *sql.Stmt
	execInsertMatchAdventureTeam     *sql.Stmt
	execInsertMatchAdventureTeamBoss *sql.Stmt
	execInsertMatchMetricStmt        *sql.Stmt

	execIncreaseGemsStmt *sql.Stmt

	queryAchievementDefsStmt *sql.Stmt
	queryAchievementsStmt    *sql.Stmt
)

func readSteamApiKey() (string, error) {
	data, err := os.ReadFile("./secret/steam_api_key.txt")
	if err != nil {
		return "", fmt.Errorf("failed to read Steam API key: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}

func prepareStatements() error {
	var err error
	if queryGemsStmt, err = db.Prepare("SELECT count FROM gem WHERE id = ?"); err != nil {
		return err
	}

	if queryProductsStmt, err = db.Prepare("SELECT prototype, name, price FROM product WHERE available = TRUE"); err != nil {
		return err
	}

	if queryProductStmt, err = db.Prepare("SELECT price FROM product WHERE available = TRUE AND prototype = ? AND name = ?"); err != nil {
		return err
	}

	if queryEquipmentStmt, err = db.Prepare("SELECT prototype, name FROM item WHERE equipped = TRUE AND owner = ?"); err != nil {
		return err
	}

	if queryInventoryStmt, err = db.Prepare("SELECT prototype, name, issued, purchase_price FROM item WHERE owner = ?"); err != nil {
		return err
	}

	if queryInventoryItemCountStmt, err = db.Prepare("SELECT COUNT(*) FROM item WHERE prototype = ? AND name = ? AND owner = ?"); err != nil {
		return err
	}

	if queryAccoladesStmt, err = db.Prepare("SELECT name, owner FROM accolade"); err != nil {
		return err
	}

	// Subquery: Set equipped with matching name to 1, and all non-matching 0 (un-equip everything else)
	if execEquipStmt, err = db.Prepare(`UPDATE item
SET equipped = CASE
   WHEN name = ? THEN 1
   ELSE 0
END
WHERE prototype = ?
 AND owner = ?`); err != nil {
		return err
	}

	if execReduceGemsStmt, err = db.Prepare("UPDATE gem SET count = count - ? WHERE count >= ? AND id = ?"); err != nil {
		return err
	}

	if execInsertItemStmt, err = db.Prepare("INSERT INTO item (prototype, name, owner, purchase_price) VALUES (?, ?, ?, ?) RETURNING issued"); err != nil {
		return err
	}

	if queryMatchStmt, err = db.Prepare("SELECT start, end, game_mode, recorded, official_identity_subject FROM match WHERE id = ?"); err != nil {
		return err
	}

	if queryMatchesStmt, err = db.Prepare("SELECT match_id FROM match_player WHERE id = ?"); err != nil {
		return err
	}

	if queryMatchPlayerStmt, err = db.Prepare("SELECT id, name, team, reward, kills, deaths FROM match_player WHERE match_id = ?"); err != nil {
		return err
	}

	if queryMatchAdventureTeam, err = db.Prepare("SELECT team, points FROM match_pvpa_team WHERE match_id = ? ORDER BY team DESC"); err != nil {
		return err
	}

	if queryMatchAdventureTeamBoss, err = db.Prepare("SELECT team, id, first_blood FROM match_pvpa_team_boss WHERE match_id = ?"); err != nil {
		return err
	}

	if execInsertMatchStmt, err = db.Prepare("INSERT INTO match (start, end, game_mode, official_identity_subject, official_identity_fingerprint) VALUES (?, ?, ?, ?, ?) RETURNING id, recorded"); err != nil {
		return err
	}

	if execInsertMatchPlayerStmt, err = db.Prepare("INSERT INTO match_player (match_id, id, name, team, reward, kills, deaths) VALUES (?, ?, ?, ?, ?, ?, ?)"); err != nil {
		return err
	}

	if execInsertMatchAdventureTeam, err = db.Prepare("INSERT INTO match_pvpa_team (match_id, team, points) VALUES (?, ?, ?)"); err != nil {
		return err
	}

	if execInsertMatchAdventureTeamBoss, err = db.Prepare("INSERT INTO match_pvpa_team_boss (match_id, team, id, first_blood) VALUES (?, ?, ?, ?)"); err != nil {
		return err
	}

	if execInsertMatchMetricStmt, err = db.Prepare("INSERT INTO match_metric (match_id, key, value) VALUES (?, ?, ?)"); err != nil {
		return err
	}

	if execIncreaseGemsStmt, err = db.Prepare("INSERT INTO gem (id, count) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET count=count + excluded.count"); err != nil {
		return err
	}

	return nil
}

func closeStatements() {
	execIncreaseGemsStmt.Close()
	execInsertMatchMetricStmt.Close()
	execInsertMatchAdventureTeamBoss.Close()
	execInsertMatchAdventureTeam.Close()
	execInsertMatchPlayerStmt.Close()
	execInsertMatchStmt.Close()

	queryMatchAdventureTeamBoss.Close()
	queryMatchAdventureTeam.Close()
	queryMatchPlayerStmt.Close()
	queryMatchesStmt.Close()
	queryMatchStmt.Close()

	execInsertItemStmt.Close()
	execReduceGemsStmt.Close()
	execEquipStmt.Close()
	queryAccoladesStmt.Close()
	queryInventoryItemCountStmt.Close()
	queryInventoryStmt.Close()
	queryEquipmentStmt.Close()
	queryProductStmt.Close()
	queryProductsStmt.Close()
	queryGemsStmt.Close()
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

func officialAuthHandler(c *gin.Context) {
	defer c.Next()

	officialVerified, hasOfficialVerified := c.Request.Header["Official-Verified"]
	if !hasOfficialVerified || officialVerified[0] == "NONE" {
		return
	}

	officialIdentitySubject, hasOfficialIdentitySubject := c.Request.Header["Official-Identity-Subject"]
	if !hasOfficialIdentitySubject {
		log.Println("Missing official identity subject from proxy request!")
		return
	}

	officialIdentityFingerprint, hasOfficialIdentityFingerprint := c.Request.Header["Official-Identity-Fingerprint"]
	if !hasOfficialIdentityFingerprint {
		log.Println("Missing official identity fingerprint from proxy request!")
		return
	}

	if officialVerified[0] != "SUCCESS" {
		log.Println("Official authentication failure for", c.ClientIP(), "result", officialVerified[0])
		return
	}

	c.Set("official_identity_subject", officialIdentitySubject[0])
	c.Set("official_identity_fingerprint", officialIdentityFingerprint[0])
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
				log.Println("Steam failed to respond for auth", err)
				c.Error(err)
				c.AbortWithStatus(http.StatusServiceUnavailable)
				return
			}
			defer res.Body.Close()

			if res.StatusCode != http.StatusOK {
				log.Println("Steam auth failed with non-OK response", res.Status)
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}

			authUserTicketRes := AuthenticateUserTicket{}
			json.NewDecoder(res.Body).Decode(&authUserTicketRes)

			if authUserTicketRes.Response.Error != nil {
				log.Println("Steam auth failed with error, code", authUserTicketRes.Response.Error.ErrorCode, "(", authUserTicketRes.Response.Error.ErrorDesc, ")")
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}

			if result := authUserTicketRes.Response.Params.Result; result != "OK" {
				log.Println("Steam auth failed with non-OK result", result)
				c.String(http.StatusUnauthorized, result)
				c.Abort()
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
		c.Status(http.StatusBadRequest)
		return
	}

	id := query.ID

	profile := Profile{
		ID: id,
	}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	// FIXME: MORE CONTEXT!!
	txQueryGemsStmt := tx.Stmt(queryGemsStmt)
	defer txQueryGemsStmt.Close()
	txQueryEquipmentStmt := tx.Stmt(queryEquipmentStmt)
	defer txQueryEquipmentStmt.Close()

	if err := txQueryGemsStmt.QueryRow(id).Scan(&profile.Gems); err != nil && err != sql.ErrNoRows {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}

	rows, err := txQueryEquipmentStmt.Query(id)
	if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	profile.Equipment = map[string]string{}
	for rows.Next() {
		var prototype, name string
		if err := rows.Scan(&prototype, &name); err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}

		profile.Equipment[prototype] = name
	}

	if err := rows.Err(); err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, profile)
}

func listProductsHandler(c *gin.Context) {
	rows, err := queryProductsStmt.Query()
	if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	products := []Product{}
	var product Product
	for rows.Next() {
		if err := rows.Scan(&product.Prototype, &product.Name, &product.Price); err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}
		products = append(products, product)
	}

	if err := rows.Err(); err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
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
		c.Status(http.StatusUnauthorized)
		return
	}

	var body PostPurchaseBody
	if c.BindJSON(&body) != nil {
		return
	}

	item := Item{DefinitionReference: body.DefinitionReference}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
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
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}

	if count > 0 {
		c.Status(http.StatusConflict)
		return
	}

	err = txQueryProductStmt.QueryRow(item.Prototype, item.Name).Scan(&item.PurchasePrice)

	if err == sql.ErrNoRows {
		c.Status(http.StatusNotFound)
		return
	} else if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}

	result, err := txExecReduceGemsStmt.Exec(item.PurchasePrice, item.PurchasePrice, id)
	if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}

	affected, err := result.RowsAffected()
	if err != nil {
		// FIXME: no panics!
		panic("can't get affected rows?")
	}

	if affected == 0 {
		c.Status(http.StatusForbidden)
		return
	}

	var issued int64
	if err := txExecInsertItemStmt.QueryRow(item.Prototype, item.Name, id, item.PurchasePrice).Scan(&issued); err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}

	item.Issued = time.Unix(issued, 0)

	if err := tx.Commit(); err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
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
		c.Status(http.StatusUnauthorized)
		return
	}

	var postProfileEquip PostProfileEquip
	if c.BindJSON(&postProfileEquip) != nil {
		return
	}

	_, err := execEquipStmt.Exec(postProfileEquip.Name, postProfileEquip.Prototype, id)
	if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusNoContent)
}

func getInventoryHandler(c *gin.Context) {
	id, hasID := c.Get("id")
	if !hasID {
		c.Status(http.StatusUnauthorized)
		return
	}

	rows, err := queryInventoryStmt.Query(id)
	if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	items := []Item{}
	for rows.Next() {
		var issued int64
		var item Item

		if err := rows.Scan(&item.Prototype, &item.Name, &issued, &item.PurchasePrice); err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}

		item.Issued = time.Unix(issued, 0)
		items = append(items, item)
	}

	if err := rows.Err(); err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
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
		c.Status(http.StatusBadRequest)
		return
	}

	id := query.ID

	var matches = []OfficialMatch{}
	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	// FIXME: MORE CONTEXT!!
	txQueryMatchesStmt := tx.Stmt(queryMatchesStmt)
	defer txQueryMatchesStmt.Close()

	// query all match ids that this player has been involved in.
	rows, err := txQueryMatchesStmt.Query(id)
	if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
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
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}

		var start, end, recorded int64
		if err := txQueryMatchStmt.QueryRow(match.ID).Scan(&start, &end, &match.GameMode, &recorded, &match.OfficialIdentity); err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}

		match.Start = time.Unix(start, 0)
		match.End = time.Unix(end, 0)
		match.Recorded = time.Unix(recorded, 0)

		rows, err := txQueryMatchPlayerStmt.Query(match.ID)
		if err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		match.Players = map[uint64]Player{}
		for rows.Next() {
			var id uint64
			var player Player

			if err := rows.Scan(&id, &player.Name, &player.Team, &player.Reward, &player.Kills, &player.Deaths); err != nil {
				c.Error(err)
				c.Status(http.StatusInternalServerError)
				return
			}

			match.Players[id] = player
		}

		if err := rows.Err(); err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}

		// Assume PvP Adventure
		txQueryMatchAdventureTeam := tx.Stmt(queryMatchAdventureTeam)
		defer txQueryMatchAdventureTeam.Close()
		txQueryMatchAdventureTeamBoss := tx.Stmt(queryMatchAdventureTeamBoss)
		defer txQueryMatchAdventureTeamBoss.Close()

		rows, err = txQueryMatchAdventureTeam.Query(match.ID)
		if err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var teamNumber, points int

			if err := rows.Scan(&teamNumber, &points); err != nil {
				c.Error(err)
				c.Status(http.StatusInternalServerError)
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
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}

		rows, err = txQueryMatchAdventureTeamBoss.Query(match.ID)
		if err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}

		defer rows.Close()
		for rows.Next() {
			var teamNumber, id int
			// TODO: Make use of first blood for bosses
			var firstBlood bool

			if err := rows.Scan(&teamNumber, &id, &firstBlood); err != nil {
				c.Error(err)
				c.Status(http.StatusInternalServerError)
				return
			}

			match.Teams[teamNumber].Bosses = append(match.Teams[teamNumber].Bosses, id)
		}

		if err := rows.Err(); err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}

		matches = append(matches, match)
	}

	if err := rows.Err(); err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, matches)
}

func recordMatchHandler(c *gin.Context) {
	officialIdentitySubject, hasSubject := c.Get("official_identity_subject")
	if !hasSubject {
		c.Status(http.StatusUnauthorized)
		return
	}

	officialIdentityFingerprint, hasFingerprint := c.Get("official_identity_fingerprint")
	if !hasFingerprint {
		c.Status(http.StatusUnauthorized)
		return
	}

	var m Match
	if c.BindJSON(&m) != nil {
		return
	}

	mRes := OfficialMatch{Match: m, OfficialIdentity: officialIdentitySubject.(string)}

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

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
	if err := txExecInsertMatchStmt.QueryRow(m.Start.Unix(), m.End.Unix(), m.GameMode, officialIdentitySubject.(string), officialIdentityFingerprint.(string)).Scan(&mRes.ID, &recorded); err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}

	mRes.Recorded = time.Unix(recorded, 0)

	for id, player := range m.Players {
		if _, err = txExecInsertMatchPlayerStmt.Exec(mRes.ID, id, player.Name, player.Team, player.Reward, player.Kills, player.Deaths); err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}

		if player.Reward > 0 {
			if _, err = txExecIncreaseGemsStmt.Exec(id, player.Reward); err != nil {
				c.Error(err)
				c.Status(http.StatusInternalServerError)
				return
			}

		}
	}

	for key, value := range m.Metrics {
		if _, err = txExecInsertMatchMetricStmt.Exec(mRes.ID, key, value); err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}
	}

	// Assume PvP Adventure
	for i, team := range m.Teams[1:] {
		if team == nil {
			continue
		}

		teamNumber := i + 1

		if _, err = txExecInsertMatchAdventureTeam.Exec(mRes.ID, teamNumber, team.Points); err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}

		for _, boss := range team.Bosses {
			// TODO: implement first blood bosses here!
			if _, err = txExecInsertMatchAdventureTeamBoss.Exec(mRes.ID, teamNumber, boss, false); err != nil {
				c.Error(err)
				c.Status(http.StatusInternalServerError)
				return
			}
		}
	}

	if err := tx.Commit(); err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, mRes)
}

type GetAchievementsQuery struct {
	GameMode string `form:"gameMode" binding:"required"`
}

func getAccoladesHandler(c *gin.Context) {
	rows, err := queryAccoladesStmt.Query()
	if err != nil {
		c.Error(err)
		return
	}
	defer rows.Close()

	accolades := map[string]string{}
	for rows.Next() {
		var name, owner string

		if err := rows.Scan(&name, &owner); err != nil {
			c.Error(err)
			c.Status(http.StatusInternalServerError)
			return
		}

		accolades[name] = owner
	}

	if err := rows.Err(); err != nil {
		c.Error(err)
		c.Status(http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, accolades)
}

const dbSchema = `
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
	CONSTRAINT pk PRIMARY KEY (name, game_mode),
	CONSTRAINT target_check CHECK (target > 0),
	CONSTRAINT reward_check CHECK (reward >= 0)
);

CREATE TABLE IF NOT EXISTS achievement (
	name TEXT NOT NULL,
	game_mode TEXT NOT NULL,
	id TEXT NOT NULL,
	progress INTEGER NOT NULL,
	claimed INTEGER NOT NULL DEFAULT FALSE,
	CONSTRAINT pk PRIMARY KEY (name, game_mode, id),
	CONSTRAINT claimed_check CHECK (claimed IN (FALSE, TRUE))
);

CREATE INDEX IF NOT EXISTS idx_item_owner ON item(owner);
CREATE INDEX IF NOT EXISTS idx_product_available ON product(available);

CREATE INDEX IF NOT EXISTS idx_accolade_owner ON accolade(owner);
`

func errorHandler(c *gin.Context) {
	c.Next()

	if err := c.Errors.Last(); err != nil {
		log.Println("most recent error:", err)
	}
}

func errorMiddleware() gin.HandlerFunc { return errorHandler }

func main() {
	var err error

	if steamApiKey, err = readSteamApiKey(); err != nil {
		log.Fatalln(err)
	}

	db, err = sql.Open("sqlite3", "./data/tavernkeep.db")

	if err != nil {
		log.Fatalln(fmt.Errorf("failed to open database: %w", err))
	}
	defer db.Close()

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
	// r.Use(steamAuthTicketMiddleware())

	r.Use(errorMiddleware())

	r.SetTrustedProxies([]string{"127.0.0.1"})

	r.GET("/shop/v1", listProductsHandler)
	r.POST("/shop/purchase/v1", steamAuthTicketHandler, purchaseHandler)

	r.GET("/profile/v1", getProfileHandler)
	r.GET("/profile/inventory/v1", steamAuthTicketHandler, getInventoryHandler)
	r.POST("/profile/equip/v1", steamAuthTicketHandler, equipItemHandler)

	r.GET("/match/v1", getMatchHandler)
	r.POST("/match/v1", officialAuthHandler, recordMatchHandler)

	r.GET("/accolades/v1", getAccoladesHandler)

	if err := r.Run(":8080"); err != nil {
		log.Fatalln(fmt.Errorf("unable to bind Gin engine to port 8080: %w", err))
	}
}
