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
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
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
		{Media: discordgo.UnfurledMediaItem{URL: "https://cdn.discordapp.com/a.png?signature=two"}},
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
