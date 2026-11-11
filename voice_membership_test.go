package main

import (
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
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
