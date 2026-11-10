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
	"encoding/json"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestRenderDiscordComponents(t *testing.T) {
	raw := `{"id":"1","components":[{"type":17,"components":[{"type":10,"content":"**Wordle** results"},{"type":1,"components":[{"type":2,"style":1,"label":"Play <now>","custom_id":"private"},{"type":2,"style":5,"label":"Rules","url":"https://example.org/?a=1&b=2"},{"type":3,"custom_id":"choice","placeholder":"Choose","options":[{"label":"One","value":"secret"}]}]}]}]}`
	var msg discordgo.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	got := (&Portal{}).renderDiscordComponents(msg.Components)
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
			got := p.renderDiscordComponents(tc.components)
			if !strings.Contains(got, tc.contains) || (tc.absent != "" && strings.Contains(got, tc.absent)) {
				t.Errorf("unexpected output: %s", got)
			}
		})
	}
}

func TestRenderWordleCardLines(t *testing.T) {
	p := &Portal{}
	got := p.renderDiscordComponents([]discordgo.MessageComponent{&discordgo.Container{Components: []discordgo.MessageComponent{&discordgo.TextDisplay{Content: "Wordle 1970 4/6*\n\n🟨⬛⬛\n🟩🟩🟩"}}}})
	if !strings.Contains(got, "Wordle 1970") || !strings.Contains(got, "🟨⬛⬛") || !strings.Contains(got, "🟩🟩🟩") || !(strings.Contains(got, "<br") || strings.Contains(got, "</p>")) {
		t.Fatalf("lost Wordle content or line breaks: %s", got)
	}
}

func TestAdjacentTextDisplaysPreserveBlocks(t *testing.T) {
	p := &Portal{}
	got := p.renderDiscordComponents([]discordgo.MessageComponent{&discordgo.TextDisplay{Content: "First"}, &discordgo.Separator{}, &discordgo.TextDisplay{Content: "Second"}})
	if !strings.Contains(got, "<p>First</p>") || !strings.Contains(got, "<p>Second</p>") {
		t.Fatalf("text displays lack block boundaries: %s", got)
	}
}
