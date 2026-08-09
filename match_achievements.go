package main

import (
	"database/sql"
	"errors"
)

func progressAchievementsViaMatch(tx *sql.Tx, m *Match) error {
	ctx := achievementProgressContext{}
	ctx.txQueryAchievementDefStmt = tx.Stmt(queryAchievementDefStmt)
	defer ctx.txQueryAchievementDefStmt.Close()
	ctx.txQueryAchievementStmt = tx.Stmt(queryAchievementStmt)
	defer ctx.txQueryAchievementStmt.Close()
	ctx.txExecAchievementProgressStmt = tx.Stmt(execAchievementProgressStmt)
	defer ctx.txExecAchievementProgressStmt.Close()

	type recordMostKills struct {
		id    []uint64
		count uint32
	}
	mostKills := recordMostKills{id: []uint64{}}

	for id, p := range m.Players {
		if err := progressAchievementsForMatchPlayer(&ctx, m, id, &p); err != nil {
			return err
		}

		if p.Kills >= 1 {
			kills := uint32(p.Kills)
			if kills == mostKills.count {
				mostKills.id = append(mostKills.id, id)
			} else if kills >= mostKills.count {
				mostKills.id = []uint64{id}
				mostKills.count = kills
			}
		}

		for _, id := range mostKills.id {
			var delta uint32 = 1

			if m.GameMode == "pvpa" {
				if _, err := progressAchievement(&ctx, id, AchievementRef{Name: "most_kills_in_match", GameMode: "pvpa"}, &delta, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
					return err
				}
			}
		}
	}

	return nil
}

func progressAchievementsForMatchPlayer(ctx *achievementProgressContext, m *Match, id uint64, p *Player) error {
	if p.Kills >= 1 {
		kills := uint32(p.Kills)

		if m.GameMode == "pvpa" {
			if _, err := progressAchievement(ctx, id, AchievementRef{Name: "kill_1", GameMode: "pvpa"}, &kills, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
				return err
			}

			if kills >= 50 {
				if _, err := progressAchievement(ctx, id, AchievementRef{Name: "kill_one_match_50", GameMode: "pvpa"}, &kills, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
					return err
				}
			}

			if kills >= 100 {
				if _, err := progressAchievement(ctx, id, AchievementRef{Name: "kill_one_match_100", GameMode: "pvpa"}, &kills, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
					return err
				}
			}
		}
	}

	if p.Winner {
		if m.GameMode == "pvpa" {
			var delta uint32 = 1
			if _, err := progressAchievement(ctx, id, AchievementRef{Name: "win_1", GameMode: "pvpa"}, &delta, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
				return err
			}

			if _, err := progressAchievement(ctx, id, AchievementRef{Name: "win_5", GameMode: "pvpa"}, &delta, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
				return err
			}

			if _, err := progressAchievement(ctx, id, AchievementRef{Name: "win_25", GameMode: "pvpa"}, &delta, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
				return err
			}

			if _, err := progressAchievement(ctx, id, AchievementRef{Name: "win_50", GameMode: "pvpa"}, &delta, false); err != nil && !errors.Is(err, ErrAchievementAlreadyClaimed) {
				return err
			}
		}
	}

	return nil
}
