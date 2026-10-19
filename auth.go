package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	sentrygin "github.com/getsentry/sentry-go/gin"

	"github.com/getsentry/sentry-go"
	"github.com/gin-gonic/gin"
)

type OfficialIdentity struct {
	Subject     string
	Fingerprint string
}

func steamAuthTicketHandler(c *gin.Context) {
	ticket, hasTicket := c.Request.Header["Ticket"]

	if !hasTicket {
		abortStatusError(c, http.StatusUnauthorized, fmt.Errorf("%w: must present Ticket header", ErrNoSteamAuth))
		return
	}

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
		err = fmt.Errorf("%w: failed to make authentication request to Steam: %w", ErrNoSteamAuth, err)
		c.Error(err)
		abortStatusError(c, http.StatusServiceUnavailable, err)
		return
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		abortStatusError(c, http.StatusUnauthorized, fmt.Errorf("%w: unsuccessful authentication response from Steam", ErrNoSteamAuth))
		return
	}

	type authResBody struct {
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

	authUserTicketRes := authResBody{}
	if err := json.NewDecoder(res.Body).Decode(&authUserTicketRes); err != nil {
		err = fmt.Errorf("%w: unable to JSON decode Steam response: %w", ErrNoSteamAuth, err)
		c.Error(err)
		abortStatusError(c, http.StatusServiceUnavailable, err)
		return
	}

	if authUserTicketRes.Response.Error != nil {
		abortStatusError(c, http.StatusUnauthorized, fmt.Errorf("%w: authentication failed: %d (%s)", ErrNoSteamAuth, authUserTicketRes.Response.Error.ErrorCode, authUserTicketRes.Response.Error.ErrorDesc))
		return
	}

	if result := authUserTicketRes.Response.Params.Result; result != "OK" {
		abortStatusError(c, http.StatusUnauthorized, fmt.Errorf("%w: authentication failed: %s", ErrNoSteamAuth, result))
		return
	}

	c.Set("id", authUserTicketRes.Response.Params.SteamID)

	if hub := sentrygin.GetHubFromContext(c); hub != nil {
		hub.ConfigureScope(func(scope *sentry.Scope) {
			scope.SetUser(sentry.User{
				ID:        strconv.FormatUint(authUserTicketRes.Response.Params.SteamID, 10),
				IPAddress: c.ClientIP(),
			})
		})
	}

	c.Next()
}

func officialAuthHandler(c *gin.Context) {
	auth, hasAuth := c.Request.Header["Authorization"]
	if !hasAuth || !strings.HasPrefix(auth[0], "Bearer ") {
		abortStatusError(c, http.StatusUnauthorized, ErrNoOfficialAuth)
		return
	}

	key := strings.TrimPrefix(auth[0], "Bearer ")

	// FIXME: nil context? BAD!!!
	tx, err := db.BeginTx(context.TODO(), nil)
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	txQueryOfficialStmt := tx.Stmt(queryOfficialStmt)
	defer txQueryOfficialStmt.Close()

	var name string
	if err = txQueryOfficialStmt.QueryRow(key).Scan(&name); err == sql.ErrNoRows {
		abortStatusError(c, http.StatusUnauthorized, ErrNoOfficialAuth)
		return
	} else if err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	c.Set("official", OfficialIdentity{Subject: name, Fingerprint: key})

	if hub := sentrygin.GetHubFromContext(c); hub != nil {
		hub.ConfigureScope(func(scope *sentry.Scope) {
			scope.SetUser(sentry.User{
				ID:        name,
				IPAddress: c.ClientIP(),
			})
		})
	}

	c.Next()
}
