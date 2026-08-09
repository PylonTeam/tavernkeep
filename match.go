package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/gin-gonic/gin"
)

// FIXME: why when unmarshalling Match does this one not respect required bindings?
type Player struct {
	Name string `json:"name" binding:"required"`
	Team uint32 `json:"team" binding:"required"`

	Reward uint32 `json:"reward,omitempty" binding:"required"`

	Kills  int32 `json:"kills" binding:"required"`
	Deaths int32 `json:"deaths" binding:"required"`
	Winner bool  `json:"winner,omitempty"`

	Stats     map[string]uint32           `json:"stats,omitempty" binding:"required"`
	ItemStats map[string]map[int32]uint32 `json:"itemStats,omitempty" binding:"required"`

	// Arenas
	BossDamage *uint32 `json:"bossDamage,omitempty"`

	// Capture the Gem
	GemCaptures []uint32 `json:"gemCaptures,omitempty"`
}

type Team struct {
	// PvP Adventure
	Points *int32  `json:"points,omitempty"`
	Bosses []int16 `json:"bosses,omitempty"`

	// Capture the Gem
	GemCaptures []uint32 `json:"gemCaptures,omitempty"`

	// Arenas
	BossDamage *uint32 `json:"bossDamage,omitempty"`
}

type Match struct {
	Start    time.Time `json:"start" binding:"required"`
	End      time.Time `json:"end" binding:"required"`
	GameMode string    `json:"gameMode" binding:"required"`

	Players map[uint64]Player `json:"players" binding:"required"`
	Metrics map[string]string `json:"metrics,omitempty" binding:"required"`

	Teams []*Team `json:"teams" binding:"required"`

	// Arenas
	Boss *int16 `json:"boss,omitempty"`
}

func (m *Match) UnmarshalJSON(b []byte) error {
	type aux Match
	if err := json.Unmarshal(b, (*aux)(m)); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedMatch, err)
	}

	isPvpa := m.GameMode == "pvpa"
	isArenas := m.GameMode == "arenas"
	isCtg := m.GameMode == "ctg"

	if !isPvpa && !isArenas && !isCtg {
		return fmt.Errorf("%w: %w: %s", ErrMalformedMatch, ErrUnsupportedGameMode, m.GameMode)
	}

	if m.Start.After(m.End) {
		return fmt.Errorf("%w: cannot start after end", ErrMalformedMatch)
	}

	if len(m.Players) == 0 {
		return fmt.Errorf("%w: no players", ErrMalformedMatch)
	}

	if len(m.Teams) == 0 {
		return fmt.Errorf("%w: no teams", ErrMalformedMatch)
	}

	if m.Teams[0] != nil {
		return fmt.Errorf("%w: team None must be null", ErrMalformedMatch)
	}

	if !isArenas {
		if m.Boss != nil {
			return fmt.Errorf("%w: cannot provide boss for this game mode", ErrMalformedMatch)
		}
	}

	for _, p := range m.Players {
		if !isArenas && p.BossDamage != nil {
			return fmt.Errorf("%w: cannot provide player boss damage for this game mode", ErrMalformedMatch)
		}

		if !isCtg && p.GemCaptures != nil {
			return fmt.Errorf("%w: cannot provide player gem captures for this game mode", ErrMalformedMatch)
		}
	}

	for _, t := range m.Teams {
		if t == nil {
			continue
		}

		if !isPvpa && t.Points != nil {
			return fmt.Errorf("%w: cannot provide points for this game mode", ErrMalformedMatch)
		}

		if !isArenas && t.BossDamage != nil {
			return fmt.Errorf("%w: cannot provide team boss damage for this game mode", ErrMalformedMatch)
		}

		if !isCtg && t.GemCaptures != nil {
			return fmt.Errorf("%w: cannot provide team gem captures for this game mode", ErrMalformedMatch)
		}
	}

	return nil
}

type CompletedMatch struct {
	ID               int64     `json:"id" binding:"required"`
	Recorded         time.Time `json:"recorded" binding:"required"`
	OfficialIdentity string    `json:"officialIdentity" binding:"required"`
	ReplayUrl        *string   `json:"replayUrl,omitempty"`

	Match
}

func scanMatch(rows *sql.Rows, m *CompletedMatch) error {
	var start, end, recorded int64

	if err := rows.Scan(&m.ID, &start, &end, &m.GameMode, &recorded, &m.ReplayUrl, &m.OfficialIdentity, &m.Boss); err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	m.Start = time.Unix(start, 0)
	m.End = time.Unix(end, 0)
	m.Recorded = time.Unix(recorded, 0)
	return nil
}

type queryMatchContext struct {
	txQueryMatchPlayerStmt          *sql.Stmt
	txQueryMatchPlayerStatsStmt     *sql.Stmt
	txQueryMatchPlayerItemStatsStmt *sql.Stmt
	txQueryMatchTeamStmt            *sql.Stmt
	txQueryMatchTeamBossStmt        *sql.Stmt
	txQueryMatchPlayerRoundStmt     *sql.Stmt
	txQueryMatchTeamRoundStmt       *sql.Stmt
}

func queryMatch(ctx *queryMatchContext, m *CompletedMatch) error {
	rows, err := ctx.txQueryMatchPlayerStmt.Query(m.ID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}
	defer rows.Close()

	m.Players = map[uint64]Player{}
	for rows.Next() {
		var id uint64
		var player Player

		if err := rows.Scan(&id, &player.Name, &player.Team, &player.Reward, &player.Kills, &player.Deaths, &player.Winner, &player.BossDamage); err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}

		player.Stats = map[string]uint32{}
		rows, err = ctx.txQueryMatchPlayerStatsStmt.Query(m.ID, id)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}
		defer rows.Close()

		for rows.Next() {
			var key string
			var value uint32
			if err := rows.Scan(&key, &value); err != nil {
				return fmt.Errorf("%w: %w", ErrDatabase, err)
			}

			player.Stats[key] = value
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}

		player.ItemStats = map[string]map[int32]uint32{}
		rows, err = ctx.txQueryMatchPlayerItemStatsStmt.Query(m.ID, id)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}
		defer rows.Close()

		for rows.Next() {
			var key string
			var item int32
			var value uint32
			if err := rows.Scan(&key, &item, &value); err != nil {
				return fmt.Errorf("%w: %w", ErrDatabase, err)
			}

			if vals, hasVals := player.ItemStats[key]; hasVals {
				// FIXME: is any of this needed?
				vals[item] = value
				player.ItemStats[key] = vals
			} else {
				player.ItemStats[key] = map[int32]uint32{item: value}
			}
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}

		rows, err = ctx.txQueryMatchPlayerRoundStmt.Query(m.ID, id)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}
		defer rows.Close()

		for rows.Next() {
			var i int
			var gemCaptures uint32
			if err := rows.Scan(&i, &gemCaptures); err != nil {
				return fmt.Errorf("%w: %w", ErrDatabase, err)
			}

			// first iteration will have the largest index, because this is in descending order,
			// so use that to initialize our length.
			if player.GemCaptures == nil {
				player.GemCaptures = make([]uint32, i+1)
			}

			player.GemCaptures[i] = gemCaptures
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}

		m.Players[id] = player
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	rows, err = ctx.txQueryMatchTeamStmt.Query(m.ID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}
	defer rows.Close()

	for rows.Next() {
		var teamNumber int
		team := new(Team)

		if err := rows.Scan(&teamNumber, &team.Points, &team.BossDamage); err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}

		// first iteration will have the largest index, because this is in descending order,
		// so use that to initialize our length.
		if m.Teams == nil {
			m.Teams = make([]*Team, teamNumber+1)
		}

		m.Teams[teamNumber] = team
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	rows, err = ctx.txQueryMatchTeamBossStmt.Query(m.ID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	defer rows.Close()
	for rows.Next() {
		var teamNumber int
		var id int16
		// TODO: Make use of first blood for bosses
		var firstBlood bool

		if err := rows.Scan(&teamNumber, &id, &firstBlood); err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}

		// first iteration will have the largest index, because this is in descending order,
		// so use that to initialize our length.
		if m.Teams == nil {
			m.Teams = make([]*Team, teamNumber+1)
		}

		if m.Teams[teamNumber].Bosses == nil {
			m.Teams[teamNumber].Bosses = []int16{id}
		} else {
			m.Teams[teamNumber].Bosses = append(m.Teams[teamNumber].Bosses, id)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	rows, err = ctx.txQueryMatchTeamRoundStmt.Query(m.ID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	defer rows.Close()
	for rows.Next() {
		var teamNumber, i int
		var gemCaptures uint32

		if err := rows.Scan(&teamNumber, &i, &gemCaptures); err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}

		// first iteration will have the largest index, because this is in descending order,
		// so use that to initialize our length.
		if m.Teams == nil {
			m.Teams = make([]*Team, teamNumber+1)
		}

		if m.Teams[teamNumber].GemCaptures == nil {
			m.Teams[teamNumber].GemCaptures = make([]uint32, i+1)
		}

		m.Teams[teamNumber].GemCaptures[i] = gemCaptures
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	return nil
}

func getMatchHandler(c *gin.Context) {
	if version := c.GetUint32("version"); version != 1 {
		abortStatusError(c, http.StatusBadRequest, ErrVersion)
		return
	}

	type requestQuery struct {
		ID       *uint64 `form:"id"`
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

	ctx := queryMatchContext{}

	txQueryMatchStmt := tx.Stmt(queryMatchStmt)
	defer txQueryMatchStmt.Close()
	ctx.txQueryMatchPlayerStmt = tx.Stmt(queryMatchPlayerStmt)
	defer ctx.txQueryMatchPlayerStmt.Close()
	ctx.txQueryMatchPlayerStatsStmt = tx.Stmt(queryMatchPlayerStatsStmt)
	defer ctx.txQueryMatchPlayerStatsStmt.Close()
	ctx.txQueryMatchPlayerItemStatsStmt = tx.Stmt(queryMatchPlayerItemStatsStmt)
	defer ctx.txQueryMatchPlayerItemStatsStmt.Close()
	ctx.txQueryMatchTeamStmt = tx.Stmt(queryMatchTeamStmt)
	defer ctx.txQueryMatchTeamStmt.Close()
	ctx.txQueryMatchTeamBossStmt = tx.Stmt(queryMatchTeamBossStmt)
	defer ctx.txQueryMatchTeamBossStmt.Close()
	ctx.txQueryMatchPlayerRoundStmt = tx.Stmt(queryMatchPlayerRoundStmt)
	defer ctx.txQueryMatchPlayerRoundStmt.Close()
	ctx.txQueryMatchTeamRoundStmt = tx.Stmt(queryMatchTeamRoundStmt)
	defer ctx.txQueryMatchTeamRoundStmt.Close()

	var matches = []CompletedMatch{}
	if query.ID != nil {
		// FIXME: MORE CONTEXT!!
		txQueryMatchesStmt := tx.Stmt(queryMatchesStmt)
		defer txQueryMatchesStmt.Close()

		// query all match ids that this player has been involved in.
		rows, err := txQueryMatchesStmt.Query(query.ID)
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		for rows.Next() {
			var m CompletedMatch
			if err = rows.Scan(&m.ID); err != nil {
				err = fmt.Errorf("%w: %w", ErrDatabase, err)
				c.Error(err)
				abortStatusError(c, http.StatusInternalServerError, err)
				return
			}

			rows, err = txQueryMatchStmt.Query(m.ID, query.GameMode)
			if err != nil {
				err = fmt.Errorf("%w: %w", ErrDatabase, err)
				c.Error(err)
				abortStatusError(c, http.StatusInternalServerError, err)
				return
			}

			if !rows.Next() {
				if err = rows.Err(); err != nil {
					err = fmt.Errorf("%w: %w", ErrDatabase, err)
					c.Error(err)
					abortStatusError(c, http.StatusInternalServerError, err)
					return
				}

				continue
			}

			if err = scanMatch(rows, &m); err != nil {
				c.Error(err)
				abortStatusError(c, http.StatusInternalServerError, err)
				return
			}

			if err = queryMatch(&ctx, &m); err != nil {
				c.Error(err)
				abortStatusError(c, http.StatusInternalServerError, err)
				return
			}

			matches = append(matches, m)
		}

		if err = rows.Err(); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}
	} else {
		rows, err := txQueryMatchStmt.Query(nil, query.GameMode)
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		for rows.Next() {
			var m CompletedMatch

			if err = scanMatch(rows, &m); err != nil {
				c.Error(err)
				abortStatusError(c, http.StatusInternalServerError, err)
				return
			}

			if err = queryMatch(&ctx, &m); err != nil {
				c.Error(err)
				abortStatusError(c, http.StatusInternalServerError, err)
				return
			}

			matches = append(matches, m)
		}

		if err = rows.Err(); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}
	}

	c.JSON(http.StatusOK, matches)
}

func recordMatchPayload(tx *sql.Tx, m Match, official OfficialIdentity) (CompletedMatch, error) {
	mRes := CompletedMatch{Match: m, OfficialIdentity: official.Subject}

	// FIXME: MORE CONTEXT!!
	txExecInsertMatchStmt := tx.Stmt(execInsertMatchStmt)
	defer txExecInsertMatchStmt.Close()
	txExecInsertMatchPlayerStmt := tx.Stmt(execInsertMatchPlayerStmt)
	defer txExecInsertMatchPlayerStmt.Close()
	txExecIncreaseGemsStmt := tx.Stmt(execIncreaseGemsStmt)
	defer txExecIncreaseGemsStmt.Close()
	txExecInsertMatchMetricStmt := tx.Stmt(execInsertMatchMetricStmt)
	defer txExecInsertMatchMetricStmt.Close()
	txExecInsertMatchPlayerStatStmt := tx.Stmt(execInsertMatchPlayerStatStmt)
	defer txExecInsertMatchPlayerStatStmt.Close()
	txExecInsertMatchPlayerItemStatStmt := tx.Stmt(execInsertMatchPlayerItemStatStmt)
	defer txExecInsertMatchPlayerItemStatStmt.Close()
	txExecInsertMatchPlayerRoundStmt := tx.Stmt(execInsertMatchPlayerRoundStmt)
	defer txExecInsertMatchPlayerRoundStmt.Close()
	txExecInsertMatchTeamRoundStmt := tx.Stmt(execInsertMatchTeamRoundStmt)
	defer txExecInsertMatchTeamRoundStmt.Close()

	var recorded int64
	if err := txExecInsertMatchStmt.QueryRow(m.Start.Unix(), m.End.Unix(), m.GameMode, official.Subject, official.Fingerprint, m.Boss).Scan(&mRes.ID, &recorded); err != nil {
		return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	mRes.Recorded = time.Unix(recorded, 0)

	for id, player := range m.Players {
		if _, err := txExecInsertMatchPlayerStmt.Exec(mRes.ID, id, player.Name, player.Team, player.Reward, player.Kills, player.Deaths, player.Winner, player.BossDamage); err != nil {
			return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
		}

		if player.Reward > 0 {
			if _, err := txExecIncreaseGemsStmt.Exec(id, player.Reward); err != nil {
				return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
			}
		}

		for key, value := range player.Stats {
			if _, err := txExecInsertMatchPlayerStatStmt.Exec(mRes.ID, id, key, value); err != nil {
				return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
			}
		}

		for key, values := range player.ItemStats {
			for item, value := range values {
				if _, err := txExecInsertMatchPlayerItemStatStmt.Exec(mRes.ID, id, key, item, value); err != nil {
					return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
				}
			}
		}

		for i, gemCaptures := range player.GemCaptures {
			if _, err := txExecInsertMatchPlayerRoundStmt.Exec(mRes.ID, id, i, gemCaptures); err != nil {
				return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
			}
		}

		for key, value := range m.Metrics {
			if _, err := txExecInsertMatchMetricStmt.Exec(mRes.ID, key, value); err != nil {
				return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
			}
		}
	}

	txExecInsertMatchTeamStmt := tx.Stmt(execInsertMatchTeamStmt)
	defer txExecInsertMatchTeamStmt.Close()
	txExecInsertMatchTeamBossStmt := tx.Stmt(execInsertMatchTeamBossStmt)
	defer txExecInsertMatchTeamBossStmt.Close()

	for i, team := range m.Teams[1:] {
		if team == nil {
			continue
		}

		teamNumber := i + 1

		if _, err := txExecInsertMatchTeamStmt.Exec(mRes.ID, teamNumber, team.Points, team.BossDamage); err != nil {
			return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
		}

		for _, boss := range team.Bosses {
			// TODO: implement first blood bosses here!
			if _, err := txExecInsertMatchTeamBossStmt.Exec(mRes.ID, teamNumber, boss, false); err != nil {
				return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
			}
		}

		for i, gemCaptures := range team.GemCaptures {
			if _, err := txExecInsertMatchTeamRoundStmt.Exec(mRes.ID, teamNumber, i, gemCaptures); err != nil {
				return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
			}
		}
	}

	if err := progressAchievementsViaMatch(tx, &m); err != nil {
		return CompletedMatch{}, fmt.Errorf("issue progressing achievements via match: %w", err)
	}

	return mRes, nil
}

func recordMatchHandler(c *gin.Context) {
	officialAny, hasOfficial := c.Get("official")
	if !hasOfficial {
		abortStatusError(c, http.StatusUnauthorized, ErrNoOfficialAuth)
		return
	}
	official := officialAny.(OfficialIdentity)

	var m Match
	var replayHeader *multipart.FileHeader

	switch c.GetUint32("version") {
	case 1:
		if c.ContentType() != "application/json" {
			c.AbortWithStatus(http.StatusUnsupportedMediaType)
			return
		}

		if err := c.ShouldBind(&m); err != nil {
			err = fmt.Errorf("%w: %w", ErrMalformedBody, err)
			abortStatusError(c, http.StatusBadRequest, err)
			return
		}
	case 2:
		if c.ContentType() != "multipart/form-data" {
			c.AbortWithStatus(http.StatusUnsupportedMediaType)
			return
		}

		type requestBody struct {
			Replay  *multipart.FileHeader `form:"replay" binding:"omitempty"`
			Payload string                `form:"payload" binding:"required"`
		}
		var body requestBody
		if err := c.ShouldBind(&body); err != nil {
			err = fmt.Errorf("%w: %w", ErrMalformedBody, err)
			abortStatusError(c, http.StatusBadRequest, err)
			return
		}

		if err := json.Unmarshal([]byte(body.Payload), &m); err != nil {
			err = fmt.Errorf("%w: %w", ErrMalformedBody, err)
			abortStatusError(c, http.StatusBadRequest, err)
			return
		}

		replayHeader = body.Replay
		// binding not marked as required, check it here (you should always be submitting a replay from here)
		if replayHeader == nil {
			err := fmt.Errorf("%w: must include replay", ErrMalformedBody)
			abortStatusError(c, http.StatusBadRequest, err)
			return
		}
	default:
		abortStatusError(c, http.StatusBadRequest, ErrVersion)
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

	mRes, err := recordMatchPayload(tx, m, official)
	if err != nil {
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	if replayHeader != nil {
		txExecUpdateMatchReplayStmt := tx.Stmt(execUpdateMatchReplayStmt)
		defer txExecUpdateMatchReplayStmt.Close()

		replay, err := replayHeader.Open()
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}
		defer replay.Close()

		var fileGameMode string
		switch mRes.GameMode {
		case "pvpa":
			fileGameMode = "TPVPA"
		case "ctg":
			fileGameMode = "CTG"
		case "arenas":
			fileGameMode = "Arenas"
		default:
			c.Error(fmt.Errorf("%w: using provided game mode for replay file name (%s)", ErrUnsupportedGameMode, mRes.GameMode))
			fileGameMode = mRes.GameMode
		}

		fileName := fmt.Sprintf("%s %d.reese", fileGameMode, mRes.ID)
		fileKey := fmt.Sprintf("replay/%s", fileName)

		// FIXME: nil context? BAD!!!
		_, err = s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
			Bucket: &s3ReplayBucket,
			Key:    aws.String(fileKey),
			Body:   replay,
			ACL:    types.ObjectCannedACLPublicRead,
		})

		// FIXME: put legal hold/lock

		if err != nil {
			err = fmt.Errorf("%w: %w", ErrReplaySave, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		replayUrl := url.URL{
			Scheme: "https",
			Host:   s3Endpoint,
			Path:   fileKey,
		}

		finalReplayUrl := replayUrl.String()

		if _, err := txExecUpdateMatchReplayStmt.Exec(finalReplayUrl, mRes.ID); err != nil {
			err = fmt.Errorf("%w: %w", ErrDatabase, err)
			c.Error(err)
			abortStatusError(c, http.StatusInternalServerError, err)
			return
		}

		mRes.ReplayUrl = &finalReplayUrl
	}

	if err := tx.Commit(); err != nil {
		err = fmt.Errorf("%w: %w", ErrDatabase, err)
		c.Error(err)
		abortStatusError(c, http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusOK, mRes)
}
