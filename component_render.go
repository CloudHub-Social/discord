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
	"fmt"
	"html"
	"net/url"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// renderDiscordComponents presents the information Discord provides without
// pretending that application controls can be operated from Matrix.
func (portal *Portal) renderDiscordComponents(components []discordgo.MessageComponent, messageURL string) string {
	var parts []string
	unsupported := false
	needsDiscord := false
	var render func(discordgo.MessageComponent)
	render = func(component discordgo.MessageComponent) {
		switch c := component.(type) {
		case *discordgo.ActionsRow:
			for _, child := range c.Components {
				render(child)
			}
		case *discordgo.Container:
			if c.Spoiler {
				parts = append(parts, "<p>Discord spoiler content: open in Discord to view.</p>")
				needsDiscord = true
				return
			}
			for _, child := range c.Components {
				render(child)
			}
		case *discordgo.Section:
			for _, child := range c.Components {
				render(child)
			}
			if c.Accessory != nil {
				render(c.Accessory)
			}
		case *discordgo.TextDisplay:
			parts = append(parts, portal.renderDiscordMarkdownOnlyHTMLNoUnwrap(c.Content, true))
		case *discordgo.Button:
			label := c.Label
			if c.Emoji != nil && c.Emoji.Name != "" {
				label = strings.TrimSpace(c.Emoji.Name + " " + label)
			}
			if label == "" {
				label = "Button"
			}
			label = html.EscapeString(label)
			u, err := url.Parse(c.URL)
			if c.Style == discordgo.LinkButton && !c.Disabled && err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") {
				parts = append(parts, fmt.Sprintf(`<p><a href="%s">%s</a></p>`, html.EscapeString(c.URL), label))
			} else if c.Disabled {
				parts = append(parts, "<p>"+label+" (disabled)</p>")
			} else {
				parts = append(parts, "<p>"+label+"</p>")
				unsupported = true
			}
		case *discordgo.SelectMenu:
			label := c.Placeholder
			if label == "" {
				label = "Select menu"
			}
			parts = append(parts, "<p>"+html.EscapeString(label)+"</p>")
			for _, option := range c.Options {
				parts = append(parts, "<p>• "+html.EscapeString(option.Label)+"</p>")
			}
			if !c.Disabled {
				unsupported = true
			}
		case *discordgo.Separator:
			// Separation is already provided by paragraph formatting.
		default:
			// Media, unknown and future component types need a safe fallback. In
			// particular, do not expose spoiler media or raw attachment URLs.
			unsupported = true
		}
	}
	for _, component := range components {
		render(component)
	}
	if unsupported {
		parts = append(parts, msgComponentTemplateHTML)
	}
	if (unsupported || needsDiscord) && messageURL != "" {
		parts = append(parts, fmt.Sprintf(`<p><a href="%s">Open message in Discord</a></p>`, html.EscapeString(messageURL)))
	}
	return strings.Join(parts, "\n")
}

// discordMessageURL uses the actual message channel, which can be a thread.
// Missing or malformed IDs fall back to the existing text-only notice.
func (portal *Portal) discordRedirectBase() string {
	if portal == nil || portal.bridge == nil || portal.bridge.Config == nil {
		return ""
	}
	return portal.bridge.Config.Bridge.DiscordRedirectBase
}

func discordMessageURL(guildID, channelID, messageID string, redirectBase ...string) string {
	validID := func(id string) bool {
		if id == "" {
			return false
		}
		for _, c := range id {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	if !validID(channelID) || !validID(messageID) {
		return ""
	}
	if guildID == "" {
		guildID = "@me"
	} else if !validID(guildID) {
		return ""
	}
	if len(redirectBase) > 0 && redirectBase[0] != "" {
		u, err := url.Parse(redirectBase[0])
		if err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" {
			return strings.TrimRight(redirectBase[0], "/") + "/mautrix-discord/open/" + guildID + "/" + channelID + "/" + messageID
		}
	}
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, channelID, messageID)
}
