package main

import (
	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPrivateCallLifecycle(t *testing.T) {
	var p privateCalls
	p.event("CALL_CREATE", []byte(`{"channel_id":"20","voice_states":[{"user_id":"1","session_id":"a","channel_id":"20"},{"user_id":"1","session_id":"b","channel_id":"20"},{"user_id":"2","session_id":"c","channel_id":"20"}]}`))
	snapshot := func() (map[voiceMembership]struct{}, map[string]bool) {
		o := map[voiceMembership]struct{}{}
		k := map[string]bool{}
		p.snapshot(o, k, time.Now())
		return o, k
	}
	o, k := snapshot()
	require.True(t, k["20"])
	require.Len(t, o, 2)
	p.voice(&discordgo.VoiceState{UserID: "1", SessionID: "a"}, time.Now())
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
	require.Equal(t, []string{"20"}, p.voice(&discordgo.VoiceState{UserID: "1", SessionID: "b"}, time.Now()))
	o, k = snapshot()
	require.Contains(t, o, voiceMembership{channelID: "20", userID: "1"}, "a departure is retained during handoff grace")
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
	p.snapshot(o, k, time.Now())
	require.Contains(t, o, voiceMembership{channelID: "20", userID: "1"})
}

func TestPrivateDepartureFallbackAndSessionHandoff(t *testing.T) {
	var p privateCalls
	p.event("CALL_CREATE", []byte(`{"channel_id":"20","voice_states":[{"user_id":"1","session_id":"a"},{"user_id":"2","session_id":"b"}]}`))
	now := time.Now()
	require.Equal(t, []string{"20"}, p.voice(&discordgo.VoiceState{UserID: "1", SessionID: "a"}, now))
	o := map[voiceMembership]struct{}{}
	k := map[string]bool{}
	p.snapshot(o, k, now.Add(19*time.Second))
	require.Contains(t, o, voiceMembership{channelID: "20", userID: "1"})
	require.False(t, k["20"])
	p.voice(&discordgo.VoiceState{UserID: "1", SessionID: "new", ChannelID: "20"}, now.Add(19*time.Second))
	o = map[voiceMembership]struct{}{}
	k = map[string]bool{}
	p.snapshot(o, k, now.Add(25*time.Second))
	require.Contains(t, o, voiceMembership{channelID: "20", userID: "1"})
	require.True(t, k["20"])
	p.voice(&discordgo.VoiceState{UserID: "1", SessionID: "new"}, now.Add(30*time.Second))
	o = map[voiceMembership]struct{}{}
	k = map[string]bool{}
	p.snapshot(o, k, now.Add(51*time.Second))
	require.NotContains(t, o, voiceMembership{channelID: "20", userID: "1"})
	require.Contains(t, o, voiceMembership{channelID: "20", userID: "2"})
	require.True(t, k["20"])
}

func TestPrivateDepartureDoesNotSurviveOutageOrReconnect(t *testing.T) {
	now := time.Now()
	for _, mode := range []string{"outage", "reconnect"} {
		t.Run(mode, func(t *testing.T) {
			var p privateCalls
			p.event("CALL_CREATE", []byte(`{"channel_id":"20","voice_states":[{"user_id":"1","session_id":"a"}]}`))
			p.voice(&discordgo.VoiceState{UserID: "1", SessionID: "a"}, now)
			if mode == "outage" {
				p.event("CALL_DELETE", []byte(`{"channel_id":"20","unavailable":true}`))
			} else {
				p.reset()
			}
			o := map[voiceMembership]struct{}{}
			k := map[string]bool{}
			p.snapshot(o, k, now.Add(time.Minute))
			require.False(t, k["20"], "outage or reconnect cannot establish a departure")
		})
	}
}
