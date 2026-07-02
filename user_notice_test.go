// mautrix-discord - A Matrix-Discord puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func newTestUser() *User {
	return &User{log: zerolog.Nop()}
}

func TestBadCredentialsNoticeSkipReason(t *testing.T) {
	tests := []struct {
		name           string
		noticesEnabled bool
		managementRoom id.RoomID
		wantSkip       bool
	}{
		{"enabled with a management room sends", true, "!room:example.com", false},
		{"disabled skips even with a management room", false, "!room:example.com", true},
		{"enabled but no management room skips", true, "", true},
		{"disabled and no management room skips", false, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, skip := badCredentialsNoticeSkipReason(tt.noticesEnabled, tt.managementRoom)
			assert.Equal(t, tt.wantSkip, skip)
		})
	}
}

// TestBadCredentialsNoticeAttemptDecision covers requirement P0-A.2 (fire at
// most once per invalidation, not once per call) and the retry-cooldown
// hardening added on review: a burst of handlePossible40002 calls while the
// homeserver send keeps failing must back off rather than retrying on every
// single call.
func TestBadCredentialsNoticeAttemptDecision(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name           string
		alreadySent    bool
		lastAttempt    time.Time
		wantAttempt    bool
		wantSkipReason string
	}{
		{"first ever attempt proceeds", false, time.Time{}, true, ""},
		{"already sent this invalidation is suppressed", true, time.Time{}, false, "already_sent"},
		{"already_sent wins even if the cooldown would also allow a retry", true, now.Add(-time.Hour), false, "already_sent"},
		{"retry within the cooldown window is suppressed", false, now.Add(-30 * time.Second), false, "retry_cooldown"},
		{"retry right at the cooldown boundary is suppressed", false, now.Add(-badCredentialsNoticeRetryCooldown + time.Second), false, "retry_cooldown"},
		{"retry after the cooldown has elapsed proceeds", false, now.Add(-badCredentialsNoticeRetryCooldown - time.Second), true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt, reason := badCredentialsNoticeAttemptDecision(tt.alreadySent, tt.lastAttempt, now)
			assert.Equal(t, tt.wantAttempt, attempt)
			assert.Equal(t, tt.wantSkipReason, reason)
		})
	}
}

// fakeNoticeSender is a test double for noticeSender that records every call
// it receives instead of talking to a real Matrix homeserver. failNext, if
// set, is returned as the error for exactly the next call, then cleared.
type fakeNoticeSender struct {
	calls    []fakeNoticeSenderCall
	failNext error
}

type fakeNoticeSenderCall struct {
	RoomID    id.RoomID
	EventType event.Type
	Content   interface{}
}

func (f *fakeNoticeSender) SendMessageEvent(roomID id.RoomID, eventType event.Type, contentJSON interface{}) (*mautrix.RespSendEvent, error) {
	f.calls = append(f.calls, fakeNoticeSenderCall{roomID, eventType, contentJSON})
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, err
	}
	return &mautrix.RespSendEvent{EventID: "$fake"}, nil
}

// TestSendBadCredentialsNoticeContent is the integration-style test the first
// pass of this PR flagged as a coverage gap: it exercises the actual dispatch
// path (room targeting, event type, message body) against a real noticeSender
// implementation, not just the gating logic around it.
func TestSendBadCredentialsNoticeContent(t *testing.T) {
	sender := &fakeNoticeSender{}
	roomID := id.RoomID("!management:example.com")

	err := sendBadCredentialsNoticeContent(sender, roomID, "token is dead")

	assert.NoError(t, err)
	assert.Len(t, sender.calls, 1)
	call := sender.calls[0]
	assert.Equal(t, roomID, call.RoomID)
	assert.Equal(t, event.EventMessage, call.EventType)
	content, ok := call.Content.(*event.MessageEventContent)
	assert.True(t, ok, "content should be a *event.MessageEventContent")
	assert.Equal(t, event.MsgNotice, content.MsgType)
	assert.Contains(t, content.Body, "token is dead")
	assert.Contains(t, content.Body, "login", "the notice must tell the user how to reconnect")
	assert.NotContains(t, content.Body, "!discord login", "commands in the management room never need the bridge's command prefix")
}

// TestSendBadCredentialsNoticeContent_PropagatesSendError confirms a failed
// send is surfaced to the caller (sendBadCredentialsNotice relies on this to
// know when to release the dedup flag for a retry).
func TestSendBadCredentialsNoticeContent_PropagatesSendError(t *testing.T) {
	wantErr := errors.New("connection refused")
	sender := &fakeNoticeSender{failNext: wantErr}

	err := sendBadCredentialsNoticeContent(sender, "!management:example.com", "token is dead")

	assert.ErrorIs(t, err, wantErr)
}
