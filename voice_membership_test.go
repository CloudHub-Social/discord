package main

import (
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-discord/config"
	"go.mau.fi/mautrix-discord/database"
)

func TestSnapshotDiscordVoiceMembershipsRespectsVisibility(t *testing.T) {
	state := discordgo.NewState()
	guild := &discordgo.Guild{
		ID:      "10",
		Roles:   []*discordgo.Role{{ID: "10", Permissions: discordgo.PermissionViewChannel}},
		Members: []*discordgo.Member{{User: &discordgo.User{ID: "1"}}},
		Channels: []*discordgo.Channel{
			{ID: "20", GuildID: "10", Type: discordgo.ChannelTypeGuildVoice},
			{ID: "21", GuildID: "10", Type: discordgo.ChannelTypeGuildVoice, PermissionOverwrites: []*discordgo.PermissionOverwrite{{ID: "10", Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel}}},
		},
		VoiceStates: []*discordgo.VoiceState{{UserID: "2", ChannelID: "20"}, {UserID: "3", ChannelID: "21"}},
	}
	require.NoError(t, state.GuildAdd(guild))
	require.Equal(t, map[voiceMembership]struct{}{{channelID: "20", userID: "2"}: {}}, snapshotDiscordVoiceMemberships(state, "1"))
	require.Empty(t, snapshotDiscordVoiceMemberships(state, "unknown"))
}

func TestStaleVoiceWorkerReturnsWakeToNewConnection(t *testing.T) {
	bridge := &DiscordBridge{Config: &config.Config{}}
	bridge.Config.Bridge.DiscordVoiceActivity = true
	user := &User{User: &database.User{}, bridge: bridge, voiceMirrorWake: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() { defer close(done); user.runDiscordVoiceMirror(0, 0) }()
	require.Eventually(t, func() bool {
		user.voiceMirrorLock.Lock()
		defer user.voiceMirrorLock.Unlock()
		return user.voiceMirrorRunning
	}, 2*time.Second, 10*time.Millisecond)
	user.reconnectLock.Lock()
	user.sessionGeneration = 1
	user.reconnectLock.Unlock()
	user.voiceMirrorWake <- struct{}{}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stale worker did not exit")
	}
	select {
	case <-user.voiceMirrorWake:
	default:
		t.Fatal("stale worker consumed the new connection's wake")
	}
}

func TestStaleVoiceWorkerDoesNotReplaceCurrentWorker(t *testing.T) {
	bridge := &DiscordBridge{Config: &config.Config{}}
	bridge.Config.Bridge.DiscordVoiceActivity = true
	user := &User{User: &database.User{}, bridge: bridge, sessionGeneration: 2, connectionEpoch: 1, voiceMirrorRunning: true, voiceMirrorGeneration: 2, voiceMirrorEpoch: 1}
	user.runDiscordVoiceMirror(1, 0)
	require.True(t, user.voiceMirrorRunning)
	require.Equal(t, uint64(2), user.voiceMirrorGeneration)
	require.Equal(t, uint64(1), user.voiceMirrorEpoch)
}

func TestClearUnpublishedVoiceMembershipDropsPending(t *testing.T) {
	member := voiceMembership{channelID: "20", userID: "2"}
	user := &User{voiceMirrorPending: map[voiceMembership]struct{}{member: {}}}
	user.clearPublishedVoiceMembership(member, "1")
	require.Empty(t, user.voiceMirrorPending)
}

func TestDiscordVoiceMembershipContent(t *testing.T) {
	content := discordVoiceMembershipContent(time.UnixMilli(123456), "1")
	require.Equal(t, int64(123456), content["created_ts"])
	require.Equal(t, int64(300000), content["expires"])
	require.Empty(t, content["foci_preferred"], "Discord audio must not advertise a Matrix media server")
	require.Equal(t, "_@discord_42:example.org_DISCORDVOICE1_m.call", discordVoiceMembershipStateKey("@discord_42:example.org", "1"))
}

func TestOwnedDiscordVoiceState(t *testing.T) {
	ghost := id.UserID("@discord_42:example.org")
	parse := func(user id.UserID) (string, bool) { return "42", user == ghost }
	key := discordVoiceMembershipStateKey(string(ghost), "1")
	makeEvent := func(sender id.UserID, key string, content map[string]any) *event.Event {
		return &event.Event{Sender: sender, StateKey: &key, Content: event.Content{Raw: content}}
	}
	owned := makeEvent(ghost, key, map[string]any{"application": "m.call"})
	user, ok := isOwnedDiscordVoiceState(owned, "1", parse)
	require.True(t, ok)
	require.Equal(t, "42", user)
	_, ok = isOwnedDiscordVoiceState(owned, "2", parse)
	require.False(t, ok, "other bridge accounts have independent memberships")
	_, ok = isOwnedDiscordVoiceState(makeEvent(ghost, key, map[string]any{}), "1", parse)
	require.False(t, ok, "a cleared membership is not active")
	_, ok = isOwnedDiscordVoiceState(makeEvent("@other:example.org", key, map[string]any{"application": "m.call"}), "1", parse)
	require.False(t, ok, "never touch another sender's state")
}

func TestDiscordVoiceGatewaySnapshotsAndPassiveUpdates(t *testing.T) {
	state := discordgo.NewState()
	require.NoError(t, state.GuildAdd(&discordgo.Guild{ID: "10", Channels: []*discordgo.Channel{{ID: "20", GuildID: "10", Type: discordgo.ChannelTypeGuildVoice}}, VoiceStates: []*discordgo.VoiceState{{GuildID: "10", ChannelID: "20", UserID: "2"}, {GuildID: "10", ChannelID: "20", UserID: "3"}}}))
	known := map[string]bool{}
	require.NoError(t, applyDiscordVoiceGatewayEvent(state, known, "READY_SUPPLEMENTAL", []byte(`{"guilds":[{"id":"10","voice_states":[{"user_id":"2","channel_id":"20"}]},{"id":"11"}]}`)))
	require.True(t, known["10"])
	require.False(t, known["11"])
	guild, err := state.Guild("10")
	require.NoError(t, err)
	require.Len(t, guild.VoiceStates, 1)
	require.Equal(t, "2", guild.VoiceStates[0].UserID)
	require.NoError(t, applyDiscordVoiceGatewayEvent(state, known, "PASSIVE_UPDATE_V2", []byte(`{"guild_id":"10","updated_voice_states":[{"user_id":"4","channel_id":"20"}],"removed_voice_states":["2"]}`)))
	require.Len(t, guild.VoiceStates, 1)
	require.Equal(t, "4", guild.VoiceStates[0].UserID)
	require.NoError(t, applyDiscordVoiceGatewayEvent(state, known, "READY_SUPPLEMENTAL", []byte(`{"guilds":[{"id":"10","voice_states":[]}]}`)))
	require.Empty(t, guild.VoiceStates)
}

func TestVoicePendingRetryRequiresCurrentConfirmationOrSnapshot(t *testing.T) {
	member := voiceMembership{channelID: "20", userID: "2"}
	user := &User{voiceMirrorConfirmed: map[voiceMembership]voiceConnection{member: {generation: 3, epoch: 4}}}
	require.True(t, user.canRetryVoiceClear(member, false, 3, 4), "confirmed leave on this connection can retry without a snapshot")
	require.False(t, user.canRetryVoiceClear(member, false, 4, 5), "a previous connection cannot confirm current absence")
	require.True(t, user.canRetryVoiceClear(member, true, 4, 5), "a complete new snapshot can confirm absence")
}
