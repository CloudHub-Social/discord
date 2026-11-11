package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-discord/config"
	"go.mau.fi/mautrix-discord/database"
)

func TestVoiceMirrorPublishesAndClears(t *testing.T) {
	updates := make(chan map[string]any, 10)
	var viewersMu sync.Mutex
	otherViewer := false
	failClearOnce := false
	failStateOnce := true
	power := map[string]any{"users": map[string]any{"@bridge:example.com": float64(100), "@discord_3:example.com": float64(1)}, "events": map[string]any{discordVoiceEventType: float64(1)}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("mock homeserver request: %s %s", r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/joined_members"):
			viewersMu.Lock()
			joined := map[string]any{"@owner:example.com": map[string]any{}, "@bridge:example.com": map[string]any{}, "@discord_2:example.com": map[string]any{}, "@discord_3:example.com": map[string]any{}}
			if otherViewer {
				joined["@other:example.com"] = map[string]any{}
			}
			viewersMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"joined": joined})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/state"):
			viewersMu.Lock()
			fail := failStateOnce
			failStateOnce = false
			viewersMu.Unlock()
			if fail {
				http.Error(w, `{"errcode":"M_FORBIDDEN"}`, http.StatusForbidden)
				return
			}
			members := []map[string]any{}
			for _, mxid := range []string{"@owner:example.com", "@bridge:example.com", "@discord_2:example.com", "@discord_3:example.com"} {
				members = append(members, map[string]any{"type": "m.room.member", "room_id": "!test:example.com", "state_key": mxid, "sender": mxid, "event_id": "$member", "content": map[string]any{"membership": "join"}})
			}
			ghost := "@discord_3:example.com"
			members = append(members, map[string]any{"type": "org.matrix.msc3401.call.member", "state_key": discordVoiceMembershipStateKey(ghost, "1"), "sender": ghost, "event_id": "$stale", "content": discordVoiceMembershipContent(time.Now(), "1")})
			_ = json.NewEncoder(w).Encode(members)
		case strings.Contains(r.URL.Path, "/state/m.room.power_levels"):
			viewersMu.Lock()
			defer viewersMu.Unlock()
			if r.Method == http.MethodPut {
				if json.NewDecoder(r.Body).Decode(&power) != nil {
					http.Error(w, "invalid", 400)
					return
				}
				_, _ = w.Write([]byte(`{"event_id":"$power"}`))
			} else {
				_ = json.NewEncoder(w).Encode(power)
			}
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/state/org.matrix.msc3401.call.member/"):
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				http.Error(w, "invalid", 400)
				return
			}
			viewersMu.Lock()
			levels := power["users"].(map[string]any)
			sender := r.URL.Query().Get("user_id")
			level, _ := levels[sender].(float64)
			required, ok := power["events"].(map[string]any)[discordVoiceEventType].(float64)
			if !ok {
				required = 50
			}
			if level < required {
				viewersMu.Unlock()
				http.Error(w, `{"errcode":"M_FORBIDDEN"}`, http.StatusForbidden)
				return
			}
			fail := len(body) == 0 && failClearOnce
			if fail {
				failClearOnce = false
			}
			viewersMu.Unlock()
			if fail {
				http.Error(w, `{"errcode":"M_FORBIDDEN"}`, http.StatusForbidden)
				return
			}
			updates <- body
			_, _ = w.Write([]byte(`{"event_id":"$event"}`))
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	as := appservice.Create()
	as.HomeserverDomain = "example.com"
	as.Registration = &appservice.Registration{SenderLocalpart: "bridge", AppToken: "test"}
	require.NoError(t, as.SetHomeserverURL(server.URL))
	cfg := &config.Config{BaseConfig: &bridgeconfig.BaseConfig{}}
	cfg.Homeserver.Domain = "example.com"
	require.NoError(t, yaml.Unmarshal([]byte("username_template: discord_{{.}}\n"), &cfg.Bridge))
	cfg.Bridge.DiscordVoiceActivity = true
	br := &DiscordBridge{Config: cfg, puppets: make(map[string]*Puppet), portalsByID: make(map[database.PortalKey]*Portal)}
	br.AS = as
	br.Bot = as.BotIntent()
	roomID := id.RoomID("!test:example.com")
	ghostID := id.UserID("@discord_2:example.com")
	staleGhostID := id.UserID("@discord_3:example.com")
	as.StateStore.MarkRegistered(ghostID)
	as.StateStore.MarkRegistered(staleGhostID)
	as.StateStore.SetMembership(roomID, ghostID, event.MembershipJoin)
	as.StateStore.SetMembership(roomID, br.Bot.UserID, event.MembershipJoin)
	key := database.NewPortalKey("20", "")
	br.portalsByID[key] = &Portal{Portal: &database.Portal{Key: key, MXID: roomID, Type: discordgo.ChannelTypeGuildVoice}, bridge: br}
	br.puppets["2"] = &Puppet{Puppet: &database.Puppet{ID: "2"}, MXID: ghostID, bridge: br}
	br.puppets["3"] = &Puppet{Puppet: &database.Puppet{ID: "3"}, MXID: staleGhostID, bridge: br}
	session, err := discordgo.New("test-token")
	require.NoError(t, err)
	require.NoError(t, session.State.GuildAdd(&discordgo.Guild{ID: "10", Roles: []*discordgo.Role{{ID: "10", Permissions: discordgo.PermissionViewChannel}}, Members: []*discordgo.Member{{User: &discordgo.User{ID: "1"}}}, Channels: []*discordgo.Channel{{ID: "20", GuildID: "10", Type: discordgo.ChannelTypeGuildVoice}}, VoiceStates: []*discordgo.VoiceState{{GuildID: "10", ChannelID: "20", UserID: "2"}}}))
	user := &User{User: &database.User{MXID: "@owner:example.com", DiscordID: "1"}, bridge: br, Session: session, voiceMirrorWake: make(chan struct{}, 1), voiceMirrorPublished: make(map[voiceMembership]time.Time), voiceMirrorPending: make(map[voiceMembership]struct{})}
	require.False(t, user.restoreDiscordVoicePortals(0, 0, "1", snapshotDiscordVoiceMemberships(session.State, "1"), []*Portal{br.portalsByID[key]}), "failed state reads must remain retryable")
	require.True(t, user.restoreDiscordVoicePortals(0, 0, "1", snapshotDiscordVoiceMemberships(session.State, "1"), []*Portal{br.portalsByID[key]}))
	require.Contains(t, user.voiceMirrorPending, voiceMembership{channelID: "20", userID: "3"}, "reconnect must recover stale bridge-owned membership")
	done := make(chan struct{})
	go func() { defer close(done); user.runDiscordVoiceMirror(0, 0) }()
	t.Cleanup(func() {
		user.reconnectLock.Lock()
		user.manualDisconnect = true
		user.reconnectLock.Unlock()
		select {
		case user.voiceMirrorWake <- struct{}{}:
		default:
		}
		<-done
	})
	next := func() map[string]any {
		t.Helper()
		select {
		case body := <-updates:
			return body
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for membership")
			return nil
		}
	}
	require.Empty(t, next(), "recovered stale membership is cleared on startup")
	require.Equal(t, "m.call", next()["application"])
	viewersMu.Lock()
	otherViewer = true
	viewersMu.Unlock()
	user.voiceMirrorWake <- struct{}{}
	require.Empty(t, next(), "a new Matrix viewer causes the published membership to clear")
	viewersMu.Lock()
	otherViewer = false
	viewersMu.Unlock()
	user.voiceMirrorWake <- struct{}{}
	require.Equal(t, "m.call", next()["application"])
	update := &discordgo.VoiceStateUpdate{VoiceState: &discordgo.VoiceState{GuildID: "10", UserID: "2"}}
	require.NoError(t, session.State.OnInterface(session, update))
	viewersMu.Lock()
	failClearOnce = true
	viewersMu.Unlock()
	user.clearDiscordVoiceDeparture(update, 0, 0)
	user.voiceMirrorWake <- struct{}{}
	require.Empty(t, next(), "a failed departure clear is retried on reconciliation")
}

func TestVoiceStateSendRegistersAndJoinsNewGhost(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/register"):
			requests = append(requests, "register")
			_, _ = w.Write([]byte(`{"user_id":"@discord_2:example.com"}`))
		case strings.HasSuffix(r.URL.Path, "/join"):
			requests = append(requests, "join")
			_, _ = w.Write([]byte(`{"room_id":"!test:example.com"}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/state/org.matrix.msc3401.call.member/"):
			requests = append(requests, "state")
			_, _ = w.Write([]byte(`{"event_id":"$event"}`))
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	as := appservice.Create()
	as.HomeserverDomain = "example.com"
	as.Registration = &appservice.Registration{SenderLocalpart: "bridge", AppToken: "test"}
	require.NoError(t, as.SetHomeserverURL(server.URL))
	ghost := as.Intent("@discord_2:example.com")
	room := id.RoomID("!test:example.com")
	typ := event.Type{Type: discordVoiceEventType, Class: event.StateEventType}
	_, err := ghost.SendStateEvent(room, typ, discordVoiceMembershipStateKey(string(ghost.UserID), "1"), discordVoiceMembershipContent(time.Now(), "1"))
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"register", "join", "state"}, requests)
}
