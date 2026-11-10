package upgrades

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"go.mau.fi/util/dbutil"
)

func TestGalleryMigrationFrom24AndFresh(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "version24"}[old], func(t *testing.T) {
			raw, err := sql.Open("sqlite3", ":memory:")
			require.NoError(t, err)
			defer raw.Close()
			db, err := dbutil.NewWithDB(raw, "sqlite3")
			require.NoError(t, err)
			db.UpgradeTable = Table
			if old {
				_, err = db.Exec(`CREATE TABLE version (version INTEGER, compat INTEGER); INSERT INTO version VALUES (24,19); CREATE TABLE message (id TEXT PRIMARY KEY); INSERT INTO message VALUES ('existing');`)
				require.NoError(t, err)
			}
			require.NoError(t, db.Upgrade())
			var version int
			require.NoError(t, db.QueryRow("SELECT version FROM version").Scan(&version))
			require.Equal(t, 25, version)
			_, err = db.Exec(`INSERT INTO gallery_pending VALUES ('$e','c','','','s','@s:x','42')`)
			require.NoError(t, err)
			if old {
				var count int
				require.NoError(t, db.QueryRow("SELECT count(*) FROM message").Scan(&count))
				require.Equal(t, 1, count)
			}
		})
	}
}
