// mautrix-discord - A Matrix-Discord puppeting bridge.
// Copyright (C) 2026
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-discord/database"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// The names and item shapes match the Ruma revision pinned by Sable v2.
const galleryMessageType event.MessageType = "dm.filament.gallery"
const galleryAttachmentID = "!msc4274-gallery"
const gallerySpoilerKey = "page.codeberg.everypizza.msc4193.spoiler"

type galleryItem struct {
	Content *event.MessageEventContent
	Spoiler bool
}

// decodeGallery rejects the entire gallery on unknown or malformed media. This
// prevents a successful bridge receipt for a message with missing attachments.
func decodeGallery(raw map[string]any) ([]galleryItem, error) {
	value, ok := raw["itemtypes"]
	if !ok {
		return nil, fmt.Errorf("gallery has no itemtypes")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("invalid gallery items: %w", err)
	}
	var objects []map[string]json.RawMessage
	if err = json.Unmarshal(data, &objects); err != nil {
		return nil, fmt.Errorf("invalid gallery items: %w", err)
	}
	if len(objects) == 0 || len(objects) > 10 {
		return nil, fmt.Errorf("gallery has %d items; Discord supports up to 10", len(objects))
	}
	items := make([]galleryItem, 0, len(objects))
	for index, object := range objects {
		var itemType event.MessageType
		if err = json.Unmarshal(object["itemtype"], &itemType); err != nil {
			return nil, fmt.Errorf("gallery item %d has invalid itemtype", index)
		}
		switch itemType {
		case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
		default:
			return nil, fmt.Errorf("gallery item %d has unsupported itemtype %q", index, itemType)
		}
		var body string
		if err = json.Unmarshal(object["body"], &body); err != nil {
			return nil, fmt.Errorf("gallery item %d has invalid body", index)
		}
		encoded, _ := json.Marshal(object)
		content := &event.MessageEventContent{}
		if err = json.Unmarshal(encoded, content); err != nil {
			return nil, fmt.Errorf("gallery item %d is invalid: %w", index, err)
		}
		content.MsgType = itemType
		if content.URL == "" && (content.File == nil || content.File.URL == "") {
			return nil, fmt.Errorf("gallery item %d has no media source", index)
		}
		var spoiler bool
		if rawSpoiler, ok := object[gallerySpoilerKey]; ok {
			if err = json.Unmarshal(rawSpoiler, &spoiler); err != nil {
				return nil, fmt.Errorf("gallery item %d has invalid spoiler", index)
			}
		}
		items = append(items, galleryItem{Content: content, Spoiler: spoiler})
	}
	return items, nil
}

func encodeGalleryItems(parts []*ConvertedMessage) ([]map[string]any, error) {
	items := make([]map[string]any, 0, len(parts))
	for index, part := range parts {
		if part == nil || part.Content == nil {
			return nil, fmt.Errorf("gallery item %d is missing", index)
		}
		switch part.Content.MsgType {
		case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
		default:
			return nil, fmt.Errorf("gallery item %d is not media", index)
		}
		if part.Content.URL == "" && (part.Content.File == nil || part.Content.File.URL == "") {
			return nil, fmt.Errorf("gallery item %d has no media source", index)
		}
		data, err := json.Marshal(part.Content)
		if err != nil {
			return nil, err
		}
		var object map[string]any
		if err = json.Unmarshal(data, &object); err != nil {
			return nil, err
		}
		delete(object, "msgtype")
		object["itemtype"] = string(part.Content.MsgType)
		for key, value := range part.Extra {
			object[key] = value
		}
		items = append(items, object)
	}
	return items, nil
}

// combineDiscordGallery leaves the original parts intact on any failure.
func combineDiscordGallery(parts []*ConvertedMessage, count int) []*ConvertedMessage {
	start := 0
	var caption *ConvertedMessage
	if len(parts) > 0 && parts[0].AttachmentID == "" {
		caption = parts[0]
		start = 1
	}
	if count < 2 || len(parts)-start != count {
		return parts
	}
	items, err := encodeGalleryItems(parts[start:])
	if err != nil {
		return parts
	}
	content := &event.MessageEventContent{MsgType: galleryMessageType, Body: "Attachments"}
	extra := map[string]any{"itemtypes": items}
	if caption != nil {
		content.Body, content.Format, content.FormattedBody = caption.Content.Body, caption.Content.Format, caption.Content.FormattedBody
		for key, value := range caption.Extra {
			extra[key] = value
		}
	}
	return []*ConvertedMessage{{AttachmentID: galleryAttachmentID, Type: event.EventMessage, Content: content, Extra: extra}}
}

// Additional Discord-only media stays reachable when an existing gallery is
// edited to include an embed or sticker that cannot share its event mapping.
func appendDiscordExtraLink(part *ConvertedMessage, rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || strings.Contains(part.Content.Body, rawURL) {
		return false
	}
	escaped := html.EscapeString(rawURL)
	link := `<a href="` + escaped + `">` + escaped + `</a>`
	if part.Content.Body == "" {
		part.Content.Body = rawURL
		part.Content.Format = event.FormatHTML
		part.Content.FormattedBody = link
	} else {
		part.Content.EnsureHasHTML()
		part.Content.Body += "\n" + rawURL
		part.Content.FormattedBody += "<br/>" + link
	}
	return true
}

func appendDiscordExtraPart(parts []*ConvertedMessage, rawURL string) []*ConvertedMessage {
	if len(parts) > 0 && (parts[0].AttachmentID == galleryAttachmentID || parts[0].AttachmentID == "") {
		appendDiscordExtraLink(parts[0], rawURL)
		return parts
	}
	caption := &ConvertedMessage{Type: event.EventMessage, Content: &event.MessageEventContent{MsgType: event.MsgText}}
	if !appendDiscordExtraLink(caption, rawURL) {
		return parts
	}
	return append([]*ConvertedMessage{caption}, parts...)
}

// Stable Matrix transaction IDs make a retried Discord gallery create/edit
// resolve to the same event even if the first response was lost.
func (portal *Portal) sendDiscordGalleryEvent(intent *appservice.IntentAPI, content *event.MessageEventContent, extra map[string]any, timestamp int64, discordID, version string) (*mautrix.RespSendEvent, error) {
	if portal.Encrypted && portal.bridge.Crypto == nil {
		return nil, fmt.Errorf("cannot send gallery to encrypted room without crypto")
	}
	wrapped := event.Content{Parsed: content, Raw: extra}
	eventType, err := portal.encrypt(intent, &wrapped, event.EventMessage)
	if err != nil {
		return nil, err
	}
	if err = intent.EnsureJoined(portal.MXID); err != nil {
		return nil, err
	}
	intent.AddDoublePuppetValue(&wrapped)
	_, _ = intent.UserTyping(portal.MXID, false, 0)
	txn := sha256.Sum256([]byte(portal.MXID.String() + "\x00" + discordID + "\x00" + version))
	return intent.Client.SendMessageEvent(portal.MXID, eventType, &wrapped, mautrix.ReqSendEvent{Timestamp: timestamp, TransactionID: fmt.Sprintf("discord-gallery-%x", txn)})
}

// A stable nonce lets the gateway reconcile a send whose REST response was lost.
func galleryNonce(eventID id.EventID) string {
	digest := sha256.Sum256([]byte(eventID))
	return strconv.FormatUint(binary.BigEndian.Uint64(digest[:8])&(1<<63-1), 10)
}

func (portal *Portal) reconcileGalleryEcho(msg *discordgo.Message) bool {
	if msg.Author == nil || msg.Nonce == "" {
		return false
	}
	pending, err := portal.bridge.DB.Message.PendingGalleryByNonce(portal.Key, string(msg.Nonce), msg.Author.ID)
	if err != nil {
		portal.log.Err(err).Msg("Failed to check gallery send reservation")
		return true
	}
	if pending == nil {
		return false
	}
	expectedChannel := portal.Key.ChannelID
	if pending.ThreadID != "" {
		expectedChannel = pending.ThreadID
	}
	if msg.ChannelID != expectedChannel {
		return false
	}
	if portal.bridge.DB.Message.GetByMXID(portal.Key, pending.MXID) == nil {
		ts, err := discordgo.SnowflakeTimestamp(msg.ID)
		if err != nil {
			portal.log.Err(err).Msg("Invalid Discord ID in gallery echo")
			return true
		}
		portal.markMessageHandled(msg.ID, pending.SenderID, ts, pending.ThreadID, pending.SenderMXID, []database.MessagePart{{AttachmentID: galleryAttachmentID, MXID: pending.MXID}})
	}
	if err = portal.bridge.DB.Message.ClearPendingGallery(pending.MXID); err != nil {
		portal.log.Err(err).Msg("Failed to clear gallery send reservation")
	}
	portal.sendStatusEvent(pending.MXID, nil)
	return true
}

// Ruma parses the fallback message before applying m.new_content, so both
// gallery payloads must carry their itemtypes.
func galleryEditExtra(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	outer := make(map[string]any, len(extra)+1)
	for key, value := range extra {
		outer[key] = value
	}
	outer["m.new_content"] = extra
	return outer
}

// fetchGalleryMessage verifies the message identity before reconstructing an edit.
func fetchGalleryMessage(sess *discordgo.Session, channelID, messageID, senderID string, opts ...discordgo.RequestOption) (*discordgo.Message, error) {
	var messages []*discordgo.Message
	var err error
	if sess.IsUser {
		messages, err = sess.ChannelMessages(channelID, 1, "", "", messageID, opts...)
	} else {
		var msg *discordgo.Message
		msg, err = sess.ChannelMessage(channelID, messageID, opts...)
		messages = []*discordgo.Message{msg}
	}
	if err != nil {
		return nil, err
	}
	return selectGalleryMessage(messages, channelID, messageID, senderID)
}

func selectGalleryMessage(messages []*discordgo.Message, channelID, messageID, senderID string) (*discordgo.Message, error) {
	if len(messages) != 1 || messages[0] == nil || messages[0].ID != messageID || messages[0].ChannelID != channelID || messages[0].Author == nil || messages[0].Author.ID != senderID {
		return nil, fmt.Errorf("Discord message identity does not match the bridge mapping")
	}
	return messages[0], nil
}

// A fetch can race a subsequent edit. Never publish a version older than the update.
func galleryMessageIsCurrent(full, update *discordgo.Message) bool {
	if full == nil || update == nil {
		return false
	}
	return update.EditedTimestamp == nil || (full.EditedTimestamp != nil && !full.EditedTimestamp.Before(*update.EditedTimestamp))
}

// Discord gateway updates may omit fields, so reconstruct a gallery only from
// the complete current message instead of treating absent attachments as removals.
func (portal *Portal) handleDiscordGalleryUpdate(ctx context.Context, user *User, update *discordgo.Message, existing *database.Message) {
	log := zerolog.Ctx(ctx)
	if user == nil || user.Session == nil {
		log.Warn().Msg("Cannot refresh gallery without Discord session")
		return
	}
	channelID := existing.DiscordProtoChannelID()
	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	opts := portal.RefererOptIfUser(user.Session, existing.ThreadID)
	opts = append(opts, discordgo.WithContext(fetchCtx), discordgo.WithRetryOnRatelimit(false), discordgo.WithRestRetries(0))
	full, err := fetchGalleryMessage(user.Session, channelID, update.ID, existing.SenderID, opts...)
	if err != nil || full == nil || full.Author == nil {
		log.Err(err).Msg("Failed to refresh edited Discord gallery")
		return
	}
	if !galleryMessageIsCurrent(full, update) {
		log.Warn().Msg("Discord gallery refresh is older than the received update")
		return
	}
	if full.Author.ID == portal.RelayWebhookID {
		return
	}
	puppet := portal.bridge.GetPuppetByID(full.Author.ID)
	intent := puppet.IntentFor(portal)
	parts := portal.convertDiscordMessageWithGallery(ctx, puppet, intent, full, true)
	var converted *ConvertedMessage
	switch {
	case len(full.Attachments) >= 2:
		if len(parts) != 1 || parts[0].AttachmentID != galleryAttachmentID {
			log.Warn().Msg("Cannot safely represent edited Discord gallery")
			return
		}
		converted = parts[0]
	case len(full.Attachments) == 1:
		if len(parts) == 2 && parts[0].AttachmentID == "" {
			caption, media := parts[0], parts[1]
			if media.Content.MsgType == event.MsgNotice {
				return
			}
			if media.Content.FileName == "" {
				media.Content.FileName = media.Content.Body
			}
			media.Content.Body, media.Content.Format, media.Content.FormattedBody = caption.Content.Body, caption.Content.Format, caption.Content.FormattedBody
			converted = media
		} else if len(parts) == 1 && parts[0].Content.MsgType != event.MsgNotice {
			converted = parts[0]
		}
	case len(full.Attachments) == 0:
		if len(parts) == 1 {
			converted = parts[0]
		} else if len(parts) == 0 {
			converted = &ConvertedMessage{Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "Attachments removed"}}
		}
	}
	if converted == nil {
		log.Warn().Msg("Cannot safely represent edited Discord gallery")
		return
	}
	converted.Content.Mentions = portal.convertDiscordMentions(full, false)
	converted.Content.SetEdit(existing.MXID)
	converted.Content.Mentions = &event.Mentions{}
	converted.Extra = galleryEditExtra(converted.Extra)
	var timestamp int64
	if full.EditedTimestamp != nil {
		timestamp = full.EditedTimestamp.UnixMilli()
	}
	if full.EditedTimestamp == nil {
		log.Warn().Msg("Cannot deduplicate gallery edit without edit timestamp")
		return
	}
	resp, err := portal.sendDiscordGalleryEvent(intent, converted.Content, converted.Extra, timestamp, full.ID, full.EditedTimestamp.UTC().Format(time.RFC3339Nano))
	if err != nil {
		log.Err(err).Msg("Failed to send gallery edit")
		return
	}
	portal.sendDeliveryReceipt(resp.EventID)
	if full.EditedTimestamp != nil {
		existing.UpdateEditTimestamp(*full.EditedTimestamp)
	}
}

// prepareMatrixGallery downloads and validates every item before submitting a
// message, so an unsupported or oversized item cannot be silently omitted.
func (portal *Portal) prepareMatrixGallery(sender *User, channelID, threadID string, items []galleryItem, useCDN bool) (*discordgo.MessageSend, error) {
	req := &discordgo.MessageSend{}
	const maxGalleryItemSize = 100 << 20
	maxTotal := int64(maxGalleryItemSize)
	if !useCDN {
		maxTotal = 24 << 20
	} // Discord limits multipart requests to 25 MiB including overhead.
	var total int64
	for i, item := range items {
		content := item.Content
		if content.Info != nil && content.Info.Size > maxGalleryItemSize {
			return nil, fmt.Errorf("gallery item %d exceeds bridge size limit", i)
		}
		data, err := downloadMatrixGalleryItem(portal.MainIntent(), content, maxTotal-total)
		if err != nil {
			return nil, fmt.Errorf("gallery item %d: %w", i, err)
		}
		total += int64(len(data))
		name := content.Body
		description := ""
		if content.FileName != "" {
			name = content.FileName
			if name != content.Body {
				description = content.Body
			}
		}
		name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
		if name == "." || name == "/" || name == "" {
			name = fmt.Sprintf("attachment-%d", i+1)
		}
		if item.Spoiler && !strings.HasPrefix(name, "SPOILER_") {
			name = "SPOILER_" + name
		}
		mime := "application/octet-stream"
		if content.Info != nil && content.Info.MimeType != "" {
			mime = content.Info.MimeType
		}
		att := &discordgo.MessageAttachment{ID: strconv.Itoa(i), Filename: name, Description: description, OriginalContentType: mime}
		if useCDN {
			isClip := false
			prep, err := sender.Session.ChannelAttachmentCreate(channelID, &discordgo.ReqPrepareAttachments{Files: []*discordgo.FilePrepare{{Size: len(data), Name: name, ID: sender.NextDiscordUploadID(), IsClip: &isClip, OriginalContentType: mime}}}, portal.RefererOpt(threadID))
			if err != nil {
				return nil, fmt.Errorf("preparing gallery item %d: %w", i, err)
			}
			if prep == nil || len(prep.Attachments) != 1 || prep.Attachments[0] == nil {
				return nil, fmt.Errorf("invalid upload response for gallery item %d", i)
			}
			att.UploadedFilename = prep.Attachments[0].UploadFilename
			if err = uploadDiscordAttachment(sender.Session.Client, prep.Attachments[0].UploadURL, data); err != nil {
				return nil, fmt.Errorf("uploading gallery item %d: %w", i, err)
			}
		} else {
			req.Files = append(req.Files, &discordgo.File{Name: name, ContentType: mime, Reader: bytes.NewReader(data)})
		}
		req.Attachments = append(req.Attachments, att)
	}
	return req, nil
}

func downloadMatrixGalleryItem(intent *appservice.IntentAPI, content *event.MessageEventContent, limit int64) ([]byte, error) {
	source := content.URL
	if content.File != nil {
		source = content.File.URL
	}
	mxc, err := source.Parse()
	if err != nil {
		return nil, err
	}
	body, err := intent.Download(mxc)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("media exceeds bridge size limit")
	}
	if content.File != nil {
		if err = content.File.DecryptInPlace(data); err != nil {
			return nil, err
		}
	}
	return data, nil
}

func (portal *Portal) handleMatrixGalleryEdit(sender *User, evt *event.Event, original *database.Message, content *event.MessageEventContent, raw map[string]any, webhook bool) {
	if original.AttachmentID != galleryAttachmentID {
		go portal.sendMessageMetrics(evt, fmt.Errorf("converting an existing non-gallery message to a gallery is unsupported"), "Invalid gallery edit in")
		return
	}
	var items []galleryItem
	var err error
	switch content.MsgType {
	case galleryMessageType:
		items, err = decodeGallery(raw)
	case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
		items = []galleryItem{{Content: content, Spoiler: raw[gallerySpoilerKey] == true}}
	case event.MsgText, event.MsgNotice:
	default:
		err = fmt.Errorf("unsupported gallery edit type %q", content.MsgType)
	}
	if err != nil {
		go portal.sendMessageMetrics(evt, err, "Invalid gallery edit in")
		return
	}
	channelID := original.DiscordProtoChannelID()
	req := &discordgo.MessageSend{}
	if len(items) > 0 {
		req, err = portal.prepareMatrixGallery(sender, channelID, original.ThreadID, items, portal.bridge.Config.Bridge.UseDiscordCDNUpload && !webhook && sender.Session.IsUser)
		if err != nil {
			go portal.sendMessageMetrics(evt, err, "Error preparing gallery edit in")
			return
		}
	}
	caption := content
	if len(items) == 1 && content.MsgType != galleryMessageType && content.FileName == "" {
		caption = &event.MessageEventContent{MsgType: event.MsgText, Body: ""}
	}
	discordContent, allowedMentions := portal.parseMatrixHTML(caption, parseAllowedLinkPreviews(raw))
	// A text replacement may be a caption-only edit from a client that doesn't
	// retain gallery itemtypes. Omit attachments so Discord preserves them.
	attachments := galleryEditAttachments(req, len(items) > 0)
	var result *discordgo.Message
	if webhook {
		result, err = relayClient.WebhookMessageEdit(portal.RelayWebhookID, portal.RelayWebhookSecret, original.DiscordID, &discordgo.WebhookEdit{Content: &discordContent, Files: req.Files, Attachments: attachments, AllowedMentions: allowedMentions})
	} else {
		edit := &discordgo.MessageEdit{ID: original.DiscordID, Channel: channelID, Content: &discordContent, Files: req.Files, Attachments: attachments}
		result, err = sender.Session.ChannelMessageEditComplex(edit, portal.RefererOptIfUser(sender.Session, original.ThreadID)...)
	}
	sender.handlePossible40002(err)
	go portal.sendMessageMetrics(evt, err, "Failed to edit gallery")
	if result != nil && result.EditedTimestamp != nil {
		original.UpdateEditTimestamp(*result.EditedTimestamp)
	}
}

// A nil attachment pointer leaves existing Discord media untouched.
func galleryEditAttachments(req *discordgo.MessageSend, replaceMedia bool) *[]*discordgo.MessageAttachment {
	if !replaceMedia {
		return nil
	}
	return &req.Attachments
}
