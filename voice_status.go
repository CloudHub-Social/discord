package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridge/commands"
)

type voiceStatusEntry struct{ user, channel, channelID string }

// snapshotVoiceStatus copies only data for one requested guild while holding
// discordgo's state lock; the cache may be incomplete for a user session.
func snapshotVoiceStatus(state *discordgo.State, guildID, userID string) ([]voiceStatusEntry, bool) {
	if state == nil || guildID == "" || !isNumber(guildID) || userID == "" {
		return nil, false
	}
	state.RLock()
	var guild *discordgo.Guild
	for _, candidate := range state.Guilds {
		if candidate != nil && candidate.ID == guildID {
			guild = candidate
			break
		}
	}
	if guild == nil || guild.Unavailable {
		state.RUnlock()
		return nil, false
	}
	channels := make(map[string]string)
	for _, channel := range guild.Channels {
		if channel != nil {
			channels[channel.ID] = channel.Name
		}
	}
	members := make(map[string]string)
	for _, member := range guild.Members {
		if member != nil && member.User != nil {
			name := member.Nick
			if name == "" {
				name = member.User.Username
			}
			members[member.User.ID] = name
		}
	}
	result := make([]voiceStatusEntry, 0, len(guild.VoiceStates))
	for _, voice := range guild.VoiceStates {
		if voice == nil || voice.ChannelID == "" || voice.UserID == "" {
			continue
		}
		// A missing channel is not evidence of visibility. Avoid exposing it.
		channel, allowed := channels[voice.ChannelID]
		if !allowed {
			continue
		}
		name := members[voice.UserID]
		if voice.Member != nil {
			if voice.Member.Nick != "" {
				name = voice.Member.Nick
			} else if voice.Member.User != nil && name == "" {
				name = voice.Member.User.Username
			}
		}
		if name == "" {
			name = "Discord user " + voice.UserID
		}
		result = append(result, voiceStatusEntry{user: name, channel: channel, channelID: voice.ChannelID})
	}
	state.RUnlock()
	visible := result[:0]
	for _, entry := range result {
		permissions, err := state.UserChannelPermissions(userID, entry.channelID)
		if err == nil && permissions&discordgo.PermissionViewChannel != 0 {
			visible = append(visible, entry)
		}
	}
	result = visible
	sort.Slice(result, func(i, j int) bool {
		if result[i].channel == result[j].channel {
			return result[i].user < result[j].user
		}
		return result[i].channel < result[j].channel
	})
	return result, true
}

var cmdVoiceStatus = &commands.FullHandler{
	Func: wrapCommand(fnVoiceStatus), Name: "voice-status", RequiresLogin: true,
	Help: commands.HelpMeta{Section: HelpSectionPortalManagement, Description: "Show cached Discord voice activity in the private management room", Args: "<_guild ID_>"},
}

func fnVoiceStatus(ce *WrappedCommandEvent) {
	if ce.User.ManagementRoom == "" || ce.RoomID != ce.User.ManagementRoom {
		ce.Reply("Use this command in your private bridge management room.")
		return
	}
	if !ce.Bridge.Config.Bridge.DiscordVoiceStatus {
		ce.Reply("Discord voice status is disabled.")
		return
	}
	if len(ce.Args) != 1 || ce.Args[0] == "" || !isNumber(ce.Args[0]) {
		ce.Reply("Usage: `$cmdprefix voice-status <guild ID>`")
		return
	}
	ce.User.Lock()
	session, discordID := ce.User.Session, ce.User.DiscordID
	ce.User.Unlock()
	if session == nil || session.State == nil || !ce.User.Connected() {
		ce.Reply("Discord is not connected.")
		return
	}
	entries, known := snapshotVoiceStatus(session.State, ce.Args[0], discordID)
	if !known {
		ce.Reply("That server is not available in the Discord session cache.")
		return
	}
	if len(entries) == 0 {
		ce.Reply("No voice activity is currently known. Discord may not provide a complete voice-state snapshot.")
		return
	}
	var lines []string
	for _, entry := range entries {
		lines = append(lines, fmt.Sprintf("• %s — %s", safeVoiceName(entry.user), safeVoiceName(entry.channel)))
	}
	ce.Reply("Known Discord voice activity (the session cache may be incomplete):\n%s", strings.Join(lines, "\n"))
}

func safeVoiceName(name string) string {
	name = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\\", "\\\\", "*", "\\*", "_", "\\_", "`", "\\`", "[", "\\[", "]", "\\]", "@", "@\u200b", "\n", " ", "\r", " ").Replace(name)
	if name == "" {
		return "(unnamed)"
	}
	return name
}
