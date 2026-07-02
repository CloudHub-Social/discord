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
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func newTestUser() *User {
	return &User{log: zerolog.Nop()}
}

func TestReconnectDelay(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		expected time.Duration
	}{
		{"first attempt", 1, 2 * time.Second},
		{"second attempt", 2, 4 * time.Second},
		{"third attempt", 3, 8 * time.Second},
		{"fourth attempt", 4, 16 * time.Second},
		{"sixth attempt", 6, 64 * time.Second},
		{"capped at the shift boundary", 10, reconnectBackoffMax},
		{"capped well past the boundary", 50, reconnectBackoffMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, reconnectDelay(tt.attempts))
		})
	}
}

// TestReconnectDelayStrictlyEscalates is the direct regression guard for the
// backoff-reset bug: a connection that keeps failing must see a strictly
// increasing delay, not the base delay forever, up to the cap.
func TestReconnectDelayStrictlyEscalates(t *testing.T) {
	var prev time.Duration
	for attempt := 1; attempt <= 10; attempt++ {
		d := reconnectDelay(attempt)
		assert.Greater(t, d, prev, "delay must strictly increase at attempt %d", attempt)
		prev = d
	}
	assert.Equal(t, reconnectBackoffMax, reconnectDelay(10))
	assert.Equal(t, reconnectBackoffMax, reconnectDelay(50), "delay must never exceed the cap regardless of attempt count")
}

// TestOnConnectionStable_MatchingSessionResetsAttempts covers the intended
// behavior: once a connection has stayed up long enough for onConnectionStable
// to fire for the session that's still current, the backoff counter clears.
func TestOnConnectionStable_MatchingSessionResetsAttempts(t *testing.T) {
	u := newTestUser()
	sess := &discordgo.Session{}
	u.Session = sess
	u.reconnectAttempts = 5

	u.onConnectionStable(sess)

	assert.Equal(t, 0, u.reconnectAttempts)
	assert.Nil(t, u.stableTimer)
}

// TestOnConnectionStable_StaleSessionDoesNotResetAttempts is the regression
// guard for the core fix: a stability timer armed for a session that has since
// been replaced (i.e. the connection it was tracking did not actually stay up)
// must not clear the backoff counter on behalf of the newer session.
func TestOnConnectionStable_StaleSessionDoesNotResetAttempts(t *testing.T) {
	u := newTestUser()
	oldSession := &discordgo.Session{}
	newSession := &discordgo.Session{}
	u.Session = newSession
	u.reconnectAttempts = 5

	u.onConnectionStable(oldSession)

	assert.Equal(t, 5, u.reconnectAttempts, "a stale session's stability timer must not touch a newer session's backoff counter")
}

// TestOnConnectionEstablished_DoesNotResetAttemptsImmediately is the regression
// guard for the original bug: reaching READY/RESUMED must arm the stability
// timer, but must NOT by itself clear reconnectAttempts -- only
// onConnectionStable, after the connection survives stableConnectionThreshold,
// may do that.
func TestOnConnectionEstablished_DoesNotResetAttemptsImmediately(t *testing.T) {
	u := newTestUser()
	u.Session = &discordgo.Session{}
	u.reconnectAttempts = 3

	u.onConnectionEstablished()
	defer u.stableTimer.Stop()

	assert.True(t, u.sessionReady)
	assert.NotNil(t, u.stableTimer, "onConnectionEstablished must arm the stability timer")
	assert.Equal(t, 3, u.reconnectAttempts, "reaching READY alone must not reset the backoff counter")
}

// TestDisarmReadyWatchdog_ClearsStableTimerWithoutResettingAttempts covers a
// connection that drops before the stability threshold: the pending timer must
// be cancelled (so it can't fire late for a session that's already gone), but
// the accumulated attempts must survive so the next scheduleReconnect call
// keeps escalating instead of restarting from the base delay.
func TestDisarmReadyWatchdog_ClearsStableTimerWithoutResettingAttempts(t *testing.T) {
	u := newTestUser()
	u.Session = &discordgo.Session{}
	u.reconnectAttempts = 3
	u.onConnectionEstablished()
	assert.NotNil(t, u.stableTimer)

	u.disarmReadyWatchdog()

	assert.Nil(t, u.stableTimer)
	assert.Equal(t, 3, u.reconnectAttempts)
}
