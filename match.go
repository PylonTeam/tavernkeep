package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

	Reward uint32 `json:"reward" binding:"required"`

	Kills  int32 `json:"kills" binding:"required"`
	Deaths int32 `json:"deaths" binding:"required"`
	Winner bool  `json:"winner,omitempty"`

	Stats     map[string]uint32           `json:"stats" binding:"required"`
	ItemStats map[string]map[int32]uint32 `json:"itemStats" binding:"required"`
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

	Teams []*Team `json:"teams" binding:"required"`
}

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
	} else if m.GameMode != "ctg" && m.GameMode != "arenas" {
		return fmt.Errorf("%w: %w: %s", ErrMalformedMatch, ErrUnsupportedGameMode, m.GameMode)
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

	if err := rows.Scan(&m.ID, &start, &end, &m.GameMode, &recorded, &m.ReplayUrl, &m.OfficialIdentity); err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	m.Start = time.Unix(start, 0)
	m.End = time.Unix(end, 0)
	m.Recorded = time.Unix(recorded, 0)
	return nil
}

func queryMatch(txQueryMatchPlayerStmt *sql.Stmt, txQueryMatchPlayerStatsStmt *sql.Stmt, txQueryMatchPlayerItemStatsStmt *sql.Stmt, txQueryMatchAdventureTeam *sql.Stmt, txQueryMatchAdventureTeamBoss *sql.Stmt, m *CompletedMatch) error {
	rows, err := txQueryMatchPlayerStmt.Query(m.ID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}
	defer rows.Close()

	m.Players = map[uint64]Player{}
	for rows.Next() {
		var id uint64
		var player Player

		if err := rows.Scan(&id, &player.Name, &player.Team, &player.Reward, &player.Kills, &player.Deaths, &player.Winner); err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}

		player.Stats = map[string]uint32{}
		{
			rows, err := txQueryMatchPlayerStatsStmt.Query(m.ID, id)
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

			m.Players[id] = player
		}

		player.ItemStats = map[string]map[int32]uint32{}
		{
			rows, err := txQueryMatchPlayerItemStatsStmt.Query(m.ID, id)
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

			m.Players[id] = player
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	if m.GameMode == "pvpa" {
		rows, err = txQueryMatchAdventureTeam.Query(m.ID)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)
		}
		defer rows.Close()

		for rows.Next() {
			var teamNumber, points int

			if err := rows.Scan(&teamNumber, &points); err != nil {
				return fmt.Errorf("%w: %w", ErrDatabase, err)
			}

			// first iteration will have the largest index, because this is in descending order,
			// so use that to initialize our length.
			if m.Teams == nil {
				m.Teams = make([]*Team, teamNumber+1)
			}

			team := new(Team)
			team.Points = points
			team.Bosses = []int{}
			m.Teams[teamNumber] = team
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)

		}

		rows, err = txQueryMatchAdventureTeamBoss.Query(m.ID)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrDatabase, err)

		}

		defer rows.Close()
		for rows.Next() {
			var teamNumber, id int
			// TODO: Make use of first blood for bosses
			var firstBlood bool

			if err := rows.Scan(&teamNumber, &id, &firstBlood); err != nil {
				return fmt.Errorf("%w: %w", ErrDatabase, err)
			}

			m.Teams[teamNumber].Bosses = append(m.Teams[teamNumber].Bosses, id)
		}
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

	txQueryMatchStmt := tx.Stmt(queryMatchStmt)
	defer txQueryMatchStmt.Close()
	txQueryMatchPlayerStmt := tx.Stmt(queryMatchPlayerStmt)
	defer txQueryMatchPlayerStmt.Close()
	txQueryMatchPlayerStatsStmt := tx.Stmt(queryMatchPlayerStatsStmt)
	defer txQueryMatchPlayerStatsStmt.Close()
	txQueryMatchPlayerItemStatsStmt := tx.Stmt(queryMatchPlayerItemStatsStmt)
	defer txQueryMatchPlayerItemStatsStmt.Close()
	txQueryMatchAdventureTeam := tx.Stmt(queryMatchAdventureTeam)
	defer txQueryMatchAdventureTeam.Close()
	txQueryMatchAdventureTeamBoss := tx.Stmt(queryMatchAdventureTeamBoss)
	defer txQueryMatchAdventureTeamBoss.Close()

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

			if err = queryMatch(txQueryMatchPlayerStmt, txQueryMatchPlayerStatsStmt, txQueryMatchPlayerItemStatsStmt, txQueryMatchAdventureTeam, txQueryMatchAdventureTeamBoss, &m); err != nil {
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

			if err = queryMatch(txQueryMatchPlayerStmt, txQueryMatchPlayerStatsStmt, txQueryMatchPlayerItemStatsStmt, txQueryMatchAdventureTeam, txQueryMatchAdventureTeamBoss, &m); err != nil {
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

	var recorded int64
	if err := txExecInsertMatchStmt.QueryRow(m.Start.Unix(), m.End.Unix(), m.GameMode, official.Subject, official.Fingerprint).Scan(&mRes.ID, &recorded); err != nil {
		return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
	}

	mRes.Recorded = time.Unix(recorded, 0)

	type recordMostKills struct {
		id    []uint64
		count uint32
	}
	mostKills := recordMostKills{id: []uint64{}}

	for id, player := range m.Players {
		if _, err := txExecInsertMatchPlayerStmt.Exec(mRes.ID, id, player.Name, player.Team, player.Reward, player.Kills, player.Deaths, player.Winner); err != nil {
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

		if player.Kills >= 1 {
			kills := uint32(player.Kills)

			if _, err := progressAchievement(tx, id, AchievementRef{Name: "kill_1", GameMode: "pvpa"}, &kills, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
				return CompletedMatch{}, err
			}

			if kills >= 50 {
				if _, err := progressAchievement(tx, id, AchievementRef{Name: "kill_one_match_50", GameMode: "pvpa"}, &kills, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
					return CompletedMatch{}, err
				}
			}

			if kills >= 100 {
				if _, err := progressAchievement(tx, id, AchievementRef{Name: "kill_one_match_100", GameMode: "pvpa"}, &kills, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
					return CompletedMatch{}, err
				}
			}

			if kills == mostKills.count {
				mostKills.id = append(mostKills.id, id)
			} else if kills >= mostKills.count {
				mostKills.id = []uint64{id}
				mostKills.count = kills
			}
		}

		if player.Winner {
			var delta uint32 = 1
			if _, err := progressAchievement(tx, id, AchievementRef{Name: "win_1", GameMode: "pvpa"}, &delta, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
				return CompletedMatch{}, err
			}

			if _, err := progressAchievement(tx, id, AchievementRef{Name: "win_5", GameMode: "pvpa"}, &delta, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
				return CompletedMatch{}, err
			}

			if _, err := progressAchievement(tx, id, AchievementRef{Name: "win_25", GameMode: "pvpa"}, &delta, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
				return CompletedMatch{}, err
			}

			if _, err := progressAchievement(tx, id, AchievementRef{Name: "win_50", GameMode: "pvpa"}, &delta, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
				return CompletedMatch{}, err
			}
		}
	}

	for _, id := range mostKills.id {
		var delta uint32 = 1

		if _, err := progressAchievement(tx, id, AchievementRef{Name: "most_kills_in_match", GameMode: "pvpa"}, &delta, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
			return CompletedMatch{}, err
		}
	}

	for key, value := range m.Metrics {
		if _, err := txExecInsertMatchMetricStmt.Exec(mRes.ID, key, value); err != nil {
			return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
		}
	}

	if m.GameMode == "pvpa" {
		txExecInsertMatchAdventureTeam := tx.Stmt(execInsertMatchAdventureTeamStmt)
		defer txExecInsertMatchAdventureTeam.Close()
		txExecInsertMatchAdventureTeamBoss := tx.Stmt(execInsertMatchAdventureTeamBossStmt)
		defer txExecInsertMatchAdventureTeamBoss.Close()

		for i, team := range m.Teams[1:] {
			if team == nil {
				continue
			}

			teamNumber := i + 1

			if _, err := txExecInsertMatchAdventureTeam.Exec(mRes.ID, teamNumber, team.Points); err != nil {
				return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
			}

			for _, boss := range team.Bosses {
				// TODO: implement first blood bosses here!
				if _, err := txExecInsertMatchAdventureTeamBoss.Exec(mRes.ID, teamNumber, boss, false); err != nil {
					return CompletedMatch{}, fmt.Errorf("%w: %w", ErrDatabase, err)
				}
			}
		}
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
			err = fmt.Errorf("%w: %w: %w", ErrMalformedMatch, ErrMalformedBody, err)
			c.Error(err)
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
			c.Error(err)
			abortStatusError(c, http.StatusBadRequest, err)
			return
		}

		if err := json.Unmarshal([]byte(body.Payload), &m); err != nil {
			err = fmt.Errorf("%w: %w: %w", ErrMalformedMatch, ErrMalformedBody, err)
			c.Error(err)
			abortStatusError(c, http.StatusBadRequest, err)
			return
		}

		replayHeader = body.Replay
		// binding not marked as required, check it here (you should always be submitting a replay from here)
		if replayHeader == nil {
			err := fmt.Errorf("%w: must include replay", ErrMalformedBody)
			c.Error(err)
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
