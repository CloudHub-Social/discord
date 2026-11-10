// mautrix-discord - A Matrix-Discord puppeting bridge.
// Copyright (C) 2026
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
)

type componentMedia struct {
	url, description, id string
	spoiler              bool
}

// Only Discord's media hosts are fetched, including across redirects. Userinfo,
// non-default ports, IP addresses and arbitrary user-supplied hosts are rejected.
func trustedComponentMediaURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" && (strings.EqualFold(u.Hostname(), "cdn.discordapp.com") || strings.EqualFold(u.Hostname(), "media.discordapp.net"))
}

func collectComponentMedia(components []discordgo.MessageComponent) []componentMedia {
	var out []componentMedia
	seen := map[string]bool{}
	add := func(raw string, description *string, spoiler bool) {
		if !trustedComponentMediaURL(raw) {
			return
		}
		u, _ := url.Parse(raw)
		desc := ""
		if description != nil {
			desc = *description
		}
		// Metadata participates in identity, so a spoiler or description edit
		// replaces the old event instead of retaining stale presentation.
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%t", u.EscapedPath(), desc, spoiler)))
		id := fmt.Sprintf("component_%x", sum[:12])
		if seen[id] {
			return
		}
		seen[id] = true
		out = append(out, componentMedia{url: raw, description: desc, id: id, spoiler: spoiler})
	}
	var walk func(discordgo.MessageComponent, bool)
	walk = func(c discordgo.MessageComponent, hidden bool) {
		switch x := c.(type) {
		case *discordgo.ActionsRow:
			for _, child := range x.Components {
				walk(child, hidden)
			}
		case *discordgo.Container:
			for _, child := range x.Components {
				walk(child, hidden || x.Spoiler)
			}
		case *discordgo.Section:
			for _, child := range x.Components {
				walk(child, hidden)
			}
			if x.Accessory != nil {
				walk(x.Accessory, hidden)
			}
		case *discordgo.Thumbnail:
			add(x.Media.URL, x.Description, hidden || x.Spoiler)
		case *discordgo.MediaGallery:
			for _, item := range x.Items {
				add(item.Media.URL, item.Description, hidden || item.Spoiler)
			}
		case *discordgo.FileComponent:
			add(x.File.URL, nil, hidden || x.Spoiler)
		}
	}
	for _, c := range components {
		walk(c, false)
	}
	return out
}

func checkComponentMediaRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || !trustedComponentMediaURL(req.URL.String()) {
		return fmt.Errorf("untrusted component media redirect")
	}
	return nil
}

func (portal *Portal) convertComponentMedia(intent *appservice.IntentAPI, m componentMedia) *ConvertedMessage {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: checkComponentMediaRedirect}
	dbFile, err := portal.bridge.copyAttachmentToMatrixWithClient(intent, m.url, portal.Encrypted, AttachmentMeta{AttachmentID: m.id}, client)
	if err != nil {
		return &ConvertedMessage{AttachmentID: m.id, Type: event.EventMessage, Content: portal.createMediaFailedMessage(fmt.Errorf("could not copy Discord card media"))}
	}
	filename := path.Base(strings.SplitN(m.url, "?", 2)[0])
	if filename == "." || filename == "/" || filename == "" {
		filename = "Discord media"
	}
	content := &event.MessageEventContent{Body: filename, Info: &event.FileInfo{MimeType: dbFile.MimeType, Size: dbFile.Size, Width: dbFile.Width, Height: dbFile.Height}}
	if m.description != "" {
		content.Body = m.description
		content.FileName = filename
	}
	switch {
	case strings.HasPrefix(dbFile.MimeType, "image/"):
		content.MsgType = event.MsgImage
	case strings.HasPrefix(dbFile.MimeType, "video/"):
		content.MsgType = event.MsgVideo
	case strings.HasPrefix(dbFile.MimeType, "audio/"):
		content.MsgType = event.MsgAudio
	default:
		content.MsgType = event.MsgFile
	}
	if dbFile.DecryptionInfo != nil {
		content.File = &event.EncryptedFileInfo{EncryptedFile: *dbFile.DecryptionInfo, URL: dbFile.MXC.CUString()}
	} else {
		content.URL = dbFile.MXC.CUString()
	}
	extra := map[string]any{}
	if m.spoiler {
		extra[gallerySpoilerKey] = true
	}
	return &ConvertedMessage{AttachmentID: m.id, Type: event.EventMessage, Content: content, Extra: extra}
}

// componentSpoilers identifies attachment-backed media that must stay hidden,
// including attachments referenced by name instead of their CDN URL.
func componentSpoilers(components []discordgo.MessageComponent) map[string]bool {
	out := map[string]bool{}
	var walk func(discordgo.MessageComponent, bool)
	walk = func(c discordgo.MessageComponent, hidden bool) {
		switch x := c.(type) {
		case *discordgo.ActionsRow:
			for _, v := range x.Components {
				walk(v, hidden)
			}
		case *discordgo.Container:
			for _, v := range x.Components {
				walk(v, hidden || x.Spoiler)
			}
		case *discordgo.Section:
			for _, v := range x.Components {
				walk(v, hidden)
			}
			if x.Accessory != nil {
				walk(x.Accessory, hidden)
			}
		case *discordgo.Thumbnail:
			if hidden || x.Spoiler {
				out[x.Media.URL] = true
			}
		case *discordgo.MediaGallery:
			for _, v := range x.Items {
				if hidden || v.Spoiler {
					out[v.Media.URL] = true
				}
			}
		case *discordgo.FileComponent:
			if hidden || x.Spoiler {
				out[x.File.URL] = true
			}
		}
	}
	for _, c := range components {
		walk(c, false)
	}
	return out
}

func attachmentHasComponentSpoiler(att *discordgo.MessageAttachment, spoilers map[string]bool) bool {
	if spoilers[att.URL] || spoilers["attachment://"+att.Filename] {
		return true
	}
	if !trustedComponentMediaURL(att.URL) {
		return false
	}
	a, err := url.Parse(att.URL)
	if err != nil {
		return false
	}
	for raw := range spoilers {
		if !trustedComponentMediaURL(raw) {
			continue
		}
		u, err := url.Parse(raw)
		if err == nil && u.EscapedPath() == a.EscapedPath() {
			return true
		}
	}
	return false
}

func componentMediaAlreadyAttached(media componentMedia, attachments []*discordgo.MessageAttachment) bool {
	u, err := url.Parse(media.url)
	if err != nil {
		return false
	}
	for _, att := range attachments {
		if att == nil {
			continue
		}
		if att.URL == media.url {
			return true
		}
		if trustedComponentMediaURL(att.URL) {
			a, err := url.Parse(att.URL)
			if err == nil && a.EscapedPath() == u.EscapedPath() {
				return true
			}
		}
	}
	return false
}

// newComponentMedia selects unmapped card items, excluding Discord attachments
// which already have their own Matrix event.
func newComponentMedia(components []discordgo.MessageComponent, attachments []*discordgo.MessageAttachment, known map[string]bool) []componentMedia {
	var pending []componentMedia
	for _, media := range collectComponentMedia(components) {
		if !known[media.id] && !componentMediaAlreadyAttached(media, attachments) {
			pending = append(pending, media)
		}
	}
	return pending
}
