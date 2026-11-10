package main

import (
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
)

func TestSnapshotVoiceStatusRespectsKnownChannels(t *testing.T) {
	state := discordgo.NewState()
	guild := &discordgo.Guild{ID: "10", Roles: []*discordgo.Role{{ID: "10", Permissions: discordgo.PermissionViewChannel}}, Channels: []*discordgo.Channel{{ID: "20", GuildID: "10", Name: "Games"}, {ID: "21", GuildID: "10", Name: "Hidden", PermissionOverwrites: []*discordgo.PermissionOverwrite{{ID: "10", Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel}}}}, Members: []*discordgo.Member{{User: &discordgo.User{ID: "30", Username: "Sam"}}, {User: &discordgo.User{ID: "1"}}}, VoiceStates: []*discordgo.VoiceState{{UserID: "30", ChannelID: "20"}, {UserID: "31", ChannelID: "21"}, {UserID: "31", ChannelID: "99"}, {UserID: "32", ChannelID: ""}}}
	require.NoError(t, state.GuildAdd(guild))
	got, known := snapshotVoiceStatus(state, "10", "1")
	require.True(t, known)
	require.Equal(t, []voiceStatusEntry{{user: "Sam", channel: "Games", channelID: "20"}}, got)
	got, known = snapshotVoiceStatus(state, "10", "missing")
	require.True(t, known)
	require.Empty(t, got, "unknown permissions must not disclose activity")
	_, known = snapshotVoiceStatus(state, "11", "1")
	require.False(t, known)
}

func TestSafeVoiceNameDoesNotCreateMentionsOrMarkup(t *testing.T) {
	require.Equal(t, "@\u200beveryone \\*hello\\*", safeVoiceName("@everyone *hello*"))
}
