package main

import (
	"database/sql"
	"errors"
	"maps"
	"path/filepath"
	"testing"
)

const arenasTestPlayer uint64 = 76561198152929668

func arenasTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "arenas.db"))
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	oldDB := db
	db = conn
	t.Cleanup(func() {
		conn.Close()
		db = oldDB
	})
	arenasTestExec(t, conn, dbSchema)
	if err := prepareStatements(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeStatements)
	return conn
}

func arenasTestExec(t *testing.T, conn *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func seedArenasTestDefinitions(t *testing.T, conn *sql.DB) {
	t.Helper()
	arenasTestExec(t, conn, `INSERT INTO achievement_def(name,game_mode,target,reward,no_ext_mut) VALUES
		('win_1','arenas',1,10,1),
		('win_5','arenas',5,50,1),
		('win_25','arenas',25,200,1),
		('win_50','arenas',50,500,1),
		('kill_1','arenas',1,20,1),
		('win_with_3_kills','arenas',1,50,1),
		('boss_damage_100000','arenas',100000,100,1),
		('win_plantera','arenas',1,50,1)`)
}

func arenasTestMatch(t *testing.T, conn *sql.DB, m Match) error {
	t.Helper()
	tx, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := progressAchievementsViaMatch(tx, &m); err != nil {
		return err
	}
	return tx.Commit()
}

func arenasTestProgress(t *testing.T, conn *sql.DB) map[string]uint32 {
	t.Helper()
	rows, err := conn.Query("SELECT game_mode,name,progress FROM achievement WHERE id=?", arenasTestPlayer)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[string]uint32{}
	for rows.Next() {
		var mode, name string
		var progress uint32
		if err := rows.Scan(&mode, &name, &progress); err != nil {
			t.Fatal(err)
		}
		result[mode+"/"+name] = progress
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func arenasTestPointer[T any](value T) *T { return &value }

func TestArenasMatchAchievementRules(t *testing.T) {
	cases := []struct {
		name   string
		boss   int16
		player Player
		want   map[string]uint32
	}{
		{
			name: "loser still earns kills and boss damage",
			boss: 262,
			player: Player{Kills: 5, BossDamage: arenasTestPointer(uint32(2500)),
				Stats: map[string]uint32{"boss_damage_dealt": 2500}},
			want: map[string]uint32{"arenas/kill_1": 1, "arenas/boss_damage_100000": 2500},
		},
		{
			name:   "Plantera winner with fewer than three kills",
			boss:   262,
			player: Player{Winner: true, Kills: 2, BossDamage: arenasTestPointer(uint32(25))},
			want: map[string]uint32{"arenas/win_1": 1, "arenas/win_5": 1, "arenas/win_25": 1, "arenas/win_50": 1,
				"arenas/kill_1": 1, "arenas/boss_damage_100000": 25, "arenas/win_plantera": 1},
		},
		{
			name:   "three kill winner without boss damage",
			boss:   4,
			player: Player{Winner: true, Kills: 3},
			want: map[string]uint32{"arenas/win_1": 1, "arenas/win_5": 1, "arenas/win_25": 1, "arenas/win_50": 1,
				"arenas/kill_1": 1, "arenas/win_with_3_kills": 1},
		},
		{
			name:   "stats cannot replace the damage field",
			boss:   4,
			player: Player{Winner: true, Stats: map[string]uint32{"boss_damage_dealt": 500}},
			want:   map[string]uint32{"arenas/win_1": 1, "arenas/win_5": 1, "arenas/win_25": 1, "arenas/win_50": 1},
		},
		{
			name:   "zero damage creates no damage progress",
			boss:   4,
			player: Player{Winner: true, BossDamage: arenasTestPointer(uint32(0))},
			want:   map[string]uint32{"arenas/win_1": 1, "arenas/win_5": 1, "arenas/win_25": 1, "arenas/win_50": 1},
		},
		{
			name:   "nonpositive kills and damage do not create zero progress",
			boss:   4,
			player: Player{Kills: -1, BossDamage: arenasTestPointer(uint32(0))},
			want:   map[string]uint32{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := arenasTestDB(t)
			seedArenasTestDefinitions(t, conn)
			m := Match{GameMode: "arenas", Boss: &tc.boss, Players: map[uint64]Player{arenasTestPlayer: tc.player}}
			if err := arenasTestMatch(t, conn, m); err != nil {
				t.Fatal(err)
			}
			if got := arenasTestProgress(t, conn); !maps.Equal(got, tc.want) {
				t.Fatalf("progress=%v, want=%v", got, tc.want)
			}
		})
	}
}

func TestArenasMatchAchievementsRequireBoss(t *testing.T) {
	for name, boss := range map[string]*int16{
		"absent":   nil,
		"sandbox":  arenasTestPointer(int16(0)),
		"negative": arenasTestPointer(int16(-1)),
	} {
		t.Run(name, func(t *testing.T) {
			conn := arenasTestDB(t)
			// No definitions: excluded reports must not even try progression.
			m := Match{GameMode: "arenas", Boss: boss, Players: map[uint64]Player{
				arenasTestPlayer: {Winner: true, Kills: 5, BossDamage: arenasTestPointer(uint32(100000))},
			}}
			if err := arenasTestMatch(t, conn, m); err != nil {
				t.Fatal(err)
			}
			if got := arenasTestProgress(t, conn); len(got) != 0 {
				t.Fatalf("excluded boss progressed achievements: %v", got)
			}
		})
	}
}

func TestArenasMatchAchievementModeIsolation(t *testing.T) {
	conn := arenasTestDB(t)
	seedArenasTestDefinitions(t, conn)
	arenasTestExec(t, conn, `INSERT INTO achievement_def(name,game_mode,target,reward,no_ext_mut) VALUES
		('win_1','pvpa',1,10,1),('win_5','pvpa',5,10,1),
		('win_25','pvpa',25,10,1),('win_50','pvpa',50,10,1),
		('kill_1','pvpa',1,10,1),('kill_one_match_50','pvpa',50,10,1),
		('kill_one_match_100','pvpa',100,10,1),('most_kills_in_match','pvpa',1,10,1)`)
	player := Player{Winner: true, Kills: 100, BossDamage: arenasTestPointer(uint32(500))}
	m := Match{GameMode: "arenas", Boss: arenasTestPointer(int16(262)), Players: map[uint64]Player{arenasTestPlayer: player}}
	if err := arenasTestMatch(t, conn, m); err != nil {
		t.Fatal(err)
	}
	want := map[string]uint32{"arenas/win_1": 1, "arenas/win_5": 1, "arenas/win_25": 1, "arenas/win_50": 1, "arenas/kill_1": 1,
		"arenas/win_with_3_kills": 1, "arenas/boss_damage_100000": 500, "arenas/win_plantera": 1}
	if got := arenasTestProgress(t, conn); !maps.Equal(got, want) {
		t.Fatalf("Arenas report touched another mode: %v", got)
	}
	m.GameMode = "pvpa"
	m.Boss = nil
	if err := arenasTestMatch(t, conn, m); err != nil {
		t.Fatal(err)
	}
	for name, progress := range map[string]uint32{"win_1": 1, "win_5": 1, "win_25": 1, "win_50": 1,
		"kill_1": 1, "kill_one_match_50": 50, "kill_one_match_100": 100, "most_kills_in_match": 1} {
		want["pvpa/"+name] = progress
	}
	if got := arenasTestProgress(t, conn); !maps.Equal(got, want) {
		t.Fatalf("PvPA progression changed: got=%v, want=%v", got, want)
	}
	m.GameMode = "ctg"
	if err := arenasTestMatch(t, conn, m); err != nil {
		t.Fatal(err)
	}
	if got := arenasTestProgress(t, conn); !maps.Equal(got, want) {
		t.Fatalf("CTG report touched achievements: %v", got)
	}
}

func TestArenasMatchAchievementCumulativeClamping(t *testing.T) {
	conn := arenasTestDB(t)
	seedArenasTestDefinitions(t, conn)
	for matchNumber := uint32(1); matchNumber <= 52; matchNumber++ {
		var damage uint32
		wantDamage := uint32(100000)
		switch matchNumber {
		case 1:
			damage, wantDamage = 40000, 40000
		case 2:
			damage, wantDamage = 50000, 90000
		case 3:
			damage = ^uint32(0)
		}
		m := Match{GameMode: "arenas", Boss: arenasTestPointer(int16(4)), Players: map[uint64]Player{
			arenasTestPlayer: {Winner: true, Kills: 3, BossDamage: &damage},
		}}
		if err := arenasTestMatch(t, conn, m); err != nil {
			t.Fatal(err)
		}
		got := arenasTestProgress(t, conn)
		for name, target := range map[string]uint32{"win_1": 1, "win_5": 5, "win_25": 25, "win_50": 50} {
			if progress := got["arenas/"+name]; progress != min(matchNumber, target) {
				t.Fatalf("match %d: %s progress=%d, want=%d", matchNumber, name, progress, min(matchNumber, target))
			}
		}
		if got["arenas/boss_damage_100000"] != wantDamage || got["arenas/win_with_3_kills"] != 1 {
			t.Fatalf("match %d: incorrect cumulative/clamped progress: %v", matchNumber, got)
		}
	}
}

func TestArenasMatchAchievementPreservesClaims(t *testing.T) {
	conn := arenasTestDB(t)
	seedArenasTestDefinitions(t, conn)
	arenasTestExec(t, conn, `INSERT INTO achievement(name,game_mode,id,progress,claimed,claimed_at) VALUES
		('win_1','arenas',?,1,10,100),
		('win_5','arenas',?,2,NULL,100),
		('win_25','arenas',?,25,200,100),
		('win_50','arenas',?,40,0,NULL),
		('kill_1','arenas',?,1,0,NULL),
		('boss_damage_100000','arenas',?,30000,25,100)`,
		arenasTestPlayer, arenasTestPlayer, arenasTestPlayer, arenasTestPlayer, arenasTestPlayer, arenasTestPlayer)
	type claimState struct {
		progress uint32
		reward   sql.NullInt64
		at       sql.NullInt64
	}
	readClaim := func(name string) claimState {
		t.Helper()
		var result claimState
		if err := conn.QueryRow("SELECT progress,claimed,claimed_at FROM achievement WHERE game_mode='arenas' AND id=? AND name=?",
			arenasTestPlayer, name).Scan(&result.progress, &result.reward, &result.at); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := map[string]claimState{}
	for _, name := range []string{"win_1", "win_5", "win_25", "win_50", "kill_1", "boss_damage_100000"} {
		before[name] = readClaim(name)
	}
	m := Match{GameMode: "arenas", Boss: arenasTestPointer(int16(262)), Players: map[uint64]Player{
		arenasTestPlayer: {Winner: true, Kills: 5, BossDamage: arenasTestPointer(uint32(100000))},
	}}
	if err := arenasTestMatch(t, conn, m); err != nil {
		t.Fatal(err)
	}
	for name, original := range before {
		if got := readClaim(name); got != original {
			t.Fatalf("claim %s changed: got=%+v, want=%+v", name, got, original)
		}
	}
	got := arenasTestProgress(t, conn)
	if got["arenas/win_with_3_kills"] != 1 || got["arenas/win_plantera"] != 1 {
		t.Fatalf("claimed achievements blocked other progression: %v", got)
	}
}

func TestArenasMatchAchievementErrorsRollback(t *testing.T) {
	t.Run("missing definition", func(t *testing.T) {
		conn := arenasTestDB(t)
		seedArenasTestDefinitions(t, conn)
		arenasTestExec(t, conn, "DELETE FROM achievement_def WHERE name='win_plantera' AND game_mode='arenas'")
		m := Match{GameMode: "arenas", Boss: arenasTestPointer(int16(262)), Players: map[uint64]Player{
			arenasTestPlayer: {Winner: true, Kills: 5, BossDamage: arenasTestPointer(uint32(25))},
		}}
		if err := arenasTestMatch(t, conn, m); !errors.Is(err, ErrAchievementDoesNotExist) {
			t.Fatalf("error=%v, want missing definition", err)
		}
		if got := arenasTestProgress(t, conn); len(got) != 0 {
			t.Fatalf("failed match left partial progress: %v", got)
		}
	})
	t.Run("database error", func(t *testing.T) {
		conn := arenasTestDB(t)
		seedArenasTestDefinitions(t, conn)
		arenasTestExec(t, conn, "DROP TABLE achievement")
		m := Match{GameMode: "arenas", Boss: arenasTestPointer(int16(4)), Players: map[uint64]Player{
			arenasTestPlayer: {Kills: 1},
		}}
		if err := arenasTestMatch(t, conn, m); !errors.Is(err, ErrDatabase) {
			t.Fatalf("error=%v, want database error", err)
		}
	})
}
