package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-discord/config"
	"go.mau.fi/mautrix-discord/database"
)

const recoveryTestMessageID = "1000000000000000001"

func newRecoveryTest(t *testing.T) (*DiscordBridge, *User, *Portal, *int) {
	t.Helper()
	db := newGuildSubscriptionTestDB(t)
	dbPortal := db.Portal.New()
	dbPortal.Key = database.NewPortalKey("1000000000000000002", "1")
	dbPortal.Type = discordgo.ChannelTypeGroupDM
	dbPortal.MXID = "!recovery:example.com"
	dbPortal.Encrypted = true
	dbPortal.Insert()
	portal := &Portal{Portal: dbPortal, discordMessages: make(chan portalDiscordMessage, 1)}
	br := &DiscordBridge{DB: db, Config: &config.Config{}, portalsByMXID: map[id.RoomID]*Portal{dbPortal.MXID: portal}}
	portal.bridge = br
	msg := db.Message.New()
	msg.Channel = dbPortal.Key
	msg.DiscordID = recoveryTestMessageID
	msg.SenderID = "2"
	msg.SenderMXID = "@discord_2:example.com"
	msg.MXID = "$original"
	msg.Timestamp = time.Unix(100, 0)
	msg.Insert()
	sess, err := discordgo.New("test-token")
	require.NoError(t, err)
	calls := new(int)
	sess.Client.Transport = presenceTestTransport(func(req *http.Request) (*http.Response, error) {
		(*calls)++
		require.Equal(t, http.MethodGet, req.Method)
		require.Equal(t, "/api/v9/channels/1000000000000000002/messages", req.URL.Path)
		require.Equal(t, recoveryTestMessageID, req.URL.Query().Get("around"))
		require.Equal(t, "1", req.URL.Query().Get("limit"))
		edited := time.Unix(200, 0)
		return recoverySourceResponse(t, &discordgo.Message{ID: recoveryTestMessageID, ChannelID: dbPortal.Key.ChannelID, Author: &discordgo.User{ID: "2"}, Content: "restored text", EditedTimestamp: &edited}), nil
	})
	u := newTestUser()
	u.User = &database.User{MXID: "@admin:example.com", DiscordID: "1", ManagementRoom: "!management:example.com"}
	u.PermissionLevel = bridgeconfig.PermissionLevelAdmin
	u.Session = sess
	u.bridge = br
	return br, u, portal, calls
}

func recoverySourceResponse(t *testing.T, msg *discordgo.Message) *http.Response {
	t.Helper()
	body, err := json.Marshal([]*discordgo.Message{msg})
	require.NoError(t, err)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}
}

func TestRecoveryMessageRejectsWithoutSourceRead(t *testing.T) {
	for _, tc := range []struct {
		name, messageID, roomID string
		mutate                  func(*User)
	}{
		{"non_admin", recoveryTestMessageID, "!recovery:example.com", func(u *User) { u.PermissionLevel = bridgeconfig.PermissionLevelUser }},
		{"not_connected", recoveryTestMessageID, "!recovery:example.com", func(u *User) { u.Session = nil }},
		{"not_own_management_room", recoveryTestMessageID, "!recovery:example.com", func(u *User) { u.ManagementRoom = "!other:example.com" }},
		{"no_management_room", recoveryTestMessageID, "!recovery:example.com", func(u *User) { u.ManagementRoom = "" }},
		{"invalid_id", "not-a-number", "!recovery:example.com", nil},
		{"overflow_id", "99999999999999999999", "!recovery:example.com", nil},
		{"zero_id", "0", "!recovery:example.com", nil},
		{"signed_id", "+3", "!recovery:example.com", nil},
		{"unknown_room", recoveryTestMessageID, "!unknown:example.com", nil},
		{"unknown_message", "1000000000000000003", "!recovery:example.com", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			br, user, portal, calls := newRecoveryTest(t)
			if tc.mutate != nil {
				tc.mutate(user)
			}
			_, err := br.recoverMessageEdit(context.Background(), user, "!management:example.com", id.RoomID(tc.roomID), tc.messageID)
			require.Error(t, err)
			require.Zero(t, *calls)
			require.Empty(t, portal.discordMessages)
		})
	}
}

func TestRecoveryMessageQueuesNormalEdit(t *testing.T) {
	br, user, portal, calls := newRecoveryTest(t)
	require.True(t, cmdResyncMessage.RequiresAdmin)
	result, err := br.recoverMessageEdit(context.Background(), user, user.ManagementRoom, portal.MXID, recoveryTestMessageID)
	require.NoError(t, err)
	require.Equal(t, "queued", result)
	require.Equal(t, 1, *calls)
	queued := <-portal.discordMessages
	require.Same(t, user, queued.user)
	update, ok := queued.msg.(*discordgo.MessageUpdate)
	require.True(t, ok)
	require.Equal(t, "restored text", update.Content)
	require.Equal(t, recoveryTestMessageID, update.ID)
	require.True(t, portal.Encrypted, "recovery uses the existing encrypted portal writer")
	require.True(t, br.DB.Message.GetFirstByDiscordID(portal.Key, recoveryTestMessageID).EditTimestamp.IsZero(), "queue acceptance must not update the delivery timestamp")
}

func TestRecoveryMessageValidatesFetchedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*discordgo.Message)
	}{
		{"wrong_id", func(m *discordgo.Message) { m.ID = "3" }},
		{"wrong_channel", func(m *discordgo.Message) { m.ChannelID = "4" }},
		{"wrong_author", func(m *discordgo.Message) { m.Author.ID = "5" }},
		{"missing_author", func(m *discordgo.Message) { m.Author = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			br, user, portal, _ := newRecoveryTest(t)
			user.Session.Client.Transport = presenceTestTransport(func(*http.Request) (*http.Response, error) {
				ts := time.Unix(200, 0)
				msg := &discordgo.Message{ID: recoveryTestMessageID, ChannelID: portal.Key.ChannelID, Author: &discordgo.User{ID: "2"}, EditedTimestamp: &ts}
				tc.mutate(msg)
				return recoverySourceResponse(t, msg), nil
			})
			_, err := br.recoverMessageEdit(context.Background(), user, user.ManagementRoom, portal.MXID, recoveryTestMessageID)
			require.Error(t, err)
			require.Empty(t, portal.discordMessages)
		})
	}
}

func TestRecoveryMessageDoesNotQueueUneditedOrAlreadyApplied(t *testing.T) {
	for _, edited := range []*time.Time{nil, ptrTime(time.Unix(50, 0)), ptrTime(time.Unix(100, 0))} {
		br, user, portal, _ := newRecoveryTest(t)
		br.DB.Message.GetFirstByDiscordID(portal.Key, recoveryTestMessageID).UpdateEditTimestamp(time.Unix(100, 0))
		user.Session.Client.Transport = presenceTestTransport(func(*http.Request) (*http.Response, error) {
			return recoverySourceResponse(t, &discordgo.Message{ID: recoveryTestMessageID, ChannelID: portal.Key.ChannelID, Author: &discordgo.User{ID: "2"}, EditedTimestamp: edited}), nil
		})
		result, err := br.recoverMessageEdit(context.Background(), user, user.ManagementRoom, portal.MXID, recoveryTestMessageID)
		require.NoError(t, err)
		require.NotEqual(t, "queued", result)
		require.Empty(t, portal.discordMessages)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func TestRecoveryMessageSourceDeniedQueueFullAndChangedSession(t *testing.T) {
	br, user, portal, _ := newRecoveryTest(t)
	transport := user.Session.Client.Transport
	user.Session.Client.Transport = presenceTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":50001,"message":"private diagnostic"}`))}, nil
	})
	_, err := br.recoverMessageEdit(context.Background(), user, user.ManagementRoom, portal.MXID, recoveryTestMessageID)
	require.ErrorContains(t, err, "could not fetch")
	require.NotContains(t, err.Error(), "private diagnostic")
	require.Empty(t, portal.discordMessages)
	user.Session.Client.Transport = transport
	portal.discordMessages <- portalDiscordMessage{}
	_, err = br.recoverMessageEdit(context.Background(), user, user.ManagementRoom, portal.MXID, recoveryTestMessageID)
	require.ErrorContains(t, err, "queue is full")
	require.Len(t, portal.discordMessages, 1)
	<-portal.discordMessages
	user.Session.Client.Transport = presenceTestTransport(func(req *http.Request) (*http.Response, error) {
		user.Lock()
		user.Session = nil
		user.Unlock()
		return transport.RoundTrip(req)
	})
	_, err = br.recoverMessageEdit(context.Background(), user, user.ManagementRoom, portal.MXID, recoveryTestMessageID)
	require.ErrorContains(t, err, "session changed")
	require.Empty(t, portal.discordMessages)
}

func TestRecoveryMessageUsesMappedThreadChannel(t *testing.T) {
	br, user, portal, _ := newRecoveryTest(t)
	const threadID = "1000000000000000004"
	_, err := br.DB.Exec("UPDATE message SET dc_thread_id=$1 WHERE dcid=$2", threadID, recoveryTestMessageID)
	require.NoError(t, err)
	user.Session.Client.Transport = presenceTestTransport(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "/api/v9/channels/"+threadID+"/messages", req.URL.Path)
		require.Equal(t, recoveryTestMessageID, req.URL.Query().Get("around"))
		ts := time.Unix(200, 0)
		return recoverySourceResponse(t, &discordgo.Message{ID: recoveryTestMessageID, ChannelID: threadID, Author: &discordgo.User{ID: "2"}, EditedTimestamp: &ts}), nil
	})
	result, err := br.recoverMessageEdit(context.Background(), user, user.ManagementRoom, portal.MXID, recoveryTestMessageID)
	require.NoError(t, err)
	require.Equal(t, "queued", result)
	require.Len(t, portal.discordMessages, 1)
}
