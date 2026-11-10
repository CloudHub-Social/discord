// mautrix-discord - A Matrix-Discord puppeting bridge.
// Copyright (C) 2026
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestTrustedComponentMediaURL(t *testing.T) {
	for _, tt := range []struct {
		url  string
		want bool
	}{
		{"https://cdn.discordapp.com/attachments/1/a.png?x=1", true},
		{"https://media.discordapp.net/a.png", true},
		{"http://cdn.discordapp.com/a", false},
		{"https://cdn.discordapp.com.evil.invalid/a", false},
		{"https://user@cdn.discordapp.com/a", false},
		{"https://cdn.discordapp.com:443/a", false},
		{"https://127.0.0.1/a", false},
		{"attachment://file.png", false},
	} {
		require.Equal(t, tt.want, trustedComponentMediaURL(tt.url), tt.url)
	}
}
func TestCollectComponentMediaNestedAndSpoiler(t *testing.T) {
	description := "description"
	components := []discordgo.MessageComponent{&discordgo.Container{Spoiler: true, Components: []discordgo.MessageComponent{&discordgo.MediaGallery{Items: []discordgo.MediaGalleryItem{
		{Media: discordgo.UnfurledMediaItem{URL: "https://cdn.discordapp.com/a.png?signature=one"}, Description: &description},
		{Media: discordgo.UnfurledMediaItem{URL: "https://cdn.discordapp.com/a.png?signature=two"}, Description: &description},
		{Media: discordgo.UnfurledMediaItem{URL: "https://evil.invalid/a.png"}},
	}}}}}
	got := collectComponentMedia(components)
	require.Len(t, got, 1)
	require.True(t, got[0].spoiler)
	require.Equal(t, description, got[0].description)
	require.Contains(t, got[0].id, "component_")
}

func TestComponentSpoilersForAttachments(t *testing.T) {
	got := componentSpoilers([]discordgo.MessageComponent{&discordgo.Container{Spoiler: true, Components: []discordgo.MessageComponent{&discordgo.MediaGallery{Items: []discordgo.MediaGalleryItem{{Media: discordgo.UnfurledMediaItem{URL: "attachment://private.png"}}}}}}})
	require.True(t, got["attachment://private.png"])
}

func TestComponentMediaRedirect(t *testing.T) {
	good, _ := http.NewRequest("GET", "https://media.discordapp.net/a", nil)
	bad, _ := http.NewRequest("GET", "http://127.0.0.1/private", nil)
	require.NoError(t, checkComponentMediaRedirect(good, nil))
	require.Error(t, checkComponentMediaRedirect(bad, nil))
	require.Error(t, checkComponentMediaRedirect(good, make([]*http.Request, 5)))
}

func TestEncryptedNewContentFailsClosedWithoutCrypto(t *testing.T) {
	_, _, portal, _ := newRecoveryTest(t)
	msg := &discordgo.Message{ID: "123", Poll: &discordgo.Poll{Question: discordgo.PollMedia{Text: "private"}}}
	require.Empty(t, portal.convertDiscordMessage(context.Background(), nil, nil, msg))
}

func TestAttachmentSpoilerMatchesRotatingSignedURL(t *testing.T) {
	att := &discordgo.MessageAttachment{Filename: "a.png", URL: "https://cdn.discordapp.com/attachments/1/a.png?token=old"}
	require.True(t, attachmentHasComponentSpoiler(att, map[string]bool{"https://media.discordapp.net/attachments/1/a.png?token=new": true}))
	require.True(t, attachmentHasComponentSpoiler(att, map[string]bool{"attachment://a.png": true}))
	require.False(t, attachmentHasComponentSpoiler(att, map[string]bool{"https://evil.invalid/attachments/1/a.png": true}))
}

func TestComponentMediaDeduplicatesAttachment(t *testing.T) {
	media := componentMedia{url: "https://media.discordapp.net/attachments/1/a.png?new=1"}
	require.True(t, componentMediaAlreadyAttached(media, []*discordgo.MessageAttachment{{URL: "https://cdn.discordapp.com/attachments/1/a.png?old=1"}}))
	require.False(t, componentMediaAlreadyAttached(media, []*discordgo.MessageAttachment{{URL: "https://evil.invalid/attachments/1/a.png"}}))
}

func TestNewComponentMediaOnEdit(t *testing.T) {
	old := &discordgo.MediaGallery{Items: []discordgo.MediaGalleryItem{{Media: discordgo.UnfurledMediaItem{URL: "https://cdn.discordapp.com/old.png"}}}}
	next := &discordgo.MediaGallery{Items: []discordgo.MediaGalleryItem{{Media: discordgo.UnfurledMediaItem{URL: "https://cdn.discordapp.com/new.png"}}}}
	oldMedia := collectComponentMedia([]discordgo.MessageComponent{old})
	require.Len(t, oldMedia, 1)
	known := map[string]bool{oldMedia[0].id: true}
	require.Empty(t, newComponentMedia([]discordgo.MessageComponent{old}, nil, known))
	add := newComponentMedia([]discordgo.MessageComponent{next}, nil, known)
	require.Len(t, add, 1)
	require.NotEqual(t, oldMedia[0].id, add[0].id)
	require.Empty(t, newComponentMedia([]discordgo.MessageComponent{next}, []*discordgo.MessageAttachment{{URL: "https://media.discordapp.net/new.png?token=1"}}, known))
}

func TestComponentMediaMetadataChangesIdentity(t *testing.T) {
	url := "https://cdn.discordapp.com/attachments/1/a.png"
	before := collectComponentMedia([]discordgo.MessageComponent{&discordgo.Thumbnail{Media: discordgo.UnfurledMediaItem{URL: url}}})
	after := collectComponentMedia([]discordgo.MessageComponent{&discordgo.Thumbnail{Media: discordgo.UnfurledMediaItem{URL: url}, Spoiler: true}})
	require.Len(t, before, 1)
	require.Len(t, after, 1)
	require.NotEqual(t, before[0].id, after[0].id)
}

func TestComponentMediaDescriptionAndSpoilerEditsChangeIdentity(t *testing.T) {
	first := "first"
	second := "second"
	makeItem := func(desc *string, spoiler bool) []componentMedia {
		return collectComponentMedia([]discordgo.MessageComponent{&discordgo.Thumbnail{Media: discordgo.UnfurledMediaItem{URL: "https://cdn.discordapp.com/a"}, Description: desc, Spoiler: spoiler}})
	}
	before, afterDesc, afterSpoiler := makeItem(&first, false), makeItem(&second, false), makeItem(&first, true)
	require.NotEqual(t, before[0].id, afterDesc[0].id)
	require.NotEqual(t, before[0].id, afterSpoiler[0].id)
}

func TestHasSeparateForwardMedia(t *testing.T) {
	msg := &discordgo.Message{MessageReference: &discordgo.MessageReference{Type: discordgo.MessageReferenceTypeForward}, MessageSnapshots: []discordgo.MessageSnapshot{{Message: &discordgo.Message{Attachments: []*discordgo.MessageAttachment{{ID: "1"}}}}}}
	require.True(t, hasSeparateForwardMedia(msg))
	msg.MessageSnapshots[0].Message.Attachments = nil
	require.False(t, hasSeparateForwardMedia(msg))
}

func TestCanAggregateGalleryAvoidsSeparateMedia(t *testing.T) {
	msg := &discordgo.Message{Attachments: []*discordgo.MessageAttachment{{ID: "1"}, {ID: "2"}}}
	require.True(t, canAggregateDiscordGallery(msg, false, true))
	msg.Components = []discordgo.MessageComponent{&discordgo.Thumbnail{Media: discordgo.UnfurledMediaItem{URL: "https://cdn.discordapp.com/new.png"}}}
	require.False(t, canAggregateDiscordGallery(msg, false, true))
	require.True(t, canAggregateDiscordGallery(msg, true, true))
}

func TestPrepareAttachmentPresentationEdit(t *testing.T) {
	part := &ConvertedMessage{Content: &event.MessageEventContent{MsgType: event.MsgImage, Body: "image.png", URL: "mxc://example.org/image"}, Extra: map[string]any{"other": "value"}}
	prepareAttachmentPresentationEdit(part, "$old", true)
	require.Equal(t, event.RelReplace, part.Content.RelatesTo.Type)
	require.Equal(t, id.EventID("$old"), part.Content.RelatesTo.EventID)
	require.Equal(t, true, part.Extra[gallerySpoilerKey])
	nested, ok := part.Extra["m.new_content"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, nested[gallerySpoilerKey])
	require.Equal(t, "value", nested["other"])
	encoded, err := json.Marshal(&event.Content{Parsed: part.Content, Raw: part.Extra})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(encoded, &payload))
	require.Equal(t, true, payload["m.new_content"].(map[string]any)[gallerySpoilerKey])
}

func TestAttachmentPresentationEditCanClearSpoiler(t *testing.T) {
	part := &ConvertedMessage{Content: &event.MessageEventContent{MsgType: event.MsgImage, Body: "image.png", URL: "mxc://example.org/image"}, Extra: map[string]any{gallerySpoilerKey: true}}
	prepareAttachmentPresentationEdit(part, "$old", false)
	data, err := json.Marshal(&event.Content{Parsed: part.Content, Raw: part.Extra})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(data, &payload))
	require.Equal(t, false, payload["m.new_content"].(map[string]any)[gallerySpoilerKey])
}

func TestMergePartialDiscordMessage(t *testing.T) {
	original := &discordgo.Message{Content: "old", Attachments: []*discordgo.MessageAttachment{{ID: "1"}}, Components: []discordgo.MessageComponent{&discordgo.Button{Label: "old"}}}
	updated := &discordgo.Message{Content: "new", Components: []discordgo.MessageComponent{}}
	mergePartialDiscordMessage(original, updated)
	require.Equal(t, "new", original.Content)
	require.Len(t, original.Attachments, 1)
	require.NotNil(t, original.Components)
	require.Empty(t, original.Components)
}
