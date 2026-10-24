package main

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A RESUMED handshake exercises the real Session callback without the unrelated
// room/backfill work performed by READY. The gateway stays alive until Close.
func serveResumedConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = conn.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 3_600_000}})
	var frame heartbeatWireFrame
	if conn.ReadJSON(&frame) != nil {
		return
	}
	if conn.WriteJSON(map[string]any{"op": 0, "t": "RESUMED", "s": 1, "d": map[string]any{}}) != nil {
		return
	}
	for conn.ReadJSON(&frame) == nil {
		if frame.Op == 1 {
			if conn.WriteJSON(map[string]any{"op": 11, "d": nil}) != nil {
				return
			}
		}
	}
}

func TestCurrentSessionLifecycleIsOrdered(t *testing.T) {
	useConnectionTestGateway(t, serveResumedConnection)
	u := newConnectionTestUser(t)
	require.NoError(t, u.Connect())
	require.True(t, u.Connected(), "RESUMED marks readiness before Open returns")

	callback := u.Session.EventHandler
	callback(&discordgo.Disconnect{})
	assert.False(t, u.Connected(), "Disconnect withdraws readiness synchronously")
	callback(&discordgo.Resumed{})
	assert.False(t, u.Connected(), "a delayed success from the dropped connection must not revive it")
	u.reconnectLock.Lock()
	assert.NotNil(t, u.reconnectTimer)
	u.reconnectLock.Unlock()
}

func TestReplacedSessionEventsCannotChangeConnection(t *testing.T) {
	closed := make(chan struct{}, 2)
	useConnectionTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		serveResumedConnection(w, r)
		closed <- struct{}{}
	})
	u := newConnectionTestUser(t)
	require.NoError(t, u.Connect())
	oldSession := u.Session
	u.reconnectLock.Lock()
	oldGeneration := u.sessionGeneration
	u.reconnectLock.Unlock()

	require.NoError(t, u.Connect())
	require.NotSame(t, oldSession, u.Session)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("replacing the session left the previous gateway socket open")
	}
	for _, evt := range []any{&discordgo.Disconnect{}, &discordgo.Resumed{}, &discordgo.Ready{}, &discordgo.InvalidAuth{}} {
		oldSession.EventHandler(evt)
	}
	// Also exercise work already queued before replacement, after its event
	// callback accepted the old generation but before Logout acquired the lock.
	u.logoutInvalidSession(oldGeneration)

	assert.True(t, u.Connected())
	assert.True(t, u.IsLoggedIn(), "an old invalid-auth callback must not clear the new session's token")
	u.reconnectLock.Lock()
	assert.Nil(t, u.reconnectTimer)
	assert.False(t, u.manualDisconnect)
	u.reconnectLock.Unlock()
}

func TestFailedOpenRejectsLateReady(t *testing.T) {
	useConnectionTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	})
	u := newConnectionTestUser(t)
	require.Error(t, u.Connect())

	u.Session.EventHandler(&discordgo.Resumed{})

	assert.False(t, u.Connected())
	u.reconnectLock.Lock()
	assert.Nil(t, u.stableTimer, "a failed Open cannot start a stability interval")
	u.reconnectLock.Unlock()
}

func TestOpenErrorClearsSynchronousReady(t *testing.T) {
	useConnectionTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		// Open dispatches this event before checking that the first gateway
		// frame was HELLO. It must then reject Op0 and withdraw the readiness
		// that its callback already established during the failed handshake.
		_ = conn.WriteJSON(map[string]any{"op": 0, "t": "RESUMED", "s": 1, "d": map[string]any{}})
	})
	u := newConnectionTestUser(t)

	err := u.Connect()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expecting Op 10, got Op 0")
	assert.False(t, u.Connected(), "a success event inside Open cannot override its eventual failure")
	u.reconnectLock.Lock()
	assert.Nil(t, u.stableTimer)
	assert.Nil(t, u.readyWatchdog)
	u.reconnectLock.Unlock()
}

func TestStaleRecoveryCannotAffectReplacement(t *testing.T) {
	var healthy atomic.Bool
	var attempts atomic.Int32
	useConnectionTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if healthy.Load() {
			serveResumedConnection(w, r)
		} else {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		}
	})
	u := newConnectionTestUser(t)
	u.startupTryConnect()
	oldSession := u.Session
	u.reconnectLock.Lock()
	oldGeneration, oldEpoch := u.sessionGeneration, u.connectionEpoch
	u.reconnectLock.Unlock()
	healthy.Store(true)
	require.NoError(t, u.Connect())
	u.reconnectLock.Lock()
	stableTimer := u.stableTimer
	u.reconnectLock.Unlock()

	u.doReconnect(oldGeneration, oldEpoch)
	u.onReadyTimeout(oldSession, oldGeneration, oldEpoch)
	u.onConnectionStable(oldEpoch)

	assert.EqualValues(t, 2, attempts.Load(), "a stale retry must not create a third connection")
	assert.True(t, u.Connected())
	u.reconnectLock.Lock()
	assert.Same(t, stableTimer, u.stableTimer, "stale timers must not clear the new session's timer references")
	assert.Nil(t, u.reconnectTimer)
	u.reconnectLock.Unlock()
}

func TestStartupRetriesRemainCappedAndCancelable(t *testing.T) {
	var attempts atomic.Int32
	useConnectionTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	})
	u := newConnectionTestUser(t)
	u.startupTryConnect()
	for failure := 1; failure <= 12; failure++ {
		u.reconnectLock.Lock()
		timer := u.reconnectTimer
		generation, epoch := u.sessionGeneration, u.connectionEpoch
		count := u.reconnectAttempts
		if timer != nil {
			timer.Stop()
		}
		u.reconnectLock.Unlock()
		require.NotNil(t, timer, "recovery must continue past the former seven-attempt startup budget")
		require.Equal(t, failure, count)
		require.LessOrEqual(t, reconnectDelay(count), reconnectBackoffMax)
		// Fire the stopped timer's real callback without waiting through minutes
		// of backoff. Each callback still performs a real local Open attempt.
		u.doReconnect(generation, epoch)
	}
	require.EqualValues(t, 13, attempts.Load())
	u.reconnectLock.Lock()
	generation, epoch := u.sessionGeneration, u.connectionEpoch
	u.reconnectLock.Unlock()

	require.NoError(t, u.Disconnect())
	u.doReconnect(generation, epoch)

	assert.EqualValues(t, 13, attempts.Load(), "a timer already firing when the user disconnects must not reconnect")
	assert.True(t, u.IsLoggedIn(), "manual disconnect retains credentials")
	assert.False(t, u.Connected())
	u.reconnectLock.Lock()
	assert.Nil(t, u.reconnectTimer)
	assert.True(t, u.manualDisconnect)
	u.reconnectLock.Unlock()
}

func TestQueuedStartupHonorsManualDisconnect(t *testing.T) {
	var attempts atomic.Int32
	useConnectionTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "unexpected connection", http.StatusServiceUnavailable)
	})
	u := newConnectionTestUser(t)
	require.ErrorIs(t, u.Disconnect(), ErrNotConnected)

	u.startupTryConnect()

	assert.Zero(t, attempts.Load())
	assert.True(t, u.IsLoggedIn())
	assert.False(t, u.Connected())
}

func TestLogoutCancelsStartupRecovery(t *testing.T) {
	var attempts atomic.Int32
	useConnectionTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	})
	u := newConnectionTestUser(t)
	u.startupTryConnect()
	u.reconnectLock.Lock()
	generation, epoch := u.sessionGeneration, u.connectionEpoch
	u.reconnectLock.Unlock()

	u.Logout(false)
	u.doReconnect(generation, epoch)

	assert.EqualValues(t, 1, attempts.Load())
	assert.False(t, u.IsLoggedIn())
	assert.False(t, u.Connected())
	u.reconnectLock.Lock()
	assert.Nil(t, u.reconnectTimer)
	u.reconnectLock.Unlock()
}
