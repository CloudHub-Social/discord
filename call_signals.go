package main

import (
	"encoding/json"
	"html"
	"sync"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type discordCallAlert struct{ messageID string }

func discordCallAlertContent(channelID, messageID string, recipient id.UserID) *event.MessageEventContent {
	link := discordMessageURL("", channelID, messageID)
	if link == "" || recipient == "" {
		return nil
	}
	return &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          "Incoming Discord call. Open in Discord: " + link,
		Format:        event.FormatHTML,
		FormattedBody: `Incoming Discord call. <a href="` + html.EscapeString(link) + `">Open in Discord</a>`,
		Mentions:      &event.Mentions{UserIDs: []id.UserID{recipient}},
	}
}

func (portal *Portal) handleDiscordCallAlert(user *User, alert *discordCallAlert) {
	if portal.Type != discordgo.ChannelTypeDM && portal.Type != discordgo.ChannelTypeGroupDM {
		return
	}
	if portal.Encrypted && portal.bridge.Crypto == nil {
		portal.log.Warn().Msg("Cannot send Discord call alert without room encryption")
		return
	}
	if !user.callRings.isRinging(portal.Key.ChannelID, alert.messageID) {
		return
	}
	content := discordCallAlertContent(portal.Key.ChannelID, alert.messageID, user.MXID)
	if content == nil {
		return
	}
	if _, err := portal.sendMatrixMessage(portal.MainIntent(), event.EventMessage, content, nil, 0); err != nil {
		portal.log.Err(err).Msg("Failed to send Discord call alert")
	}
}

func (tracker *callRingTracker) isRinging(channelID, messageID string) bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.active[channelID] == messageID && messageID != ""
}

// discordCallSignal represents the private, user-session gateway payload. A
// missing ringing list is different from an explicitly empty list.
type discordCallSignal struct {
	ChannelID   string    `json:"channel_id"`
	GuildID     string    `json:"guild_id"`
	MessageID   string    `json:"message_id"`
	Ringing     *[]string `json:"ringing"`
	Unavailable bool      `json:"unavailable"`
}

// callRingTracker keeps only calls currently ringing this account. Gateway
// sequence numbers prevent older asynchronous dispatches reversing newer state.
type callRingTracker struct {
	mu           sync.Mutex
	generation   uint64
	epoch        uint64
	lastSequence map[string]int64
	active       map[string]string
}

func (tracker *callRingTracker) accept(generation, epoch uint64, sequence int64, kind string, raw json.RawMessage, self string) (string, string, bool) {
	var call discordCallSignal
	if json.Unmarshal(raw, &call) != nil || call.ChannelID == "" || !isNumber(call.ChannelID) || call.GuildID != "" || self == "" {
		return "", "", false
	}
	if kind != "CALL_CREATE" && kind != "CALL_UPDATE" && kind != "CALL_DELETE" {
		return "", "", false
	}
	if kind != "CALL_DELETE" && (call.MessageID == "" || !isNumber(call.MessageID) || call.Ringing == nil) {
		return "", "", false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if generation < tracker.generation || (generation == tracker.generation && epoch < tracker.epoch) {
		return "", "", false
	}
	if generation != tracker.generation || epoch != tracker.epoch {
		tracker.generation, tracker.epoch = generation, epoch
		tracker.lastSequence = make(map[string]int64)
		tracker.active = make(map[string]string)
	}
	if tracker.lastSequence == nil {
		tracker.lastSequence = make(map[string]int64)
	}
	if sequence <= tracker.lastSequence[call.ChannelID] {
		return "", "", false
	}
	tracker.lastSequence[call.ChannelID] = sequence
	if tracker.active == nil {
		tracker.active = make(map[string]string)
	}
	if kind == "CALL_DELETE" || call.Unavailable {
		delete(tracker.active, call.ChannelID)
		return "", "", false
	}
	ringing := false
	for _, id := range *call.Ringing {
		if id == self {
			ringing = true
			break
		}
	}
	if !ringing {
		delete(tracker.active, call.ChannelID)
		return "", "", false
	}
	if tracker.active[call.ChannelID] == call.MessageID {
		return "", "", false
	}
	tracker.active[call.ChannelID] = call.MessageID
	return call.ChannelID, call.MessageID, true
}
