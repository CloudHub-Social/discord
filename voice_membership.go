package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/appservice"
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

// A departure observed on an earlier connection may have been reversed while
// disconnected, so its failed clear is not current evidence of absence.
type voiceConnection struct{ generation, epoch uint64 }

func (user *User) canRetryVoiceClear(member voiceMembership, complete bool, generation, epoch uint64) bool {
	if complete {
		return true
	}
	confirmed, ok := user.voiceMirrorConfirmed[member]
	return ok && confirmed == (voiceConnection{generation, epoch})
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

// applyDiscordVoiceGatewayEvent incorporates user-gateway voice snapshots and
// deltas that discordgo does not parse. The caller serializes gateway events.
// A missing voice_states field means unavailable, not an empty snapshot.
func applyDiscordVoiceGatewayEvent(state *discordgo.State, known map[string]bool, kind string, raw []byte) error {
	type guildSnapshot struct {
		ID          string          `json:"id"`
		VoiceStates json.RawMessage `json:"voice_states"`
	}
	var snapshots []guildSnapshot
	switch kind {
	case "READY_SUPPLEMENTAL":
		var payload struct {
			Guilds []guildSnapshot `json:"guilds"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
		snapshots = payload.Guilds
	case "PASSIVE_UPDATE_V2":
		var payload struct {
			GuildID string                  `json:"guild_id"`
			Updated []*discordgo.VoiceState `json:"updated_voice_states"`
			Removed []string                `json:"removed_voice_states"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
		state.Lock()
		defer state.Unlock()
		for _, guild := range state.Guilds {
			if guild == nil || guild.ID != payload.GuildID {
				continue
			}
			for _, id := range payload.Removed {
				guild.VoiceStates = removeVoiceUser(guild.VoiceStates, id)
			}
			for _, voice := range payload.Updated {
				if voice == nil || voice.UserID == "" {
					continue
				}
				guild.VoiceStates = removeVoiceUser(guild.VoiceStates, voice.UserID)
				if voice.ChannelID != "" {
					voice.GuildID = guild.ID
					guild.VoiceStates = append(guild.VoiceStates, voice)
				}
			}
			break
		}
		return nil
	default:
		return nil
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == "" || len(snapshot.VoiceStates) == 0 || string(snapshot.VoiceStates) == "null" {
			continue
		}
		var voices []*discordgo.VoiceState
		if err := json.Unmarshal(snapshot.VoiceStates, &voices); err != nil {
			return err
		}
		for _, voice := range voices {
			if voice == nil || voice.UserID == "" || voice.ChannelID == "" {
				return fmt.Errorf("invalid voice state in supplemental snapshot")
			}
			voice.GuildID = snapshot.ID
		}
		state.Lock()
		for _, guild := range state.Guilds {
			if guild != nil && guild.ID == snapshot.ID && !guild.Unavailable {
				guild.VoiceStates = voices
				known[snapshot.ID] = true
				break
			}
		}
		state.Unlock()
	}
	return nil
}

func removeVoiceUser(states []*discordgo.VoiceState, userID string) []*discordgo.VoiceState {
	result := states[:0]
	for _, state := range states {
		if state != nil && state.UserID != userID {
			result = append(result, state)
		}
	}
	return result
}

// snapshotDiscordVoiceMirror evaluates visibility once per channel so a
// transient permission lookup failure cannot turn an observed user into a
// supposedly departed one. The caller holds voiceSnapshotLock.
func snapshotDiscordVoiceMirror(state *discordgo.State, userID string, known map[string]bool, bot bool) (map[voiceMembership]struct{}, map[string]bool) {
	observed := make(map[voiceMembership]struct{})
	knownChannels := make(map[string]bool)
	channels := make(map[string]bool)
	var candidates []voiceMembership
	state.RLock()
	for _, guild := range state.Guilds {
		if guild == nil || guild.Unavailable {
			continue
		}
		for _, channel := range guild.Channels {
			if channel != nil && (channel.Type == discordgo.ChannelTypeGuildVoice || channel.Type == discordgo.ChannelTypeGuildStageVoice) {
				channels[channel.ID] = known[guild.ID] || (bot && guild.VoiceStates != nil)
			}
		}
		for _, voice := range guild.VoiceStates {
			if voice != nil && voice.UserID != "" && voice.ChannelID != "" {
				candidates = append(candidates, voiceMembership{channelID: voice.ChannelID, userID: voice.UserID})
			}
		}
	}
	state.RUnlock()
	allowed := make(map[string]bool)
	for channel, complete := range channels {
		permissions, err := state.UserChannelPermissions(userID, channel)
		if err == nil && permissions&discordgo.PermissionViewChannel != 0 {
			allowed[channel] = true
			if complete {
				knownChannels[channel] = true
			}
		}
	}
	for _, member := range candidates {
		if allowed[member.channelID] {
			observed[member] = struct{}{}
		}
	}
	return observed, knownChannels
}

// runDiscordVoiceMirror refreshes observed memberships and clears missing
// entries only after a complete snapshot for their guild has arrived.
func (user *User) runDiscordVoiceMirror(generation, epoch uint64) {
	if !user.bridge.Config.Bridge.DiscordVoiceActivity {
		return
	}
	user.voiceMirrorLock.Lock()
	if !user.isCurrentConnection(generation, epoch) {
		user.voiceMirrorLock.Unlock()
		return
	}
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
	restoredPrivate := make(map[string]bool)
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
			user.voiceSnapshotLock.Lock()
			observed, knownChannels := snapshotDiscordVoiceMirror(session.State, discordID, user.voiceSnapshotGuilds, !session.IsUser)
			user.privateCalls.snapshot(observed, knownChannels)
			user.voiceSnapshotLock.Unlock()
			if !bootstrapped && discordID != "" && !time.Now().Before(nextBootstrapAttempt) {
				bootstrapped = user.restoreDiscordVoiceState(generation, epoch, discordID, observed, knownChannels)
				nextBootstrapAttempt = time.Now().Add(time.Minute)
			}
			// Inspect only private rooms with authoritative call data. This avoids
			// blocking live updates on unrelated historical DM rooms.
			for channel := range knownChannels {
				if restoredPrivate[channel] {
					continue
				}
				portal := user.GetExistingPortalByID(channel)
				if portal == nil || (portal.Type != discordgo.ChannelTypeDM && portal.Type != discordgo.ChannelTypeGroupDM) {
					continue
				}
				if user.restoreDiscordVoicePortals(generation, epoch, discordID, observed, knownChannels, []*Portal{portal}) {
					restoredPrivate[channel] = true
				}
			}
			if !user.isCurrentConnection(generation, epoch) {
				user.voiceMirrorLock.Unlock()
				return
			}
			// A complete guild snapshot can prove a participant departed while
			// disconnected. An incomplete startup cache cannot.
			for member := range user.voiceMirrorPublished {
				if knownChannels[member.channelID] {
					if _, present := observed[member]; !present {
						user.voiceMirrorPending[member] = struct{}{}
					}
				}
			}
			for member := range user.voiceMirrorPending {
				if !user.canRetryVoiceClear(member, knownChannels[member.channelID], generation, epoch) {
					continue
				}
				if _, returned := observed[member]; returned {
					delete(user.voiceMirrorPending, member)
					delete(user.voiceMirrorConfirmed, member)
				} else {
					// Matrix I/O above may have taken long enough for a newer
					// private voice event to supersede this snapshot.
					user.voiceSnapshotLock.Lock()
					current := make(map[voiceMembership]struct{})
					currentKnown := make(map[string]bool)
					user.privateCalls.snapshot(current, currentKnown)
					user.voiceSnapshotLock.Unlock()
					if currentKnown[member.channelID] {
						if _, returned := current[member]; returned {
							delete(user.voiceMirrorPending, member)
							delete(user.voiceMirrorConfirmed, member)
							continue
						}
					} else if portal := user.GetExistingPortalByID(member.channelID); portal != nil && (portal.Type == discordgo.ChannelTypeDM || portal.Type == discordgo.ChannelTypeGroupDM) {
						continue
					}
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
					allowed = portal.discordVoiceAudienceIsPrivate(user.MXID)
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
				if err := portal.ensureDiscordVoicePower(puppet.MXID, user.MXID); err != nil {
					user.log.Warn().Err(err).Msg("Cannot grant narrow Discord voice membership permission")
					continue
				}
				intent := puppet.DefaultIntent()
				if err := portal.ensureDiscordVoiceGhostJoined(intent); err != nil {
					user.log.Warn().Err(err).Msg("Failed to join Discord voice ghost")
					continue
				}
				if portal.Type == discordgo.ChannelTypeDM || portal.Type == discordgo.ChannelTypeGroupDM {
					user.voiceSnapshotLock.Lock()
					current := make(map[voiceMembership]struct{})
					user.privateCalls.snapshot(current, make(map[string]bool))
					_, present := current[member]
					user.voiceSnapshotLock.Unlock()
					if !present {
						continue
					}
				}
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
			if !user.isCurrentConnection(generation, epoch) {
				// An older worker can wake after a reconnect. Return its signal
				// so the current worker can promptly process the new voice state.
				select {
				case user.voiceMirrorWake <- struct{}{}:
				default:
				}
				return
			}
		}
	}
}

// restoreDiscordVoiceState reconciles state left by an earlier bridge process.
// Missing cached Discord activity is cleared only with a complete guild snapshot.
// Calls belonging to real Matrix devices or another linked account are never modified.
func (user *User) restoreDiscordVoiceState(generation, epoch uint64, discordID string, observed map[voiceMembership]struct{}, knownChannels map[string]bool) bool {
	if user.bridge.DB == nil {
		return true
	}
	var guildPortals []*Portal
	for _, portal := range user.bridge.GetAllPortals() {
		if portal != nil && (portal.Type == discordgo.ChannelTypeGuildVoice || portal.Type == discordgo.ChannelTypeGuildStageVoice) {
			guildPortals = append(guildPortals, portal)
		}
	}
	return user.restoreDiscordVoicePortals(generation, epoch, discordID, observed, knownChannels, guildPortals)
}

func (user *User) restoreDiscordVoicePortals(generation, epoch uint64, discordID string, observed map[voiceMembership]struct{}, knownChannels map[string]bool, portals []*Portal) bool {
	complete := true
	for _, portal := range portals {
		if !user.isCurrentConnection(generation, epoch) {
			return false
		}
		if portal == nil || portal.MXID == "" || (portal.Type != discordgo.ChannelTypeGuildVoice && portal.Type != discordgo.ChannelTypeGuildStageVoice && portal.Type != discordgo.ChannelTypeDM && portal.Type != discordgo.ChannelTypeGroupDM) {
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
				if _, tracked := user.voiceMirrorPublished[member]; !tracked {
					user.voiceMirrorPublished[member] = time.Time{}
				}
				if _, present := observed[member]; !present && knownChannels[member.channelID] {
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
	if user.voiceMirrorConfirmed == nil {
		user.voiceMirrorConfirmed = make(map[voiceMembership]voiceConnection)
	}
	user.voiceMirrorConfirmed[memberKey] = voiceConnection{generation, epoch}
	user.clearPublishedVoiceMembership(memberKey, discordID)
}

// The caller holds voiceMirrorLock. Only clear a membership we published, and
// keep it tracked on failures so a later reconciliation can retry.
func (user *User) clearPublishedVoiceMembership(member voiceMembership, discordID string) {
	if _, published := user.voiceMirrorPublished[member]; !published {
		delete(user.voiceMirrorPending, member)
		delete(user.voiceMirrorConfirmed, member)
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
		delete(user.voiceMirrorConfirmed, member)
	}
}

// discordVoiceAudienceIsPrivate includes pending invitations: an invitee can
// join without another action by the bridge after the audience check.
func (portal *Portal) discordVoiceAudienceIsPrivate(owner id.UserID) bool {
	members, err := portal.MainIntent().Members(portal.MXID)
	if err != nil {
		return false
	}
	ownerJoined := false
	for _, member := range members.Chunk {
		if member == nil || member.StateKey == nil {
			return false
		}
		membership, ok := member.Content.Raw["membership"].(string)
		if !ok {
			return false
		}
		if membership != "join" && membership != "invite" {
			continue
		}
		mxid := id.UserID(*member.StateKey)
		if mxid == owner {
			if membership == "join" {
				ownerJoined = true
			}
			continue
		}
		if _, puppet := portal.bridge.ParsePuppetMXID(mxid); !puppet && mxid != portal.bridge.Bot.UserID {
			return false
		}
	}
	return ownerJoined
}

func (portal *Portal) ensureDiscordVoiceGhostJoined(intent *appservice.IntentAPI) error {
	return intent.EnsureJoined(portal.MXID, appservice.EnsureJoinedParams{BotOverride: portal.MainIntent().Client})
}
