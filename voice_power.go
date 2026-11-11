package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const discordVoiceEventType = "org.matrix.msc3401.call.member"

// planDiscordVoicePower preserves unknown power-level fields and refuses to
// widen unrelated permissions or override an explicitly restrictive call rule.
func planDiscordVoicePower(original map[string]any, ghost, owner, bot id.UserID, isGhost func(id.UserID) bool) (map[string]any, bool, error) {
	raw, err := json.Marshal(original)
	if err != nil {
		return nil, false, err
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&result); err != nil {
		return nil, false, err
	}
	if result == nil {
		return nil, false, errors.New("missing room power levels")
	}
	number := func(m map[string]any, key string, fallback int) (int, error) {
		v, found := m[key]
		if !found {
			return fallback, nil
		}
		n, ok := v.(json.Number)
		if !ok {
			return 0, fmt.Errorf("invalid %s power level", key)
		}
		level, err := n.Int64()
		if err != nil || int64(int(level)) != level {
			return 0, fmt.Errorf("invalid %s power level", key)
		}
		return int(level), nil
	}
	object := func(m map[string]any, key string) (map[string]any, error) {
		v, found := m[key]
		if !found {
			n := make(map[string]any)
			m[key] = n
			return n, nil
		}
		n, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid %s power map", key)
		}
		return n, nil
	}
	users, err := object(result, "users")
	if err != nil {
		return nil, false, err
	}
	events, err := object(result, "events")
	if err != nil {
		return nil, false, err
	}
	defaultUser, err := number(result, "users_default", 0)
	if err != nil {
		return nil, false, err
	}
	ghostLevel, err := number(users, string(ghost), defaultUser)
	if err != nil {
		return nil, false, err
	}
	stateDefault, err := number(result, "state_default", 50)
	if err != nil {
		return nil, false, err
	}
	required, err := number(events, discordVoiceEventType, stateDefault)
	if err != nil {
		return nil, false, err
	}
	if ghostLevel >= required {
		return result, false, nil
	}
	_, explicitCallRule := events[discordVoiceEventType]
	if defaultUser != 0 || ghostLevel < 0 || ghostLevel > 1 || (explicitCallRule && required != 1) || stateDefault <= 1 {
		return nil, false, errors.New("custom power levels require manual voice configuration")
	}
	// Elevating a ghost from 0 to 1 must not unlock any other operation.
	if ghostLevel == 0 {
		for _, field := range []string{"events_default", "invite", "kick", "ban", "redact"} {
			fallback := 50
			if field == "events_default" {
				fallback = 0
			}
			level, e := number(result, field, fallback)
			if e != nil || level == 1 {
				return nil, false, fmt.Errorf("%s would grant unrelated permission", field)
			}
		}
		for name := range events {
			if name == discordVoiceEventType {
				continue
			}
			level, e := number(events, name, 0)
			if e != nil || level == 1 {
				return nil, false, fmt.Errorf("event %s would grant unrelated permission", name)
			}
		}
		if n, exists := result["notifications"]; exists {
			m, ok := n.(map[string]any)
			if !ok {
				return nil, false, errors.New("invalid notification power map")
			}
			level, e := number(m, "room", 50)
			if e != nil || level == 1 {
				return nil, false, errors.New("notification permission would expand")
			}
		}
	}
	// Do not grant other real users a new ability to write foreign ghost keys.
	for name := range users {
		uid := id.UserID(name)
		level, e := number(users, name, defaultUser)
		if e != nil {
			return nil, false, e
		}
		if uid != owner && uid != bot && !isGhost(uid) && level >= 1 && level < required {
			return nil, false, errors.New("other users would gain voice-state permission")
		}
	}
	events[discordVoiceEventType] = 1
	users[string(ghost)] = 1
	return result, true, nil
}

func (portal *Portal) ensureDiscordVoicePower(ghost, owner id.UserID) error {
	portal.voicePowerLock.Lock()
	defer portal.voicePowerLock.Unlock()
	intent := portal.MainIntent()
	var raw json.RawMessage
	if err := intent.StateEvent(portal.MXID, event.StatePowerLevels, "", &raw); err != nil {
		return err
	}
	var levels map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&levels); err != nil {
		return err
	}
	updated, changed, err := planDiscordVoicePower(levels, ghost, owner, portal.bridge.Bot.UserID, func(uid id.UserID) bool { _, ok := portal.bridge.ParsePuppetMXID(uid); return ok })
	if err != nil || !changed {
		return err
	}
	_, err = intent.SendStateEvent(portal.MXID, event.StatePowerLevels, "", updated)
	return err
}
