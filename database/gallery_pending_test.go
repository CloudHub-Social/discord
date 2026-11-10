package database

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"go.mau.fi/util/dbutil"
)

func TestGalleryReservationAndReconciliationLookup(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer raw.Close()
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE gallery_pending (mxid TEXT PRIMARY KEY, dc_chan_id TEXT NOT NULL, dc_chan_receiver TEXT NOT NULL, dc_thread_id TEXT NOT NULL, dc_sender TEXT NOT NULL, sender_mxid TEXT NOT NULL, nonce TEXT NOT NULL UNIQUE)`)
	require.NoError(t, err)
	mq := &MessageQuery{db: &Database{Database: db}}
	pending := GalleryPending{MXID: "$event", Channel: PortalKey{ChannelID: "123", Receiver: "receiver"}, ThreadID: "thread", SenderID: "sender", SenderMXID: "@test:example.org", Nonce: "42"}
	ok, err := mq.ReserveGallery(pending)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = mq.ReserveGallery(pending)
	require.NoError(t, err)
	require.False(t, ok)
	exists, err := mq.HasPendingGallery(pending.MXID)
	require.NoError(t, err)
	require.True(t, exists)
	got, err := mq.PendingGalleryByNonce(pending.Channel, "42", "other")
	require.NoError(t, err)
	require.Nil(t, got)
	got, err = mq.PendingGalleryByNonce(pending.Channel, "42", "sender")
	require.NoError(t, err)
	require.Equal(t, &pending, got)
	require.NoError(t, mq.ClearPendingGallery(pending.MXID))
	ok, err = mq.ReserveGallery(pending)
	require.NoError(t, err)
	require.True(t, ok)
}
