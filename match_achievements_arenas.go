package main

import "errors"

func progressArenasAchievementsForMatchPlayer(ctx *achievementProgressContext, m *Match, id uint64, p *Player) error {
	// Sandbox reports have boss 0 and must not grant match achievements.
	if m.Boss == nil || *m.Boss <= 0 {
		return nil
	}

	progress := func(name string, delta uint32) error {
		_, err := progressAchievement(ctx, id, AchievementRef{Name: name, GameMode: "arenas"}, &delta, false)
		if errors.Is(err, ErrAchievementAlreadyClaimed) {
			return nil
		}
		return err
	}

	if p.Kills > 0 {
		if err := progress("kill_1", uint32(p.Kills)); err != nil {
			return err
		}
	}

	if p.BossDamage != nil && *p.BossDamage > 0 {
		// This achievement's target is 100000. Cap a single report before the
		// shared uint32 progress addition so even a maximal report cannot wrap.
		if err := progress("boss_damage_100000", min(*p.BossDamage, 100000)); err != nil {
			return err
		}
	}

	if !p.Winner {
		return nil
	}

	for _, name := range []string{"win_1", "win_5", "win_25", "win_50"} {
		if err := progress(name, 1); err != nil {
			return err
		}
	}

	if p.Kills >= 3 {
		if err := progress("win_with_3_kills", 1); err != nil {
			return err
		}
	}

	const planteraNPCID = 262
	if *m.Boss == planteraNPCID {
		if err := progress("win_plantera", 1); err != nil {
			return err
		}
	}

	return nil
}
