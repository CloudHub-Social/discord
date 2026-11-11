package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// The dedicated device name makes this state key distinct from real clients.
const discordVoiceDeviceID = "DISCORDVOICE"
const discordVoiceMembershipLifetime = 5 * time.Minute
const discordVoiceRefreshInterval = 2 * time.Minute

// discordVoiceMembershipContent constructs a MatrixRTC membership with no
// focus URL because the bridge does not provide a Matrix audio connection.
func discordVoiceMembershipContent(now time.Time, accountID string) map[string]any {
	return map[string]any{
		"application":    "m.call",
		"call_id":        "",
		"scope":          "m.room",
		"device_id":      discordVoiceDeviceID + accountID,
		"created_ts":     now.UnixMilli(),
		"expires":        discordVoiceMembershipLifetime.Milliseconds(),
		"focus_active":   map[string]string{"type": "livekit", "focus_selection": "oldest_membership"},
		"foci_preferred": []any{},
	}
}

func discordVoiceMembershipStateKey(mxid, accountID string) string {
	return fmt.Sprintf("_%s_%s%s_m.call", mxid, discordVoiceDeviceID, accountID)
}

// isOwnedDiscordVoiceState only matches a nonempty call state sent by a bridge
// ghost under the current Discord account's dedicated device key.
func isOwnedDiscordVoiceState(evt *event.Event, accountID string, parseGhost func(id.UserID) (string, bool)) (string, bool) {
	if evt == nil || evt.StateKey == nil || accountID == "" || evt.Content.Raw["application"] != "m.call" {
		return "", false
	}
	if *evt.StateKey != discordVoiceMembershipStateKey(string(evt.Sender), accountID) || !strings.HasPrefix(string(evt.Sender), "@") {
		return "", false
	}
	return parseGhost(evt.Sender)
}

// voiceMembership identifies one bridge-owned Matrix call membership. A
// participant can move between rooms, so the room is part of its identity.
type voiceMembership struct {
	channelID string
	userID    string
}

// snapshotDiscordVoiceMemberships only returns channels the logged-in Discord
// account is known to see. Discord's user-session cache may be incomplete.
func snapshotDiscordVoiceMemberships(state *discordgo.State, userID string) map[voiceMembership]struct{} {
	result := make(map[voiceMembership]struct{})
	if state == nil || userID == "" {
		return result
	}
	type candidate struct{ channelID, userID string }
	var candidates []candidate
	state.RLock()
	for _, guild := range state.Guilds {
		if guild == nil || guild.Unavailable {
			continue
		}
		channels := make(map[string]bool)
		for _, channel := range guild.Channels {
			if channel != nil && (channel.Type == discordgo.ChannelTypeGuildVoice || channel.Type == discordgo.ChannelTypeGuildStageVoice) {
				channels[channel.ID] = true
			}
		}
		for _, voice := range guild.VoiceStates {
			if voice != nil && voice.UserID != "" && channels[voice.ChannelID] {
				candidates = append(candidates, candidate{voice.ChannelID, voice.UserID})
			}
		}
	}
	state.RUnlock()
	for _, c := range candidates {
		permissions, err := state.UserChannelPermissions(userID, c.channelID)
		if err == nil && permissions&discordgo.PermissionViewChannel != 0 {
			result[voiceMembership{channelID: c.channelID, userID: c.userID}] = struct{}{}
		}
	}
	return result
}

// runDiscordVoiceMirror refreshes only currently observed memberships. Missing
// entries are not explicitly removed because the upstream snapshot may be
// incomplete; they expire automatically when no longer refreshed.
func (user *User) runDiscordVoiceMirror(generation, epoch uint64) {
	if !user.bridge.Config.Bridge.DiscordVoiceActivity {
		return
	}
	user.voiceMirrorLock.Lock()
	if user.voiceMirrorRunning && user.voiceMirrorGeneration == generation && user.voiceMirrorEpoch == epoch {
		user.voiceMirrorLock.Unlock()
		return
	}
	user.voiceMirrorRunning = true
	user.voiceMirrorGeneration, user.voiceMirrorEpoch = generation, epoch
	user.voiceMirrorLock.Unlock()
	defer func() {
		user.voiceMirrorLock.Lock()
		if user.voiceMirrorGeneration == generation && user.voiceMirrorEpoch == epoch {
			user.voiceMirrorRunning = false
		}
		user.voiceMirrorLock.Unlock()
	}()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var lastReconcile time.Time
	bootstrapped := false
	var nextBootstrapAttempt time.Time
	for user.isCurrentConnection(generation, epoch) {
		if wait := time.Until(lastReconcile.Add(time.Second)); wait > 0 {
			time.Sleep(wait)
			if !user.isCurrentConnection(generation, epoch) {
				return
			}
		}
		lastReconcile = time.Now()
		user.voiceMirrorLock.Lock()
		user.Lock()
		session, discordID := user.Session, user.DiscordID
		user.Unlock()
		if session != nil && session.State != nil {
			if user.voiceMirrorPublished == nil {
				user.voiceMirrorPublished = make(map[voiceMembership]time.Time)
			}
			if user.voiceMirrorPending == nil {
				user.voiceMirrorPending = make(map[voiceMembership]struct{})
			}
			observed := snapshotDiscordVoiceMemberships(session.State, discordID)
			if !bootstrapped && discordID != "" && !time.Now().Before(nextBootstrapAttempt) {
				bootstrapped = user.restoreDiscordVoiceState(generation, epoch, discordID, observed)
				nextBootstrapAttempt = time.Now().Add(time.Minute)
			}
			for member := range user.voiceMirrorPending {
				if _, returned := observed[member]; returned {
					delete(user.voiceMirrorPending, member)
				} else {
					user.clearPublishedVoiceMembership(member, discordID)
				}
			}
			audienceByChannel := make(map[string]bool)
			for member := range observed {
				if !user.isCurrentConnection(generation, epoch) {
					user.voiceMirrorLock.Unlock()
					return
				}
				portal := user.GetExistingPortalByID(member.channelID)
				if portal == nil || portal.MXID == "" {
					continue
				}
				// Fail closed when other real Matrix users can see the room. The
				// Discord permission check only establishes this account's access.
				allowed, checked := audienceByChannel[member.channelID]
				if !checked {
					viewers, err := portal.getMatrixUsers()
					allowed = err == nil && len(viewers) == 1 && viewers[0] == user.MXID
					audienceByChannel[member.channelID] = allowed
				}
				if !allowed {
					user.voiceMirrorPending[member] = struct{}{}
					user.clearPublishedVoiceMembership(member, discordID)
					continue
				}
				if last, exists := user.voiceMirrorPublished[member]; exists && time.Since(last) < discordVoiceRefreshInterval {
					continue
				}
				puppet := user.bridge.GetPuppetByID(member.userID)
				intent := puppet.DefaultIntent()
				stateKey := discordVoiceMembershipStateKey(string(puppet.MXID), discordID)
				eventType := event.Type{Type: "org.matrix.msc3401.call.member", Class: event.StateEventType}
				if _, err := intent.SendStateEvent(portal.MXID, eventType, stateKey, discordVoiceMembershipContent(time.Now(), discordID)); err != nil {
					user.log.Warn().Err(err).Msg("Failed to publish Discord voice membership")
				} else {
					user.voiceMirrorPublished[member] = time.Now()
				}
			}
		}
		user.voiceMirrorLock.Unlock()
		select {
		case <-ticker.C:
		case <-user.voiceMirrorWake:
		}
	}
}

// restoreDiscordVoiceState reconciles state left by an earlier bridge process.
// Missing cached Discord activity is cleared rather than displayed as certain;
// future gateway updates can publish it again. Calls belonging to real Matrix
// devices or another linked account are never modified.
func (user *User) restoreDiscordVoiceState(generation, epoch uint64, discordID string, observed map[voiceMembership]struct{}) bool {
	if user.bridge.DB == nil {
		return true
	}
	return user.restoreDiscordVoicePortals(generation, epoch, discordID, observed, user.bridge.GetAllPortals())
}

func (user *User) restoreDiscordVoicePortals(generation, epoch uint64, discordID string, observed map[voiceMembership]struct{}, portals []*Portal) bool {
	complete := true
	for _, portal := range portals {
		if !user.isCurrentConnection(generation, epoch) {
			return false
		}
		if portal == nil || portal.MXID == "" || (portal.Type != discordgo.ChannelTypeGuildVoice && portal.Type != discordgo.ChannelTypeGuildStageVoice) {
			continue
		}
		state, err := portal.MainIntent().State(portal.MXID)
		if err != nil {
			user.log.Warn().Err(err).Msg("Failed to inspect Discord voice state after reconnect")
			complete = false
			continue
		}
		for typ, events := range state {
			if typ.Type != "org.matrix.msc3401.call.member" {
				continue
			}
			for _, evt := range events {
				discordUser, owned := isOwnedDiscordVoiceState(evt, discordID, user.bridge.ParsePuppetMXID)
				if !owned {
					continue
				}
				member := voiceMembership{channelID: portal.Key.ChannelID, userID: discordUser}
				user.voiceMirrorPublished[member] = time.Time{}
				if _, present := observed[member]; !present {
					user.voiceMirrorPending[member] = struct{}{}
				}
			}
		}
	}
	return complete
}

// A concrete Discord move/leave can clear immediately, without assuming that
// an incomplete guild snapshot is authoritative.
func (user *User) clearDiscordVoiceDeparture(update *discordgo.VoiceStateUpdate, generation, epoch uint64) {
	if !user.bridge.Config.Bridge.DiscordVoiceActivity || update == nil || update.VoiceState == nil || update.BeforeUpdate == nil || update.BeforeUpdate.ChannelID == "" || update.BeforeUpdate.ChannelID == update.ChannelID {
		return
	}
	user.voiceMirrorLock.Lock()
	defer user.voiceMirrorLock.Unlock()
	if !user.isCurrentConnection(generation, epoch) {
		return
	}
	user.Lock()
	session, discordID := user.Session, user.DiscordID
	user.Unlock()
	if session == nil || session.State == nil {
		return
	}
	oldChannel := update.BeforeUpdate.ChannelID
	memberKey := voiceMembership{channelID: oldChannel, userID: update.UserID}
	if _, published := user.voiceMirrorPublished[memberKey]; !published {
		return
	}
	for member := range snapshotDiscordVoiceMemberships(session.State, discordID) {
		if member.channelID == oldChannel && member.userID == update.UserID {
			return // a later event has already placed this user back here
		}
	}
	if user.voiceMirrorPending == nil {
		user.voiceMirrorPending = make(map[voiceMembership]struct{})
	}
	user.voiceMirrorPending[memberKey] = struct{}{}
	user.clearPublishedVoiceMembership(memberKey, discordID)
}

// The caller holds voiceMirrorLock. Only clear a membership we published, and
// keep it tracked on failures so a later reconciliation can retry.
func (user *User) clearPublishedVoiceMembership(member voiceMembership, discordID string) {
	if _, published := user.voiceMirrorPublished[member]; !published {
		return
	}
	portal := user.GetExistingPortalByID(member.channelID)
	puppet := user.bridge.GetPuppetByIDIfExists(member.userID)
	if portal == nil || portal.MXID == "" || puppet == nil {
		return
	}
	eventType := event.Type{Type: "org.matrix.msc3401.call.member", Class: event.StateEventType}
	if _, err := puppet.DefaultIntent().SendStateEvent(portal.MXID, eventType, discordVoiceMembershipStateKey(string(puppet.MXID), discordID), map[string]any{}); err != nil {
		user.log.Warn().Err(err).Msg("Failed to clear Discord voice membership")
	} else {
		delete(user.voiceMirrorPublished, member)
		delete(user.voiceMirrorPending, member)
	}
}
