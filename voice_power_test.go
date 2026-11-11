package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/id"
)

func TestPlanDiscordVoicePower(t *testing.T) {
	ghost, owner, bot := id.UserID("@discord_2:example.com"), id.UserID("@owner:example.com"), id.UserID("@bridge:example.com")
	isGhost := func(uid id.UserID) bool { return uid == ghost }
	base := func() map[string]any {
		return map[string]any{"users": map[string]any{string(bot): 100}, "events": map[string]any{"m.room.topic": 50}, "custom_extension": map[string]any{"keep": true}}
	}
	updated, changed, err := planDiscordVoicePower(base(), ghost, owner, bot, isGhost)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, 1, updated["users"].(map[string]any)[string(ghost)])
	require.Equal(t, 1, updated["events"].(map[string]any)[discordVoiceEventType])
	require.Equal(t, true, updated["custom_extension"].(map[string]any)["keep"])
	large := base()
	large["custom_extension"].(map[string]any)["large"] = json.Number("9007199254740993")
	updatedLarge, _, err := planDiscordVoicePower(large, ghost, owner, bot, isGhost)
	require.NoError(t, err)
	require.Equal(t, json.Number("9007199254740993"), updatedLarge["custom_extension"].(map[string]any)["large"])
	_, changed, err = planDiscordVoicePower(updated, ghost, owner, bot, isGhost)
	require.NoError(t, err)
	require.False(t, changed, "repeat reconciliation must not rewrite room power levels")

	restrictive := base()
	restrictive["events"].(map[string]any)[discordVoiceEventType] = 50
	_, _, err = planDiscordVoicePower(restrictive, ghost, owner, bot, isGhost)
	require.Error(t, err, "explicitly restrictive call permissions are respected")
	unrelated := base()
	unrelated["events"].(map[string]any)["com.example.moderation"] = 1
	_, _, err = planDiscordVoicePower(unrelated, ghost, owner, bot, isGhost)
	require.Error(t, err, "ghost elevation cannot grant another protected event")
	other := base()
	other["users"].(map[string]any)["@other:example.com"] = 1
	_, _, err = planDiscordVoicePower(other, ghost, owner, bot, isGhost)
	require.Error(t, err, "other people must not gain call-state permission")
	open := base()
	open["events"].(map[string]any)[discordVoiceEventType] = 0
	_, changed, err = planDiscordVoicePower(open, ghost, owner, bot, isGhost)
	require.NoError(t, err)
	require.False(t, changed, "already-permitted events need no power changes")
}
