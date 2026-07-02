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

// TestOnConnectionStable_MatchingEpochResetsAttempts covers the intended
// behavior: once a connection has stayed up long enough for onConnectionStable
// to fire with no intervening disconnect (connectionEpoch unchanged since the
// timer was armed), the backoff counter clears.
func TestOnConnectionStable_MatchingEpochResetsAttempts(t *testing.T) {
	u := newTestUser()
	u.reconnectAttempts = 5

	u.onConnectionStable(u.connectionEpoch)

	assert.Equal(t, 0, u.reconnectAttempts)
	assert.Nil(t, u.stableTimer)
}

// TestOnConnectionStable_StaleEpochDoesNotResetAttempts is the regression guard
// for the core fix: a disconnect between arming the stability timer and it
// firing bumps connectionEpoch (see disarmReadyWatchdog), so a stale timer that
// fires anyway -- e.g. racing past time.Timer.Stop() -- must not clear the
// backoff counter on behalf of a connection that didn't actually stay up.
func TestOnConnectionStable_StaleEpochDoesNotResetAttempts(t *testing.T) {
	u := newTestUser()
	u.reconnectAttempts = 5
	staleEpoch := u.connectionEpoch
	u.connectionEpoch++ // simulates disarmReadyWatchdog running after the timer was armed

	u.onConnectionStable(staleEpoch)

	assert.Equal(t, 5, u.reconnectAttempts, "a stale epoch must not touch the backoff counter after a disconnect bumped it")
}

// TestOnConnectionEstablished_DoesNotResetAttemptsImmediately is the regression
// guard for the original bug: reaching READY/RESUMED must arm the stability
// timer, but must NOT by itself clear reconnectAttempts -- only
// onConnectionStable, after the connection survives stableConnectionThreshold,
// may do that.
func TestOnConnectionEstablished_DoesNotResetAttemptsImmediately(t *testing.T) {
	u := newTestUser()
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
	u.reconnectAttempts = 3
	u.onConnectionEstablished()
	assert.NotNil(t, u.stableTimer)

	u.disarmReadyWatchdog()

	assert.Nil(t, u.stableTimer)
	assert.Equal(t, 3, u.reconnectAttempts)
}

// TestDisarmReadyWatchdog_BumpsEpochSoLateStableFiringIsIgnored is the direct
// regression test for the Stop()-vs-fire race this epoch exists to close: even
// if onConnectionStable's timer callback is already running when
// disarmReadyWatchdog runs (Stop() returning too late to prevent it), the epoch
// bump means that stale callback -- once it does run -- sees a mismatched epoch
// and leaves reconnectAttempts alone.
func TestDisarmReadyWatchdog_BumpsEpochSoLateStableFiringIsIgnored(t *testing.T) {
	u := newTestUser()
	u.reconnectAttempts = 3
	u.onConnectionEstablished()
	armedEpoch := u.connectionEpoch

	u.disarmReadyWatchdog() // simulates a disconnect landing first

	// Simulate the stale timer callback running anyway, after the disconnect.
	u.onConnectionStable(armedEpoch)

	assert.Equal(t, 3, u.reconnectAttempts, "a stale onConnectionStable firing after disarmReadyWatchdog must not reset attempts")
}

// TestOnConnectionStable_DoesNotResetWhilePendingReconnect is the direct
// regression test for a gap both automated reviewers on PR #31 flagged: a
// stale onConnectionEstablished can run for a READY/RESUMED whose dispatch was
// delayed until after disconnectedHandler already processed that same
// connection's drop (event handlers can run on a goroutine that lags behind
// actual gateway state). In that case the stale event captures the
// *already-bumped* epoch when arming its stability timer -- an epoch match
// alone would then incorrectly look legitimate 30 seconds later. This
// simulates exactly that: the epoch matches (no *further* disconnect happened
// after the stale timer was armed), but a reconnect is currently pending
// (reconnectTimer != nil), which can only be true if we are not actually in a
// stable connected state right now -- reconnectAttempts must not reset.
func TestOnConnectionStable_DoesNotResetWhilePendingReconnect(t *testing.T) {
	u := newTestUser()
	u.reconnectAttempts = 3
	// A pending backoff reconnect -- only its nil-ness is checked, so any live
	// timer works as a stand-in.
	u.reconnectTimer = time.NewTimer(time.Hour)
	defer u.reconnectTimer.Stop()

	u.onConnectionStable(u.connectionEpoch) // epoch matches; no disconnect happened

	assert.Equal(t, 3, u.reconnectAttempts, "must not reset the backoff counter while a reconnect is pending, even with a matching epoch")
	assert.Nil(t, u.stableTimer, "the timer reference should still be cleared even when the reset itself is skipped")
}
