package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-discord/database"
)

func newGuildSubscriptionTestDB(t *testing.T) *database.Database {
	t.Helper()
	sqlDB, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	baseDB, err := dbutil.NewWithDB(sqlDB, "sqlite3")
	require.NoError(t, err)
	db := database.New(baseDB, maulogger.Create())
	require.NoError(t, db.Upgrade())
	for _, guildID := range []string{"100", "200"} {
		guild := db.Guild.New()
		guild.ID = guildID
		guild.Insert()
	}
	return db
}

func addGuildSubscriptionTestPortal(db *database.Database, channelID, guildID string, channelType discordgo.ChannelType, bridged bool) {
	p := db.Portal.New()
	p.Key = database.NewPortalKey(channelID, "")
	p.GuildID = guildID
	p.Type = channelType
	if bridged {
		p.MXID = id.RoomID("!" + channelID + ":example.com")
	}
	p.Insert()
}

func TestGuildMemberListSelection(t *testing.T) {
	db := newGuildSubscriptionTestDB(t)
	u := newTestUser()
	u.bridge = &DiscordBridge{DB: db}
	// An organizational space, a voice room, and a thread are not text-channel
	// member sidebars. Put them first so truncating before filtering fails.
	for i, kind := range []discordgo.ChannelType{
		discordgo.ChannelTypeGuildCategory, discordgo.ChannelTypeGuildVoice,
		discordgo.ChannelTypeGuildPublicThread,
	} {
		addGuildSubscriptionTestPortal(db, fmt.Sprint(i+1), "100", kind, true)
	}
	addGuildSubscriptionTestPortal(db, "10", "100", discordgo.ChannelTypeGuildText, true)
	addGuildSubscriptionTestPortal(db, "11", "100", discordgo.ChannelTypeGuildNews, true)
	addGuildSubscriptionTestPortal(db, "12", "100", discordgo.ChannelTypeGuildText, false)
	addGuildSubscriptionTestPortal(db, "13", "200", discordgo.ChannelTypeGuildText, true)
	require.Equal(t, []string{"10", "11"}, u.guildChannelIDs("100"))
	// Insert backwards to ensure the bounded sample remains deterministic and
	// the reconciler compares the selected set, not every portal in the guild.
	for i := 29; i >= 20; i-- {
		addGuildSubscriptionTestPortal(db, fmt.Sprint(i), "100", discordgo.ChannelTypeGuildText, true)
	}
	require.Equal(t, []string{"10", "11", "20", "21", "22"}, u.guildChannelIDs("100"))
	require.Empty(t, u.guildChannelIDs("300"))
}

func TestGuildMemberListSubscriptionWire(t *testing.T) {
	for _, count := range []int{0, 2, 5, 19} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			br, user, capture := newPresenceTestBridge(t, true, false, false)
			br.DB = newGuildSubscriptionTestDB(t)
			for i := count - 1; i >= 0; i-- {
				addGuildSubscriptionTestPortal(br.DB, fmt.Sprintf("10000000000000000%02d", i), "100", discordgo.ChannelTypeGuildText, true)
			}
			require.True(t, user.sendGuildPresenceSubscribe(user.Session, "100"))
			frames, _ := capture.snapshot(t, user.Session)
			require.Len(t, frames, 1)
			require.Equal(t, 14, frames[0].Op)
			var data discordgo.GuildSubscribeData
			require.NoError(t, json.Unmarshal(frames[0].D, &data))
			require.Equal(t, "100", data.GuildID)
			require.True(t, data.Typing, "guild message events must remain subscribed")
			require.True(t, data.Activities)
			require.True(t, data.Threads)
			require.Len(t, data.Channels, min(count, 5))
			for i := 0; i < min(count, 5); i++ {
				require.Equal(t, [][]int{{0, 99}}, data.Channels[fmt.Sprintf("10000000000000000%02d", i)])
			}
			// Release exactly the bounded set we sent, using the existing path.
			sent, abort := user.sendGuildRelease(user.Session, "100", user.guildChannelIDs("100"))
			require.True(t, sent)
			require.False(t, abort)
			frames, _ = capture.snapshot(t, user.Session)
			require.Len(t, frames, 1)
			require.NoError(t, json.Unmarshal(frames[0].D, &data))
			require.Len(t, data.Channels, min(count, 5))
			for _, ranges := range data.Channels {
				require.Empty(t, ranges)
			}
		})
	}
}
