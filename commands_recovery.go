package main

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/bridge/commands"
	"maunium.net/go/mautrix/id"
)

var cmdResyncMessage = &commands.FullHandler{
	Func:          wrapCommand(fnResyncMessage),
	Name:          "resync-message",
	RequiresAdmin: true,
	Help: commands.HelpMeta{
		Section:     HelpSectionPortalManagement,
		Description: "Recover a missed Discord edit for one existing bridged message (management room only).",
		Args:        "<Matrix room ID> <Discord message ID>",
	},
}

func fnResyncMessage(ce *WrappedCommandEvent) {
	ce.User.Lock()
	managementRoom := ce.User.ManagementRoom
	ce.User.Unlock()
	if ce.User.PermissionLevel < bridgeconfig.PermissionLevelAdmin || managementRoom == "" || ce.RoomID != managementRoom {
		ce.Reply("Message recovery is available only to bridge administrators in their own management room.")
		return
	}
	if len(ce.Args) != 2 {
		ce.Reply("**Usage**: `$cmdprefix resync-message <Matrix room ID> <Discord message ID>`")
		return
	}
	result, err := ce.Bridge.recoverMessageEdit(context.Background(), ce.User, ce.RoomID, id.RoomID(ce.Args[0]), ce.Args[1])
	if err != nil {
		ce.Reply("Message recovery failed: %s", err)
	} else if result == "queued" {
		ce.Reply("The current Discord edit was queued through the normal bridge writer. Delivery is not yet confirmed.")
	} else {
		ce.Reply("No recovery was queued: %s.", result)
	}
}

// recoverMessageEdit accepts identifiers, never replacement content. It fetches
// one exact known message as the requesting account and hands it to the normal
// portal writer, preserving encryption, formatting, sender identity and dedup.
func (br *DiscordBridge) recoverMessageEdit(ctx context.Context, user *User, commandRoom, roomID id.RoomID, messageID string) (string, error) {
	if user.PermissionLevel < bridgeconfig.PermissionLevelAdmin {
		return "", errors.New("administrator permission required")
	}
	parsedID, err := strconv.ParseUint(messageID, 10, 64)
	if err != nil || parsedID == 0 || len(messageID) > 20 || !isNumber(messageID) {
		return "", errors.New("invalid Discord message ID")
	}
	user.Lock()
	sess := user.Session
	managementRoom := user.ManagementRoom
	user.Unlock()
	if managementRoom == "" || commandRoom != managementRoom {
		return "", errors.New("recovery requires your own management room")
	}
	if sess == nil {
		return "", errors.New("Discord account is not connected")
	}
	portal := br.GetPortalByMXID(roomID)
	if portal == nil || portal.MXID == "" {
		return "", errors.New("room is not an existing bridged portal")
	}
	existing := br.DB.Message.GetFirstByDiscordID(portal.Key, messageID)
	if existing == nil || existing.MXID == "" {
		return "", errors.New("message has no existing bridge mapping")
	}
	channelID := portal.Key.ChannelID
	if existing.ThreadID != "" {
		channelID = existing.ThreadID
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	opts := portal.RefererOptIfUser(sess, existing.ThreadID)
	opts = append(opts, discordgo.WithContext(ctx), discordgo.WithRetryOnRatelimit(false), discordgo.WithRestRetries(0))
	messages, err := sess.ChannelMessages(channelID, 1, "", "", messageID, opts...)
	if err != nil {
		return "", errors.New("could not fetch the current Discord message")
	}
	if len(messages) != 1 || messages[0] == nil {
		return "", errors.New("Discord did not return the exact message")
	}
	msg := messages[0]
	if msg.ID != messageID || msg.ChannelID != channelID || msg.Author == nil || msg.Author.ID != existing.SenderID {
		return "", errors.New("Discord message identity does not match the bridge mapping")
	}
	if msg.EditedTimestamp == nil {
		return "not edited on Discord", nil
	}
	if !msg.EditedTimestamp.After(existing.EditTimestamp) {
		return "edit already applied", nil
	}
	user.Lock()
	defer user.Unlock()
	if user.Session != sess || ctx.Err() != nil {
		return "", errors.New("Discord session changed or the recovery request expired")
	}
	select {
	case portal.discordMessages <- portalDiscordMessage{user: user, msg: &discordgo.MessageUpdate{Message: msg}}:
		return "queued", nil
	default:
		return "", errors.New("portal message queue is full; no recovery was queued")
	}
}
