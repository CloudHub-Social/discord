// mautrix-discord - A Matrix-Discord puppeting bridge.
// Copyright (C) 2026 CloudHub Social
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"go.mau.fi/mautrix-discord/database"
)

func TestRenderDiscordComponents(t *testing.T) {
	raw := `{"id":"1","components":[{"type":17,"components":[{"type":10,"content":"**Wordle** results"},{"type":1,"components":[{"type":2,"style":1,"label":"Play <now>","custom_id":"private"},{"type":2,"style":5,"label":"Rules","url":"https://example.org/?a=1&b=2"},{"type":3,"custom_id":"choice","placeholder":"Choose","options":[{"label":"One","value":"secret"}]}]}]}]}`
	var msg discordgo.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	got := (&Portal{}).renderDiscordComponents(msg.Components, "")
	for _, want := range []string{"<strong>Wordle</strong>", "Play &lt;now&gt;", `href="https://example.org/?a=1&amp;b=2"`, "Choose", "One", "Use the Discord app"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, forbidden := range []string{"private", "secret", "<now>"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("unexpected %q in %s", forbidden, got)
		}
	}
}

func TestRenderDiscordComponentsSafety(t *testing.T) {
	p := &Portal{}
	cases := []struct {
		name             string
		components       []discordgo.MessageComponent
		contains, absent string
	}{
		{"unsafe link", []discordgo.MessageComponent{&discordgo.Button{Style: discordgo.LinkButton, Label: "open", URL: "javascript:alert(1)"}}, "Use the Discord app", "javascript:"},
		{"disabled", []discordgo.MessageComponent{&discordgo.Button{Label: "Done", Disabled: true}}, "Done (disabled)", "Use the Discord app"},
		{"spoiler", []discordgo.MessageComponent{&discordgo.Container{Spoiler: true, Components: []discordgo.MessageComponent{&discordgo.TextDisplay{Content: "hidden"}}}}, "spoiler content", "hidden"},
		{"media", []discordgo.MessageComponent{&discordgo.MediaGallery{}}, "Use the Discord app", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := p.renderDiscordComponents(tc.components, "")
			if !strings.Contains(got, tc.contains) || (tc.absent != "" && strings.Contains(got, tc.absent)) {
				t.Errorf("unexpected output: %s", got)
			}
		})
	}
}

func TestRenderWordleCardLines(t *testing.T) {
	p := &Portal{}
	got := p.renderDiscordComponents([]discordgo.MessageComponent{&discordgo.Container{Components: []discordgo.MessageComponent{&discordgo.TextDisplay{Content: "Wordle 1970 4/6*\n\n🟨⬛⬛\n🟩🟩🟩"}}}}, "")
	if !strings.Contains(got, "Wordle 1970") || !strings.Contains(got, "🟨⬛⬛") || !strings.Contains(got, "🟩🟩🟩") || !(strings.Contains(got, "<br") || strings.Contains(got, "</p>")) {
		t.Fatalf("lost Wordle content or line breaks: %s", got)
	}
}

func TestAdjacentTextDisplaysPreserveBlocks(t *testing.T) {
	p := &Portal{}
	got := p.renderDiscordComponents([]discordgo.MessageComponent{&discordgo.TextDisplay{Content: "First"}, &discordgo.Separator{}, &discordgo.TextDisplay{Content: "Second"}}, "")
	if !strings.Contains(got, "<p>First</p>") || !strings.Contains(got, "<p>Second</p>") {
		t.Fatalf("text displays lack block boundaries: %s", got)
	}
}

func TestDiscordMessageURL(t *testing.T) {
	tests := []struct{ guild, channel, message, want string }{
		{"", "123", "456", "https://discord.com/channels/@me/123/456"},
		{"789", "123", "456", "https://discord.com/channels/789/123/456"},
		{"789", "", "456", ""}, {"789", "123", "", ""},
		{"bad/host", "123", "456", ""}, {"", "123/evil", "456", ""},
	}
	for _, tt := range tests {
		if got := discordMessageURL(tt.guild, tt.channel, tt.message); got != tt.want {
			t.Errorf("discordMessageURL(%q,%q,%q) = %q, want %q", tt.guild, tt.channel, tt.message, got, tt.want)
		}
	}
}

func TestComponentDiscordLinks(t *testing.T) {
	portal := &Portal{}
	messageURL := discordMessageURL("", "123", "456")
	cases := []struct {
		name         string
		components   []discordgo.MessageComponent
		want, absent string
	}{
		{"interactive", []discordgo.MessageComponent{&discordgo.Button{Label: "Play", Style: discordgo.PrimaryButton}}, `href="https://discord.com/channels/@me/123/456"`, ""},
		{"spoiler", []discordgo.MessageComponent{&discordgo.Container{Spoiler: true}}, `href="https://discord.com/channels/@me/123/456"`, ""},
		{"link", []discordgo.MessageComponent{&discordgo.Button{Label: "Visit", Style: discordgo.LinkButton, URL: "https://example.org/path?a=1&b=2"}}, `href="https://example.org/path?a=1&amp;b=2"`, "Open message in Discord"},
		{"disabled", []discordgo.MessageComponent{&discordgo.Button{Label: "Visit", Style: discordgo.LinkButton, URL: "https://example.org", Disabled: true}}, "Visit (disabled)", "https://example.org"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := portal.renderDiscordComponents(tt.components, messageURL)
			if !strings.Contains(got, tt.want) || (tt.absent != "" && strings.Contains(got, tt.absent)) {
				t.Fatalf("unexpected result: %s", got)
			}
		})
	}
	if got := portal.renderDiscordComponents([]discordgo.MessageComponent{&discordgo.Button{Label: "Play"}}, ""); !strings.Contains(got, "Use the Discord app") || strings.Contains(got, "href=") {
		t.Fatalf("missing fallback: %s", got)
	}
}

func TestComponentDiscordLinkInMatrixMessage(t *testing.T) {
	portal := &Portal{Portal: &database.Portal{}}
	msg := &discordgo.Message{ID: "456", ChannelID: "123", Components: []discordgo.MessageComponent{&discordgo.Button{Label: "Play", Style: discordgo.PrimaryButton}}}
	converted := portal.convertDiscordTextMessage(context.Background(), nil, msg)
	if converted == nil || !strings.Contains(converted.Content.FormattedBody, `href="https://discord.com/channels/@me/123/456"`) {
		t.Fatalf("missing Discord link in converted Matrix message: %#v", converted)
	}
}
