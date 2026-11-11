// mautrix-discord - A Matrix-Discord puppeting bridge.
// Copyright (C) 2026
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"fmt"
	"html"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// discordPollHTML renders a snapshot, never a Matrix poll that falsely suggests voting works.
func discordPollHTML(msg *discordgo.Message, guildID string, redirectBase ...string) string {
	var lines []string
	if msg.Poll != nil {
		p := msg.Poll
		lines = append(lines, "<p><strong>Discord poll: "+html.EscapeString(p.Question.Text)+"</strong></p>")
		counts := map[int]int{}
		if p.Results != nil {
			for _, count := range p.Results.AnswerCounts {
				if count != nil {
					counts[count.ID] = count.Count
				}
			}
		}
		for _, a := range p.Answers {
			if a.Media != nil {
				label := a.Media.Text
				if a.Media.Emoji != nil && a.Media.Emoji.Name != "" {
					label = a.Media.Emoji.Name + " " + label
				}
				if count, ok := counts[a.AnswerID]; ok {
					label += fmt.Sprintf(" (%d votes)", count)
				}
				lines = append(lines, "<p>• "+html.EscapeString(label)+"</p>")
			}
		}
		if p.Expiry != nil {
			lines = append(lines, "<p>Ends: "+html.EscapeString(p.Expiry.UTC().Format("2006-01-02 15:04 UTC"))+"</p>")
		}
		lines = append(lines, "<p>Vote in Discord. Vote counts, when shown, are a snapshot.</p>")
	}
	for _, e := range msg.Embeds {
		if e == nil || string(e.Type) != "poll_result" {
			continue
		}
		labels := map[string]string{"poll_question_text": "Poll", "victor_answer_text": "Winner", "victor_answer_votes": "Winning votes", "total_votes": "Total votes"}
		var fields []string
		for _, f := range e.Fields {
			if f != nil {
				if label, ok := labels[f.Name]; ok {
					fields = append(fields, "<p>"+label+": "+html.EscapeString(f.Value)+"</p>")
				}
			}
		}
		if len(fields) == 0 {
			lines = append(lines, "<p>Discord poll ended. Open Discord to see the results.</p>")
		} else {
			lines = append(lines, fields...)
		}
	}
	if len(lines) > 0 {
		if link := discordMessageURL(guildID, msg.ChannelID, msg.ID, redirectBase...); link != "" {
			lines = append(lines, fmt.Sprintf(`<p><a href="%s">Open message in Discord</a></p>`, html.EscapeString(link)))
		}
	}
	return strings.Join(lines, "\n")
}
