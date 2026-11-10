package main

import (
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
)

func TestSnapshotVoiceStatusRespectsKnownChannels(t *testing.T) {
	state := discordgo.NewState()
	state.Guilds = []*discordgo.Guild{{ID: "10", Channels: []*discordgo.Channel{{ID: "20", Name: "Games"}}, Members: []*discordgo.Member{{User: &discordgo.User{ID: "30", Username: "Sam"}}}, VoiceStates: []*discordgo.VoiceState{{UserID: "30", ChannelID: "20"}, {UserID: "31", ChannelID: "99"}, {UserID: "32", ChannelID: ""}}}}
	got, known := snapshotVoiceStatus(state, "10")
	require.True(t, known)
	require.Equal(t, []voiceStatusEntry{{user: "Sam", channel: "Games"}}, got)
	_, known = snapshotVoiceStatus(state, "11")
	require.False(t, known)
}

func TestSafeVoiceNameDoesNotCreateMentionsOrMarkup(t *testing.T) {
	require.Equal(t, "@\u200beveryone \\*hello\\*", safeVoiceName("@everyone *hello*"))
}
