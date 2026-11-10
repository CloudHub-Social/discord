package main

import (
	"context"
	"time"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/event"
)

// Accessed under pollRefreshLock because workers may time out while queuing.
type pollRefreshState struct {
	generation uint64
	inFlight   bool
}

func retainRefreshedPoll(cached, refreshed *discordgo.Message) {
	if cached != nil && refreshed != nil && refreshed.Poll != nil {
		cached.Poll = refreshed.Poll
	}
}

// queuePollRefresh coalesces bursts of vote events into authoritative, bounded
// reads instead of counting events, which may be duplicated or lost.
func (portal *Portal) queuePollRefresh(user *User, messageID string, vote bool) {
	if user == nil || messageID == "" || !isNumber(messageID) {
		return
	}
	existing := portal.bridge.DB.Message.GetFirstByDiscordID(portal.Key, messageID)
	if existing == nil {
		return
	}
	user.Lock()
	session := user.Session
	user.Unlock()
	if session == nil {
		return
	}

	portal.pollRefreshLock.Lock()
	if portal.pollRefreshState == nil {
		portal.pollRefreshState = make(map[string]*pollRefreshState)
	}
	if portal.pollRefreshSlots == nil {
		portal.pollRefreshSlots = make(chan struct{}, 8)
	}
	state := portal.pollRefreshState[messageID]
	if state == nil {
		state = &pollRefreshState{}
		portal.pollRefreshState[messageID] = state
	}
	if vote {
		state.generation++
	}
	if state.inFlight {
		portal.pollRefreshLock.Unlock()
		return
	}
	select {
	case portal.pollRefreshSlots <- struct{}{}:
	default:
		delete(portal.pollRefreshState, messageID)
		portal.pollRefreshLock.Unlock()
		portal.log.Warn().Msg("Could not schedule Discord poll refresh")
		return
	}
	state.inFlight = true
	generation := state.generation
	slots := portal.pollRefreshSlots
	channelID, senderID, threadID := existing.DiscordProtoChannelID(), existing.SenderID, existing.ThreadID
	portal.pollRefreshLock.Unlock()
	go func() {
		defer func() { <-slots }()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		opts := portal.RefererOptIfUser(session, threadID)
		opts = append(opts, discordgo.WithContext(ctx), discordgo.WithRetryOnRatelimit(false), discordgo.WithRestRetries(0))
		var message *discordgo.Message
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			message, err = fetchGalleryMessage(session, channelID, messageID, senderID, opts...)
			if err == nil || attempt == 2 {
				break
			}
			delay := componentRefreshRetryDelay(err, attempt)
			if delay == 0 {
				break
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				err = ctx.Err()
			}
			if ctx.Err() != nil {
				break
			}
		}
		user.Lock()
		sameSession := user.Session == session
		user.Unlock()
		if err != nil || !sameSession {
			message = nil
		}
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		select {
		case portal.discordMessages <- portalDiscordMessage{user: user, msg: &discordPollSnapshot{messageID: messageID, message: message, generation: generation}}:
		case <-timer.C:
			portal.pollRefreshLock.Lock()
			delete(portal.pollRefreshState, messageID)
			portal.pollRefreshLock.Unlock()
			portal.log.Warn().Msg("Could not queue Discord poll refresh")
		}
	}()
}

func (portal *Portal) finishPollRefresh(user *User, snapshot *discordPollSnapshot) {
	portal.pollRefreshLock.Lock()
	state := portal.pollRefreshState[snapshot.messageID]
	if state == nil {
		portal.pollRefreshLock.Unlock()
		return
	}
	changed := state.generation != snapshot.generation
	if changed {
		state.inFlight = false
	} else {
		delete(portal.pollRefreshState, snapshot.messageID)
	}
	portal.pollRefreshLock.Unlock()
	if changed {
		defer portal.queuePollRefresh(user, snapshot.messageID, false)
	}
	if snapshot.message == nil || snapshot.message.Poll == nil {
		portal.log.Warn().Msg("Discord poll refresh did not return a poll")
		return
	}
	if portal.Encrypted && portal.bridge.Crypto == nil {
		portal.log.Warn().Msg("Cannot update Discord poll without room encryption")
		return
	}
	existing := portal.bridge.DB.Message.GetByDiscordID(portal.Key, snapshot.messageID)
	for _, part := range existing {
		if part.AttachmentID != "" {
			continue
		}
		if !part.EditTimestamp.IsZero() && !galleryMessageIsCurrent(snapshot.message, &discordgo.Message{EditedTimestamp: &part.EditTimestamp}) {
			return
		}
		if snapshot.message.Author == nil || snapshot.message.Author.ID != part.SenderID {
			return
		}
		puppet := portal.bridge.GetPuppetByID(part.SenderID)
		intent := puppet.IntentFor(portal)
		converted := portal.convertDiscordTextMessage(portal.log.WithContext(context.Background()), intent, snapshot.message)
		if converted == nil {
			return
		}
		puppet.addWebhookMeta(converted, snapshot.message)
		puppet.addMemberMeta(converted, snapshot.message)
		converted.Content.Mentions = &event.Mentions{}
		converted.Content.SetEdit(part.MXID)
		var extra map[string]interface{}
		if converted.Extra != nil {
			extra = map[string]interface{}{"m.new_content": converted.Extra}
		}
		if _, err := portal.sendMatrixMessage(intent, event.EventMessage, converted.Content, extra, 0); err != nil {
			portal.log.Err(err).Msg("Failed to send refreshed Discord poll")
		} else if portal.recentMessages != nil {
			if cached, ok := portal.recentMessages.Get(snapshot.messageID); ok {
				retainRefreshedPoll(cached, snapshot.message)
			}
		}
		return
	}
}
