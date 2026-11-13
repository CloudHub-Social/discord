package main

import (
	"encoding/json"
	"time"

	"github.com/bwmarrin/discordgo"
)

// privateCalls is only accessed while voiceSnapshotLock is held. A call create
// is a snapshot; updates to voice sessions are deltas. Keep sessions separate
// so one device disconnecting cannot remove another device's participant.
type privateCalls struct {
	calls     map[string]map[string]discordgo.VoiceState
	complete  map[string]bool
	uncertain map[string]map[string]time.Time
}

func (p *privateCalls) reset() {
	p.calls = make(map[string]map[string]discordgo.VoiceState)
	p.complete = make(map[string]bool)
	p.uncertain = make(map[string]map[string]time.Time)
}
func (p *privateCalls) event(kind string, raw []byte) {
	var call struct {
		ChannelID   string          `json:"channel_id"`
		Unavailable bool            `json:"unavailable"`
		VoiceStates json.RawMessage `json:"voice_states"`
	}
	if json.Unmarshal(raw, &call) != nil || call.ChannelID == "" {
		return
	}
	if p.calls == nil {
		p.reset()
	}
	switch kind {
	case "CALL_CREATE":
		if len(call.VoiceStates) == 0 || string(call.VoiceStates) == "null" {
			return
		}
		var voices []*discordgo.VoiceState
		if json.Unmarshal(call.VoiceStates, &voices) != nil {
			return
		}
		next := make(map[string]discordgo.VoiceState)
		for _, v := range voices {
			if v == nil || v.UserID == "" || v.SessionID == "" || (v.ChannelID != "" && v.ChannelID != call.ChannelID) {
				return
			}
			v.ChannelID = call.ChannelID
			next[v.UserID+"/"+v.SessionID] = *v
		}
		delete(p.uncertain, call.ChannelID)
		p.calls[call.ChannelID] = next
		p.complete[call.ChannelID] = true
	case "CALL_DELETE":
		if call.Unavailable {
			delete(p.complete, call.ChannelID)
			delete(p.uncertain, call.ChannelID)
			delete(p.calls, call.ChannelID)
			return
		}
		delete(p.calls, call.ChannelID)
		delete(p.uncertain, call.ChannelID)
		p.complete[call.ChannelID] = true
	}
}

// A voice departure may be transient. Ask Discord for a fresh call snapshot
// and retain the participant during the handoff grace. If no snapshot arrives,
// the explicit departure takes effect after that grace on the same connection.
const privateVoiceDepartureGrace = 20 * time.Second

func (p *privateCalls) voice(v *discordgo.VoiceState, now time.Time) []string {
	if v == nil || v.GuildID != "" || v.UserID == "" || v.SessionID == "" {
		return nil
	}
	key := v.UserID + "/" + v.SessionID
	var refresh []string
	for channel, voices := range p.calls {
		if _, present := voices[key]; present && channel != v.ChannelID {
			if p.uncertain[channel] == nil {
				p.uncertain[channel] = make(map[string]time.Time)
			}
			if _, pending := p.uncertain[channel][key]; !pending {
				p.uncertain[channel][key] = now.Add(privateVoiceDepartureGrace)
				refresh = append(refresh, channel)
			}
		}
	}
	if v.ChannelID != "" {
		if voices, ok := p.calls[v.ChannelID]; ok {
			// A replacement voice session for the same person cancels the
			// pending departure and supersedes its old session.
			for oldKey := range p.uncertain[v.ChannelID] {
				if voices[oldKey].UserID == v.UserID {
					delete(voices, oldKey)
					delete(p.uncertain[v.ChannelID], oldKey)
				}
			}
			voices[key] = *v
		}
	}
	return refresh
}

// Snapshot requests are not always answered by Discord for an already
// connected call. A concrete departure on the same stable gateway connection
// becomes effective after a short handoff grace, unless a replacement session
// or authoritative call snapshot arrived in the meantime.
func (p *privateCalls) snapshot(observed map[voiceMembership]struct{}, known map[string]bool, now time.Time) {
	for channel, pending := range p.uncertain {
		for key, deadline := range pending {
			if !now.Before(deadline) {
				delete(p.calls[channel], key)
				delete(pending, key)
			}
		}
	}
	for channel := range p.complete {
		if len(p.uncertain[channel]) == 0 {
			known[channel] = true
		}
	}
	for channel, voices := range p.calls {
		for _, v := range voices {
			observed[voiceMembership{channelID: channel, userID: v.UserID}] = struct{}{}
		}
	}
}
