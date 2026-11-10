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
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"go.mau.fi/mautrix-discord/database"
)

func TestDiscordPollHTML(t *testing.T) {
	msg := &discordgo.Message{ID: "456", ChannelID: "123", Poll: &discordgo.Poll{Question: discordgo.PollMedia{Text: "Lunch <today>?"}, Answers: []discordgo.PollAnswer{{Media: &discordgo.PollMedia{Text: "Pizza"}}}}}
	got := discordPollHTML(msg, "")
	for _, want := range []string{"Lunch &lt;today&gt;?", "Pizza", "Vote in Discord", "https://discord.com/channels/@me/123/456"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
}
func TestDiscordPollResultHTML(t *testing.T) {
	msg := &discordgo.Message{ID: "456", ChannelID: "123", Embeds: []*discordgo.MessageEmbed{{Type: "poll_result", Fields: []*discordgo.MessageEmbedField{{Name: "poll_question_text", Value: "Lunch?"}, {Name: "victor_answer_text", Value: "Pizza"}, {Name: "total_votes", Value: "3"}}}}}
	got := discordPollHTML(msg, "")
	for _, want := range []string{"Lunch?", "Winner: Pizza", "Total votes: 3", "Open message in Discord"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
}

func TestDiscordVoiceWaveform(t *testing.T) {
	got := discordVoiceWaveform([]byte{0, 128, 255})
	if len(got) != 3 || got[0] != 0 || got[1] != 514 || got[2] != 1024 {
		t.Fatalf("unexpected waveform %v", got)
	}
}

func TestDiscordPollResultAsMatrixMessage(t *testing.T) {
	portal := &Portal{Portal: &database.Portal{}}
	msg := &discordgo.Message{ID: "456", ChannelID: "123", Embeds: []*discordgo.MessageEmbed{{Type: "poll_result", Fields: []*discordgo.MessageEmbedField{{Name: "poll_question_text", Value: "Lunch?"}, {Name: "victor_answer_text", Value: "Pizza"}}}}}
	converted := portal.convertDiscordTextMessage(context.Background(), nil, msg)
	if converted == nil || !strings.Contains(converted.Content.FormattedBody, "Winner: Pizza") {
		t.Fatalf("poll result missing from Matrix message: %#v", converted)
	}
}

func TestForwardWithCommentRetainsSnapshotText(t *testing.T) {
	_, _, portal, _ := newRecoveryTest(t)
	msg := &discordgo.Message{ID: "1001", ChannelID: "1002", Content: "my comment", MessageReference: &discordgo.MessageReference{Type: discordgo.MessageReferenceTypeForward, ChannelID: "1003"}, MessageSnapshots: []discordgo.MessageSnapshot{{Message: &discordgo.Message{Content: "forwarded text"}}}}
	converted := portal.convertDiscordTextMessage(context.Background(), nil, msg)
	if converted == nil || !strings.Contains(converted.Content.Body, "my comment") || !strings.Contains(converted.Content.Body, "forwarded text") {
		t.Fatalf("missing comment or forwarded text: %#v", converted)
	}
}

func TestForwardSnapshotAttachmentRedactionSet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		snapshots []discordgo.MessageSnapshot
		retained  bool
	}{
		{"omitted", nil, true},
		{"present", []discordgo.MessageSnapshot{{Message: &discordgo.Message{Attachments: []*discordgo.MessageAttachment{{ID: "123"}}}}}, true},
		{"removed", []discordgo.MessageSnapshot{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := map[string]*database.Message{"forward_123": nil, "other": nil}
			retainForwardSnapshotAttachments(pending, tc.snapshots)
			_, remains := pending["forward_123"]
			if remains == tc.retained {
				t.Fatalf("unexpected redaction set: %v", pending)
			}
			if _, ok := pending["other"]; !ok {
				t.Fatal("unrelated attachment removed")
			}
		})
	}
}
