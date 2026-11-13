package main

import (
	"encoding/json"

	"github.com/bwmarrin/discordgo"
)

// privateCalls is only accessed while voiceSnapshotLock is held. A call create
// is a snapshot; updates to voice sessions are deltas. Keep sessions separate
// so one device disconnecting cannot remove another device's participant.
type privateCalls struct {
	calls     map[string]map[string]discordgo.VoiceState
	complete  map[string]bool
	uncertain map[string]map[string]bool
}

func (p *privateCalls) reset() {
	p.calls = make(map[string]map[string]discordgo.VoiceState)
	p.complete = make(map[string]bool)
	p.uncertain = make(map[string]map[string]bool)
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
// before changing the participant list. The caller sends opcode 13 for each
// returned channel; a missing response is not evidence of departure.
func (p *privateCalls) voice(v *discordgo.VoiceState) []string {
	if v == nil || v.GuildID != "" || v.UserID == "" || v.SessionID == "" {
		return nil
	}
	key := v.UserID + "/" + v.SessionID
	var refresh []string
	for channel, voices := range p.calls {
		if _, present := voices[key]; present && channel != v.ChannelID {
			if p.uncertain[channel] == nil {
				p.uncertain[channel] = make(map[string]bool)
			}
			p.uncertain[channel][key] = true
			refresh = append(refresh, channel)
		}
	}
	if v.ChannelID != "" {
		if voices, ok := p.calls[v.ChannelID]; ok {
			voices[key] = *v
			delete(p.uncertain[v.ChannelID], key)
		}
	}
	return refresh
}
func (p *privateCalls) snapshot(observed map[voiceMembership]struct{}, known map[string]bool) {
	for channel := range p.complete {
		if len(p.uncertain[channel]) == 0 {
			known[channel] = true
		}
	}
	for channel, voices := range p.calls {
		for key, v := range voices {
			if p.uncertain[channel][key] {
				continue
			}
			observed[voiceMembership{channelID: channel, userID: v.UserID}] = struct{}{}
		}
	}
}
