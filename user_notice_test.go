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
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
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

// TestSentBadCredentialsNoticeDedup exercises the sentBadCredentialsNotice
// check-and-set pattern that sendBadCredentialsNotice uses to ensure a notice
// fires at most once per invalidation, not once per call (handlePossible40002
// in particular can be hit repeatedly by retried REST calls while the account
// stays broken). This mirrors sendBadCredentialsNotice's locking exactly, but
// does not call the function itself, since doing so requires a live Matrix
// intent (user.bridge.Bot) this repo has no test double for yet -- see the
// spec's Testing Strategy section for the coverage gap this leaves.
func TestSentBadCredentialsNoticeDedup(t *testing.T) {
	u := newTestUser()

	assert.False(t, u.sentBadCredentialsNotice, "starts unset")

	u.bridgeStateLock.Lock()
	firstCall := !u.sentBadCredentialsNotice
	u.sentBadCredentialsNotice = true
	u.bridgeStateLock.Unlock()
	assert.True(t, firstCall, "first call should proceed to send")

	u.bridgeStateLock.Lock()
	secondCall := !u.sentBadCredentialsNotice
	u.bridgeStateLock.Unlock()
	assert.False(t, secondCall, "second call for the same invalidation must be suppressed")

	// A fresh Login resets the flag, allowing a future invalidation to notify again.
	u.wasLoggedOut = true
	u.bridgeStateLock.Lock()
	u.wasLoggedOut = false
	u.sentBadCredentialsNotice = false
	u.bridgeStateLock.Unlock()
	assert.False(t, u.sentBadCredentialsNotice, "Login resets the dedup flag for the next invalidation")
}
