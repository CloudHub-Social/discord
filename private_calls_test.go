package main

import (
	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPrivateCallLifecycle(t *testing.T) {
	var p privateCalls
	p.event("CALL_CREATE", []byte(`{"channel_id":"20","voice_states":[{"user_id":"1","session_id":"a","channel_id":"20"},{"user_id":"1","session_id":"b","channel_id":"20"},{"user_id":"2","session_id":"c","channel_id":"20"}]}`))
	snapshot := func() (map[voiceMembership]struct{}, map[string]bool) {
		o := map[voiceMembership]struct{}{}
		k := map[string]bool{}
		p.snapshot(o, k)
		return o, k
	}
	o, k := snapshot()
	require.True(t, k["20"])
	require.Len(t, o, 2)
	p.voice(&discordgo.VoiceState{UserID: "1", SessionID: "a"})
	o, _ = snapshot()
	require.Contains(t, o, voiceMembership{channelID: "20", userID: "1"})
	p.event("CALL_UPDATE", []byte(`{"channel_id":"20","ringing":[]}`))
	o, _ = snapshot()
	require.Len(t, o, 2)
	p.event("CALL_DELETE", []byte(`{"channel_id":"20","unavailable":true}`))
	o, k = snapshot()
	require.Empty(t, o)
	require.False(t, k["20"])
	p.event("CALL_CREATE", []byte(`{"channel_id":"20","voice_states":[{"user_id":"1","session_id":"b"}]}`))
	require.Equal(t, []string{"20"}, p.voice(&discordgo.VoiceState{UserID: "1", SessionID: "b"}))
	o, k = snapshot()
	require.NotContains(t, o, voiceMembership{channelID: "20", userID: "1"}, "an unconfirmed departure must not be refreshed")
	require.False(t, k["20"], "absence is not confirmed")
	p.event("CALL_CREATE", []byte(`{"channel_id":"20","voice_states":[]}`))
	o, k = snapshot()
	require.Empty(t, o)
	require.True(t, k["20"])
	p.event("CALL_DELETE", []byte(`{"channel_id":"20"}`))
	_, k = snapshot()
	require.True(t, k["20"])
	p.reset()
	_, k = snapshot()
	require.False(t, k["20"])
}

func TestPrivateCallMissingSnapshotDoesNotClear(t *testing.T) {
	var p privateCalls
	p.event("CALL_CREATE", []byte(`{"channel_id":"20","voice_states":[{"user_id":"1","session_id":"a"}]}`))
	p.event("CALL_CREATE", []byte(`{"channel_id":"20"}`))
	o := map[voiceMembership]struct{}{}
	k := map[string]bool{}
	p.snapshot(o, k)
	require.Contains(t, o, voiceMembership{channelID: "20", userID: "1"})
}
