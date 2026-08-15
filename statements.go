package main

import "database/sql"

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
    achieve_game_mode TEXT,
    achieve_name TEXT,
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
	official_identity_fingerprint TEXT NOT NULL,
	boss INTEGER,
	CONSTRAINT start_end_unity_check CHECK (start <= end)
);

CREATE TABLE IF NOT EXISTS match_player (
	match_id INTEGER NOT NULL,
	id TEXT NOT NULL,
	name TEXT NOT NULL,
	team INTEGER NOT NULL,
	reward INTEGER NOT NULL DEFAULT 0,
	kills INTEGER NOT NULL,
	deaths INTEGER NOT NULL,
	winner INTEGER NOT NULL,
	boss_damage INTEGER,
	CONSTRAINT pk PRIMARY KEY (match_id, id),
	CONSTRAINT reward_check CHECK (reward >= 0),
	CONSTRAINT winner_check CHECK (winner IN (FALSE, TRUE))
);

CREATE TABLE IF NOT EXISTS match_player_stat (
	match_id INTEGER NOT NULL,
	id TEXT NOT NULL,
	"key" TEXT NOT NULL,
	value INTEGER NOT NULL,
	CONSTRAINT pk PRIMARY KEY (match_id, id, "key"),
	CONSTRAINT value_check CHECK (value > 0)
);

CREATE TABLE IF NOT EXISTS match_player_item_stat (
	match_id INTEGER NOT NULL,
	id TEXT NOT NULL,
	"key" TEXT NOT NULL,
	item INTEGER NOT NULL,
	value INTEGER NOT NULL,
	CONSTRAINT pk PRIMARY KEY (match_id, id, "key", item),
	CONSTRAINT value_check CHECK (value > 0)
);

CREATE TABLE IF NOT EXISTS match_team (
	match_id INTEGER NOT NULL,
	team INTEGER NOT NULL,
	points INTEGER,
	boss_damage INTEGER,
	CONSTRAINT pk PRIMARY KEY (match_id, team),
	CONSTRAINT team_check CHECK (team > 0),
	CONSTRAINT points_check CHECK (points >= 0)
);

CREATE TABLE IF NOT EXISTS match_player_round (
	match_id INTEGER NOT NULL,
	id TEXT NOT NULL,
	"index" INTEGER NOT NULL,
	gem_captures INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS match_team_round (
	match_id INTEGER NOT NULL,
	team INTEGER NOT NULL,
	"index" INTEGER NOT NULL,
	gem_captures INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS match_team_boss (
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

CREATE INDEX IF NOT EXISTS idx_match_game_mode ON match(game_mode);
CREATE INDEX IF NOT EXISTS idx_match_player_player ON match_player(id, match_id);
CREATE INDEX IF NOT EXISTS idx_match_player_stat_player ON match_player_stat(id, match_id, key);
CREATE INDEX IF NOT EXISTS idx_match_player_item_stat_player ON match_player_item_stat(id, match_id, key, item);
`

var (
	queryGemsStmt               *sql.Stmt
	queryProductsStmt           *sql.Stmt
	queryProductStmt            *sql.Stmt
	queryEquipmentStmt          *sql.Stmt
	queryInventoryStmt          *sql.Stmt
	queryInventoryItemCountStmt *sql.Stmt
	queryAccoladesStmt          *sql.Stmt
	queryAccoladeStmt           *sql.Stmt

	execEquipStmt      *sql.Stmt
	execReduceGemsStmt *sql.Stmt
	execInsertItemStmt *sql.Stmt

	queryMatchStmt                *sql.Stmt
	queryMatchesStmt              *sql.Stmt
	queryMatchPlayerStmt          *sql.Stmt
	queryMatchPlayerStatsStmt     *sql.Stmt
	queryMatchPlayerItemStatsStmt *sql.Stmt
	queryMatchTeamStmt            *sql.Stmt
	queryMatchTeamBossStmt        *sql.Stmt
	queryMatchPlayerRoundStmt     *sql.Stmt
	queryMatchTeamRoundStmt       *sql.Stmt

	execInsertMatchStmt               *sql.Stmt
	execUpdateMatchReplayStmt         *sql.Stmt
	execInsertMatchPlayerStmt         *sql.Stmt
	execInsertMatchPlayerStatStmt     *sql.Stmt
	execInsertMatchPlayerItemStatStmt *sql.Stmt
	execInsertMatchTeamStmt           *sql.Stmt
	execInsertMatchTeamBossStmt       *sql.Stmt
	execInsertMatchMetricStmt         *sql.Stmt
	execInsertMatchPlayerRoundStmt    *sql.Stmt
	execInsertMatchTeamRoundStmt      *sql.Stmt

	execIncreaseGemsStmt *sql.Stmt

	queryAchievementDefsStmt    *sql.Stmt
	queryAchievementDefStmt     *sql.Stmt
	queryAchievementClaimedStmt *sql.Stmt

	queryAchievementsStmt *sql.Stmt
	queryAchievementStmt  *sql.Stmt

	execAchievementProgressStmt *sql.Stmt
	execAchievementClaimStmt    *sql.Stmt

	queryMatchAggregatePlayerStmt           *sql.Stmt
	queryMatchAggregatePlayerStatStmt       *sql.Stmt
	queryMatchAggregatePlayerItemStatStmt   *sql.Stmt
	queryMatchAggregatePlayerTeamPointsStmt *sql.Stmt
)

func prepareStatements() error {
	var err error
	if queryGemsStmt, err = db.Prepare("SELECT count FROM gem WHERE id = ?"); err != nil {
		return err
	}

	if queryProductsStmt, err = db.Prepare("SELECT prototype, name, price, accolade, achieve_game_mode, achieve_name FROM product WHERE (prototype = ?1 OR ?1 IS NULL) AND available = TRUE"); err != nil {
		return err
	}

	if queryProductStmt, err = db.Prepare("SELECT price, accolade, achieve_game_mode, achieve_name FROM product WHERE available = TRUE AND prototype = ? AND name = ?"); err != nil {
		return err
	}

	if queryEquipmentStmt, err = db.Prepare("SELECT prototype, name FROM item WHERE equipped = TRUE AND owner = ?"); err != nil {
		return err
	}

	if queryInventoryStmt, err = db.Prepare("SELECT prototype, name, issued, purchase_price FROM item WHERE owner = ?"); err != nil {
		return err
	}

	if queryInventoryItemCountStmt, err = db.Prepare("SELECT COUNT() FROM item WHERE prototype = ? AND name = ? AND owner = ?"); err != nil {
		return err
	}

	if queryAccoladesStmt, err = db.Prepare("SELECT name, owner FROM accolade"); err != nil {
		return err
	}

	if queryAccoladeStmt, err = db.Prepare("SELECT COUNT() FROM accolade WHERE name = ? AND owner = ?"); err != nil {
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

	if execInsertItemStmt, err = db.Prepare("INSERT INTO item (prototype, name, owner, purchase_price, accolade) VALUES (?, ?, ?, ?, ?) RETURNING issued"); err != nil {
		return err
	}

	if queryMatchStmt, err = db.Prepare("SELECT id, start, end, game_mode, recorded, replay_url, official_identity_subject, boss FROM match WHERE (id = ?1 OR ?1 IS NULL) AND (game_mode = ?2 OR ?2 IS NULL)"); err != nil {
		return err
	}

	if queryMatchesStmt, err = db.Prepare("SELECT match_id FROM match_player WHERE id = ?"); err != nil {
		return err
	}

	if queryMatchPlayerStmt, err = db.Prepare("SELECT id, name, team, reward, kills, deaths, winner, boss_damage FROM match_player WHERE match_id = ?"); err != nil {
		return err
	}

	if queryMatchPlayerStatsStmt, err = db.Prepare("SELECT key, value FROM match_player_stat WHERE match_id = ? AND id = ?"); err != nil {
		return err
	}

	if queryMatchPlayerItemStatsStmt, err = db.Prepare("SELECT key, item, value FROM match_player_item_stat WHERE match_id = ? AND id = ?"); err != nil {
		return err
	}

	if queryMatchTeamStmt, err = db.Prepare("SELECT team, points, boss_damage FROM match_team WHERE match_id = ? ORDER BY team DESC"); err != nil {
		return err
	}

	if queryMatchTeamBossStmt, err = db.Prepare("SELECT team, id, first_blood FROM match_team_boss WHERE match_id = ? ORDER BY team DESC"); err != nil {
		return err
	}

	if queryMatchPlayerRoundStmt, err = db.Prepare("SELECT \"index\", gem_captures FROM match_player_round WHERE match_id = ? AND id = ? ORDER BY \"index\" DESC"); err != nil {
		return err
	}

	if queryMatchTeamRoundStmt, err = db.Prepare("SELECT team, \"index\", gem_captures FROM match_team_round WHERE match_id = ? ORDER BY team DESC, \"index\" DESC"); err != nil {
		return err
	}

	if execInsertMatchStmt, err = db.Prepare("INSERT INTO match (start, end, game_mode, official_identity_subject, official_identity_fingerprint, boss) VALUES (?, ?, ?, ?, ?, ?) RETURNING id, recorded"); err != nil {
		return err
	}

	if execUpdateMatchReplayStmt, err = db.Prepare("UPDATE match SET replay_url = ? WHERE id = ? AND replay_url IS NULL"); err != nil {
		return err
	}

	if execInsertMatchPlayerStmt, err = db.Prepare("INSERT INTO match_player (match_id, id, name, team, reward, kills, deaths, winner, boss_damage) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)"); err != nil {
		return err
	}

	if execInsertMatchPlayerStatStmt, err = db.Prepare("INSERT INTO match_player_stat (match_id, id, key, value) VALUES (?, ?, ?, ?)"); err != nil {
		return err
	}

	if execInsertMatchPlayerItemStatStmt, err = db.Prepare("INSERT INTO match_player_item_stat (match_id, id, key, item, value) VALUES (?, ?, ?, ?, ?)"); err != nil {
		return err
	}

	if execInsertMatchTeamStmt, err = db.Prepare("INSERT INTO match_team (match_id, team, points, boss_damage) VALUES (?, ?, ?, ?)"); err != nil {
		return err
	}

	if execInsertMatchTeamBossStmt, err = db.Prepare("INSERT INTO match_team_boss (match_id, team, id, first_blood) VALUES (?, ?, ?, ?)"); err != nil {
		return err
	}

	if execInsertMatchMetricStmt, err = db.Prepare("INSERT INTO match_metric (match_id, key, value) VALUES (?, ?, ?)"); err != nil {
		return err
	}

	if execInsertMatchPlayerRoundStmt, err = db.Prepare("INSERT INTO match_player_round (match_id, id, \"index\", gem_captures) VALUES (?, ?, ?, ?)"); err != nil {
		return err
	}

	if execInsertMatchTeamRoundStmt, err = db.Prepare("INSERT INTO match_team_round (match_id, team, \"index\", gem_captures) VALUES (?, ?, ?, ?)"); err != nil {
		return err
	}

	if execIncreaseGemsStmt, err = db.Prepare("INSERT INTO gem (id, count) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET count=count + excluded.count"); err != nil {
		return err
	}

	if queryAchievementDefsStmt, err = db.Prepare("SELECT name, target, reward FROM achievement_def WHERE game_mode = ?"); err != nil {
		return err
	}

	if queryAchievementDefStmt, err = db.Prepare("SELECT target, reward, no_ext_mut FROM achievement_def WHERE name = ? AND game_mode = ?"); err != nil {
		return err
	}

	if queryAchievementClaimedStmt, err = db.Prepare("SELECT COUNT() FROM achievement WHERE name = ? AND game_mode = ? AND id = ? AND claimed IS NOT NULL and claimed_at IS NOT NULL"); err != nil {
		return err
	}

	if queryAchievementsStmt, err = db.Prepare("SELECT name, progress, claimed, claimed_at FROM achievement WHERE game_mode = ? AND id = ?"); err != nil {
		return err
	}

	if queryAchievementStmt, err = db.Prepare("SELECT progress, claimed, claimed_at FROM achievement WHERE name = ? AND game_mode = ? AND id = ?"); err != nil {
		return err
	}

	if execAchievementProgressStmt, err = db.Prepare("INSERT INTO achievement (name, game_mode, id, progress) VALUES (?, ?, ?, ?) ON CONFLICT(name, game_mode, id) DO UPDATE SET progress=? WHERE claimed IS NULL AND claimed_at IS NULL"); err != nil {
		return err
	}

	if execAchievementClaimStmt, err = db.Prepare("UPDATE achievement SET claimed=?, claimed_at=unixepoch() WHERE name=? AND game_mode=? AND id=? AND progress=? AND claimed_at IS NULL AND claimed_at IS NULL RETURNING claimed_at, progress"); err != nil {
		return err
	}

	if queryMatchAggregatePlayerStmt, err = db.Prepare("SELECT match_player.id, name, SUM(kills), SUM(deaths), SUM(winner), COUNT() FROM match_player JOIN match ON match_player.match_id = match.id WHERE (match.game_mode = ?1 OR ?1 IS NULL) GROUP BY match_player.id ORDER BY SUM(winner) DESC, SUM(kills) DESC, MAX(match.start)"); err != nil {
		return err
	}

	if queryMatchAggregatePlayerStatStmt, err = db.Prepare(" SELECT key, SUM(value) FROM match_player_stat AS mps JOIN match AS m ON mps.match_id = m.id WHERE (m.game_mode = ?1 OR ?1 IS NULL) AND mps.id = ? GROUP BY mps.id, key"); err != nil {
		return err
	}

	if queryMatchAggregatePlayerItemStatStmt, err = db.Prepare("SELECT key, item, SUM(value) FROM match_player_item_stat AS mpis JOIN match AS m ON mpis.match_id = m.id WHERE (m.game_mode = ?1 OR ?1 IS NULL) AND mpis.id = ? GROUP BY mpis.id, key, item"); err != nil {
		return err
	}

	if queryMatchAggregatePlayerTeamPointsStmt, err = db.Prepare("SELECT COALESCE(SUM(mt.points), 0) FROM match_player mp LEFT JOIN match_team mt ON mp.match_id = mt.match_id AND mp.team = mt.team WHERE mp.id = ?"); err != nil {
		return err
	}

	return nil
}

func closeStatements() {
	queryMatchAggregatePlayerTeamPointsStmt.Close()
	queryMatchAggregatePlayerItemStatStmt.Close()
	queryMatchAggregatePlayerStatStmt.Close()
	queryMatchAggregatePlayerStmt.Close()
	execAchievementClaimStmt.Close()
	execAchievementProgressStmt.Close()
	queryAchievementStmt.Close()
	queryAchievementsStmt.Close()
	queryAchievementClaimedStmt.Close()
	queryAchievementDefStmt.Close()
	queryAchievementDefsStmt.Close()
	execIncreaseGemsStmt.Close()
	execInsertMatchTeamRoundStmt.Close()
	execInsertMatchPlayerRoundStmt.Close()
	execInsertMatchMetricStmt.Close()
	execInsertMatchTeamBossStmt.Close()
	execInsertMatchTeamStmt.Close()
	execInsertMatchPlayerItemStatStmt.Close()
	execInsertMatchPlayerStatStmt.Close()
	execInsertMatchPlayerStmt.Close()
	execUpdateMatchReplayStmt.Close()
	execInsertMatchStmt.Close()

	queryMatchTeamRoundStmt.Close()
	queryMatchPlayerRoundStmt.Close()
	queryMatchTeamBossStmt.Close()
	queryMatchTeamStmt.Close()
	queryMatchPlayerItemStatsStmt.Close()
	queryMatchPlayerStatsStmt.Close()
	queryMatchPlayerStmt.Close()
	queryMatchesStmt.Close()
	queryMatchStmt.Close()

	execInsertItemStmt.Close()
	execReduceGemsStmt.Close()
	execEquipStmt.Close()
	queryAccoladeStmt.Close()
	queryAccoladesStmt.Close()
	queryInventoryItemCountStmt.Close()
	queryInventoryStmt.Close()
	queryEquipmentStmt.Close()
	queryProductStmt.Close()
	queryProductsStmt.Close()
	queryGemsStmt.Close()
}
