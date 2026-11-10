package main

import (
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/id"
)

func TestCallRingTracker(t *testing.T) {
	var tracker callRingTracker
	ring := json.RawMessage(`{"channel_id":"10","message_id":"20","ringing":["1"]}`)
	_, first, notify := tracker.accept(1, 1, 1, "CALL_CREATE", ring, "1")
	require.True(t, notify)
	require.True(t, tracker.isRinging("10", first))
	_, _, notify = tracker.accept(1, 1, 2, "CALL_UPDATE", ring, "1")
	require.False(t, notify, "repeated ring is deduplicated")
	_, _, notify = tracker.accept(1, 1, 3, "CALL_UPDATE", json.RawMessage(`{"channel_id":"10","message_id":"20"}`), "1")
	require.False(t, notify, "missing ringing list is not an end")
	_, _, notify = tracker.accept(1, 1, 4, "CALL_UPDATE", json.RawMessage(`{"channel_id":"10","message_id":"20","ringing":[]}`), "1")
	require.False(t, notify)
	require.False(t, tracker.isRinging("10", first))
	_, second, notify := tracker.accept(1, 1, 5, "CALL_UPDATE", ring, "1")
	require.True(t, notify, "a fresh ring after stop is delivered")
	require.NotEqual(t, first, second)
	require.False(t, tracker.isRinging("10", first), "canceled alert must stay canceled")
	tracker.complete("10", first, true)
	require.True(t, tracker.isRinging("10", second), "stale completion must not consume the new ring")
	_, _, notify = tracker.accept(1, 1, 4, "CALL_UPDATE", ring, "1")
	require.False(t, notify, "out of order events are ignored")
	_, _, notify = tracker.accept(1, 2, 1, "CALL_CREATE", ring, "1")
	require.True(t, notify, "new connection resets sequence and state")
}

func TestCallRingTrackerRejectsInvalidOrUnrelatedEvents(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"channel_id":"x","message_id":"20","ringing":["1"]}`,
		`{"channel_id":"10","message_id":"20","guild_id":"30","ringing":["1"]}`,
		`{"channel_id":"10","message_id":"20","ringing":["2"]}`,
	} {
		var tracker callRingTracker
		_, _, notify := tracker.accept(1, 1, 1, "CALL_CREATE", json.RawMessage(raw), "1")
		require.False(t, notify)
	}
}

func TestCallRingTrackerRetriesFailedDelivery(t *testing.T) {
	var tracker callRingTracker
	ring := json.RawMessage(`{"channel_id":"10","message_id":"20","ringing":["1"]}`)
	_, first, notify := tracker.accept(1, 1, 1, "CALL_CREATE", ring, "1")
	require.True(t, notify)
	tracker.complete("10", first, false)
	_, retry, notify := tracker.accept(1, 1, 2, "CALL_UPDATE", ring, "1")
	require.True(t, notify)
	tracker.complete("10", retry, true)
	_, _, notify = tracker.accept(1, 1, 3, "CALL_UPDATE", ring, "1")
	require.False(t, notify)
}

func TestCallRingTrackerBoundsFailedDeliveryRetries(t *testing.T) {
	var tracker callRingTracker
	raw := json.RawMessage(`{"channel_id":"10","message_id":"20","ringing":["1"]}`)
	for sequence := int64(1); sequence <= 3; sequence++ {
		_, ring, notify := tracker.accept(1, 1, sequence, "CALL_UPDATE", raw, "1")
		require.True(t, notify)
		tracker.complete("10", ring, false)
	}
	_, _, notify := tracker.accept(1, 1, 4, "CALL_UPDATE", raw, "1")
	require.False(t, notify)
}

func TestCallAlertDroppedAfterPortalRemovalCanRetry(t *testing.T) {
	_, user, portal, _ := newRecoveryTest(t)
	portal.MXID = ""
	portal.log = zerolog.Nop()
	raw := json.RawMessage(`{"channel_id":"1000000000000000002","message_id":"20","ringing":["1"]}`)
	channel, ring, notify := user.callRings.accept(1, 1, 1, "CALL_CREATE", raw, "1")
	require.True(t, notify)
	portal.handleDiscordMessages(portalDiscordMessage{user: user, msg: &discordCallAlert{ring: ring}})
	_, _, retry := user.callRings.accept(1, 1, 2, "CALL_UPDATE", raw, "1")
	require.Equal(t, portal.Key.ChannelID, channel)
	require.True(t, retry)
}

func TestDiscordCallAlertContent(t *testing.T) {
	content := discordCallAlertContent("10", "20", "@evie:example.org")
	require.NotNil(t, content)
	require.Contains(t, content.Body, "https://discord.com/channels/@me/10/20")
	require.Equal(t, []id.UserID{"@evie:example.org"}, content.Mentions.UserIDs)
	require.Nil(t, discordCallAlertContent("not-a-channel", "20", "@evie:example.org"))
}
