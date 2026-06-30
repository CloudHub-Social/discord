package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/bridge/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"

	"go.mau.fi/mautrix-discord/database"
)

var (
	ErrNotConnected = errors.New("not connected")
	ErrNotLoggedIn  = errors.New("not logged in")
)

type User struct {
	*database.User

	sync.Mutex

	bridge *DiscordBridge
	log    zerolog.Logger

	PermissionLevel bridgeconfig.PermissionLevel

	spaceCreateLock          sync.Mutex
	spaceMembershipChecked   bool
	dmSpaceMembershipChecked bool

	Session *discordgo.Session

	// subscribingGuilds guards against overlapping subscribeGuilds runs. Both
	// readyHandler (connect) and resumeHandler (resume) launch subscribeGuilds,
	// and on a resume the *Session pointer is unchanged, so the in-loop session
	// re-check does not stop a stale run. Without this guard a flapping
	// connection can stack concurrent goroutines, each emitting opcode-14
	// commands, and blow Discord's 120-commands/60s gateway budget.
	subscribingGuilds atomic.Bool

	// guildSubLock guards activeGuildSubs, the on-demand guild presence
	// subscription set. It maps a Discord guild ID to the last time a Matrix
	// client was active in one of its rooms; entries are added when a room is
	// focused, refreshed on continued activity, and released when they go idle
	// or are evicted to honor DiscordPresenceActiveLimit.
	guildSubLock    sync.Mutex
	activeGuildSubs map[string]time.Time

	BridgeState     *bridge.BridgeStateQueue
	bridgeStateLock sync.Mutex
	wasDisconnected bool
	wasLoggedOut    bool

	// Gateway reconnection is owned by the bridge rather than discordgo
	// (session.ShouldReconnectOnError is disabled in Connect). reconnectLock
	// guards the fields below. reconnectAttempts counts consecutive failures
	// since the last successful READY and drives the exponential backoff in
	// scheduleReconnect; it resets to 0 on READY. reconnectTimer is the pending
	// backoff timer, if any. manualDisconnect latches when the bridge tears the
	// session down on purpose (logout / manual disconnect / shutdown) so the
	// resulting Disconnect event does not trigger a reconnect.
	reconnectLock     sync.Mutex
	reconnectAttempts int
	reconnectTimer    *time.Timer
	manualDisconnect  bool
	// readyWatchdog force-closes a session that connects but never reaches
	// READY within readyTimeout. discordgo answers a Discord Op9 Invalid Session
	// by re-IDENTIFYing in-band (no close, no Disconnect event), so without this
	// a rejected session would re-IDENTIFY at gateway speed and never reach the
	// backoff path. Closing it routes the failure through disconnectedHandler.
	readyWatchdog *time.Timer
	// sessionReady is true once the current session reached READY or RESUMED. It
	// lets onReadyTimeout bail if the handshake completed concurrently with the
	// watchdog firing, so a valid session is never closed. Reset to false when a
	// new connect arms the watchdog.
	sessionReady bool

	markedOpened     map[string]time.Time
	markedOpenedLock sync.Mutex

	pendingInteractions     map[string]*WrappedCommandEvent
	pendingInteractionsLock sync.Mutex

	nextDiscordUploadID atomic.Int32

	relationships map[string]*discordgo.Relationship
	// relationshipsReady should be protected by relationshipLock and is merely
	// used to cover the brief moment in time where the readyHandler goroutine
	// is being scheduled; during that time, the relationships map is unlocked
	// and "available" but not logically "ready" just yet.
	relationshipsReady bool
	relationshipLock   sync.RWMutex

	// presenceLock protects the status-sync fields and presenceCache below,
	// all accessed concurrently from the discordgo event goroutine and the
	// appservice EventProcessor goroutine.
	presenceLock sync.Mutex

	// Status text sync state. lastMatrixStatusText is the last non-empty
	// status_msg received from Matrix. lastDiscordStatusText is the last
	// custom status text seen from Discord. matrixStatusEverSet tracks
	// whether Matrix has ever sent a status_msg this session (so we can
	// distinguish "never set" from "intentionally cleared").
	lastMatrixStatusText  string
	lastDiscordStatusText string
	matrixStatusEverSet   bool

	// lastSentDiscordStatus and lastSentDiscordStatusText track what was most
	// recently sent to Discord via UpdateStatusComplex. Used to suppress
	// redundant WebSocket status updates when the effective status hasn't
	// changed (Matrix clients send presence pings frequently, and each one
	// would otherwise generate a Discord opcode-3 message).
	lastSentDiscordStatus     string
	lastSentDiscordStatusText string

	// presenceCache maps Discord user IDs to their last known non-offline
	// Matrix presence state. The keepalive goroutine re-sends these every
	// presenceKeepaliveInterval so Synapse does not expire ghost users'
	// presence (ghosts never /sync, so presence times out without refresh).
	presenceCache map[string]presenceCacheEntry

	// discordPresenceSetAt is the timestamp of the last time applyPresence
	// successfully updated the bridge user's own real Matrix account from a
	// Discord status change. HandleMatrixPresence uses it to suppress the
	// resulting Synapse push_ephemeral echo for 2 seconds, preventing the
	// status from bouncing back to Discord (e.g., DND→unavailable→idle).
	// Also set by runPresenceKeepalive when it re-asserts own-user presence,
	// so the resulting push_ephemeral echo is suppressed without a Discord call.
	// Only set on success so a failed PUT doesn't block legitimate echoes.
	discordPresenceSetAt time.Time
	// matrixPresenceSetAt is the timestamp of the last time HandleMatrixPresence
	// successfully forwarded a Matrix presence change to Discord. applyPresence
	// uses it, together with lastSentToDiscordStatus/Text, to detect Discord
	// echoes of Matrix-originated status changes and skip overwriting the
	// Matrix-side state (which may have a [dnd] prefix that was stripped before
	// forwarding to Discord).
	matrixPresenceSetAt time.Time
	// lastSentToDiscordStatus / lastSentToDiscordText record the Discord status
	// string and custom text that HandleMatrixPresence last successfully sent via
	// UpdateStatusComplex. applyPresence checks these alongside matrixPresenceSetAt
	// to narrow the echo window: a Discord status change within the 2-second window
	// is only treated as an echo when its values match what was just sent,
	// preventing a genuine deliberate change from being silently dropped.
	lastSentToDiscordStatus string
	lastSentToDiscordText   string
	// lastOwnMatrixPresence / lastOwnMatrixStatusMsg cache the last presence
	// state written to the user's own real Matrix account from Discord, or
	// pre-armed by the applyPresence PUT path before the HTTP call. applyPresence
	// skips redundant writes when neither value has changed (dedup), and
	// HandleMatrixPresence uses them for value-matched echo suppression.
	// Not cleared on reconnect: seedPresences skips own-account applyPresence
	// calls to avoid clobbering Charm-side state set while the bridge was down.
	lastOwnMatrixPresence  event.Presence
	lastOwnMatrixStatusMsg string

	// Presence rate limiting (Matrix→Discord), protected by presenceLock.
	// Discord invalidates user-account tokens that emit opcode-3 presence
	// updates at machine cadence, and Matrix presence is a high-frequency
	// signal (clients oscillate online/idle constantly). Forwarding is
	// throttled to at most one update per presenceMinInterval: a change
	// outside the cooldown is sent immediately, while changes inside it are
	// coalesced into pendingDiscordStatus/Text and flushed by a single
	// trailing timer (presenceDebounceTimer) when the cooldown expires.
	// lastDiscordPresenceSentAt is when an opcode-3 update was last dispatched.
	lastDiscordPresenceSentAt time.Time
	presenceDebounceTimer     *time.Timer
	pendingDiscordStatus      string
	pendingDiscordStatusText  string

	// stopKeepalive, when non-nil, cancels the presence keepalive goroutine.
	// Protected by user.Lock().
	stopKeepalive func()
}

func (user *User) GetRemoteID() string {
	return user.DiscordID
}

func (user *User) GetRemoteName() string {
	if user.Session != nil && user.Session.State != nil && user.Session.State.User != nil {
		if user.Session.State.User.Discriminator == "0" {
			return fmt.Sprintf("@%s", user.Session.State.User.Username)
		}
		return fmt.Sprintf("%s#%s", user.Session.State.User.Username, user.Session.State.User.Discriminator)
	}
	return user.DiscordID
}

var discordLog zerolog.Logger

func discordToZeroLevel(level int) zerolog.Level {
	switch level {
	case discordgo.LogError:
		return zerolog.ErrorLevel
	case discordgo.LogWarning:
		return zerolog.WarnLevel
	case discordgo.LogInformational:
		return zerolog.InfoLevel
	case discordgo.LogDebug:
		fallthrough
	default:
		return zerolog.DebugLevel
	}
}

func init() {
	discordgo.Logger = func(msgL, caller int, format string, a ...interface{}) {
		discordLog.WithLevel(discordToZeroLevel(msgL)).Caller(caller+1).Msgf(strings.TrimSpace(format), a...) // zerolog-allow-msgf
	}
}

func (user *User) GetPermissionLevel() bridgeconfig.PermissionLevel {
	return user.PermissionLevel
}

func (user *User) GetManagementRoomID() id.RoomID {
	return user.ManagementRoom
}

func (user *User) GetMXID() id.UserID {
	return user.MXID
}

func (user *User) GetCommandState() map[string]interface{} {
	return nil
}

func (user *User) GetIDoublePuppet() bridge.DoublePuppet {
	p := user.bridge.GetPuppetByCustomMXID(user.MXID)
	if p == nil || p.CustomIntent() == nil {
		return nil
	}
	return p
}

func (user *User) GetIGhost() bridge.Ghost {
	if user.DiscordID == "" {
		return nil
	}
	p := user.bridge.GetPuppetByID(user.DiscordID)
	if p == nil {
		return nil
	}
	return p
}

var _ bridge.User = (*User)(nil)

func (br *DiscordBridge) loadUser(dbUser *database.User, mxid *id.UserID) *User {
	if dbUser == nil {
		if mxid == nil {
			return nil
		}
		dbUser = br.DB.User.New()
		dbUser.MXID = *mxid
		dbUser.Insert()
	}

	user := br.NewUser(dbUser)
	br.usersByMXID[user.MXID] = user
	if user.DiscordID != "" {
		br.usersByID[user.DiscordID] = user
	}
	if user.ManagementRoom != "" {
		br.managementRoomsLock.Lock()
		br.managementRooms[user.ManagementRoom] = user
		br.managementRoomsLock.Unlock()
	}
	return user
}

// GetUserByMXIDIfExists returns the bridge user for a Matrix user ID if one
// already exists in the in-memory cache or the database, without creating a
// new row. Returns nil when no bridge user has ever been created for this MXID.
func (br *DiscordBridge) GetUserByMXIDIfExists(userID id.UserID) *User {
	if userID == br.Bot.UserID || br.IsGhost(userID) {
		return nil
	}
	br.usersLock.Lock()
	defer br.usersLock.Unlock()

	user, ok := br.usersByMXID[userID]
	if !ok {
		// Pass nil for the mxid parameter so loadUser does not insert a new row
		// when the DB returns nil.
		return br.loadUser(br.DB.User.GetByMXID(userID), nil)
	}
	return user
}

func (br *DiscordBridge) GetUserByMXID(userID id.UserID) *User {
	if userID == br.Bot.UserID || br.IsGhost(userID) {
		return nil
	}
	br.usersLock.Lock()
	defer br.usersLock.Unlock()

	user, ok := br.usersByMXID[userID]
	if !ok {
		return br.loadUser(br.DB.User.GetByMXID(userID), &userID)
	}
	return user
}

func (br *DiscordBridge) GetUserByID(id string) *User {
	br.usersLock.Lock()
	defer br.usersLock.Unlock()

	user, ok := br.usersByID[id]
	if !ok {
		return br.loadUser(br.DB.User.GetByID(id), nil)
	}
	return user
}

func (br *DiscordBridge) GetCachedUserByID(id string) *User {
	br.usersLock.Lock()
	defer br.usersLock.Unlock()
	return br.usersByID[id]
}

func (br *DiscordBridge) GetCachedUserByMXID(userID id.UserID) *User {
	br.usersLock.Lock()
	defer br.usersLock.Unlock()
	return br.usersByMXID[userID]
}

// presenceCacheEntry holds the last known non-offline Matrix presence state
// for a Discord user, used by the keepalive goroutine.
type presenceCacheEntry struct {
	presence  event.Presence
	statusMsg string
}

// presenceKeepaliveInterval is how often the keepalive goroutine re-sends
// presence for non-offline ghost users. Synapse's presence sweeper runs every
// ~5s and can override an explicitly-set presence if the virtual user's
// last_user_sync_ts is very old; 10s re-asserts before a second sweep fires.
const presenceKeepaliveInterval = 10 * time.Second

// presenceMinInterval is the minimum time between opcode-3 presence updates
// forwarded to Discord for a single user. HandleMatrixPresence can fire many
// times per minute as the user's Matrix clients oscillate online/idle, and
// Discord flags user-account tokens that update presence at machine cadence
// (closing the gateway with 4004 / invalid auth). Throttling to one update per
// minute keeps the bridge well under the anti-automation threshold; changes
// arriving within the interval are coalesced and flushed by a trailing timer.
const presenceMinInterval = 60 * time.Second

func (br *DiscordBridge) NewUser(dbUser *database.User) *User {
	user := &User{
		User:   dbUser,
		bridge: br,
		log:    br.ZLog.With().Str("user_id", string(dbUser.MXID)).Logger(),

		markedOpened:    make(map[string]time.Time),
		PermissionLevel: br.Config.Bridge.Permissions.Get(dbUser.MXID),

		pendingInteractions: make(map[string]*WrappedCommandEvent),

		relationships:   make(map[string]*discordgo.Relationship),
		presenceCache:   make(map[string]presenceCacheEntry),
		activeGuildSubs: make(map[string]time.Time),
	}
	user.nextDiscordUploadID.Store(rand.Int31n(100))
	user.BridgeState = br.NewBridgeStateQueue(user)
	return user
}

func (br *DiscordBridge) getAllUsersWithToken() []*User {
	br.usersLock.Lock()
	defer br.usersLock.Unlock()

	dbUsers := br.DB.User.GetAllWithToken()
	users := make([]*User, len(dbUsers))

	for idx, dbUser := range dbUsers {
		user, ok := br.usersByMXID[dbUser.MXID]
		if !ok {
			user = br.loadUser(dbUser, nil)
		}
		users[idx] = user
	}
	return users
}

func (br *DiscordBridge) startUsers() {
	br.ZLog.Debug().Msg("Starting users")

	usersWithToken := br.getAllUsersWithToken()
	for _, u := range usersWithToken {
		go u.startupTryConnect(0)
	}
	if len(usersWithToken) == 0 {
		br.SendGlobalBridgeState(status.BridgeState{StateEvent: status.StateUnconfigured}.Fill(nil))
	}

	br.ZLog.Debug().Msg("Starting custom puppets")
	for _, customPuppet := range br.GetAllPuppetsWithCustomMXID() {
		go func(puppet *Puppet) {
			br.ZLog.Debug().Str("user_id", puppet.CustomMXID.String()).Msg("Starting custom puppet")

			if err := puppet.StartCustomMXID(true); err != nil {
				puppet.log.Error().Err(err).Msg("Failed to start custom puppet")
			}
		}(customPuppet)
	}
}

func (user *User) startupTryConnect(retryCount int) {
	user.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
	err := user.Connect()
	if err != nil {
		user.log.Error().Err(err).Msg("Error connecting on startup")
		closeErr := &websocket.CloseError{}
		if errors.As(err, &closeErr) && closeErr.Code == 4004 {
			user.invalidAuthHandler(nil)
		} else if retryCount < 6 {
			user.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Error: "dc-unknown-websocket-error", Message: err.Error()})
			retryInSeconds := 2 << retryCount
			user.log.Debug().Int("retry_in_seconds", retryInSeconds).Msg("Sleeping and retrying connection")
			time.Sleep(time.Duration(retryInSeconds) * time.Second)
			user.startupTryConnect(retryCount + 1)
		} else {
			user.BridgeState.Send(status.BridgeState{StateEvent: status.StateUnknownError, Error: "dc-unknown-websocket-error", Message: err.Error()})
		}
	}
}

func (user *User) SetManagementRoom(roomID id.RoomID) {
	user.bridge.managementRoomsLock.Lock()
	defer user.bridge.managementRoomsLock.Unlock()

	existing, ok := user.bridge.managementRooms[roomID]
	if ok {
		existing.ManagementRoom = ""
		existing.Update()
	}

	user.ManagementRoom = roomID
	user.bridge.managementRooms[user.ManagementRoom] = user
	user.Update()
}

func (user *User) getSpaceRoom(ptr *id.RoomID, name, topic string, parent id.RoomID) id.RoomID {
	if len(*ptr) > 0 {
		return *ptr
	}
	user.spaceCreateLock.Lock()
	defer user.spaceCreateLock.Unlock()
	if len(*ptr) > 0 {
		return *ptr
	}

	initialState := []*event.Event{{
		Type: event.StateRoomAvatar,
		Content: event.Content{
			Parsed: &event.RoomAvatarEventContent{
				URL: user.bridge.Config.AppService.Bot.ParsedAvatar,
			},
		},
	}}

	if parent != "" {
		parentIDStr := parent.String()
		initialState = append(initialState, &event.Event{
			Type:     event.StateSpaceParent,
			StateKey: &parentIDStr,
			Content: event.Content{
				Parsed: &event.SpaceParentEventContent{
					Canonical: true,
					Via:       []string{user.bridge.AS.HomeserverDomain},
				},
			},
		})
	}

	resp, err := user.bridge.Bot.CreateRoom(&mautrix.ReqCreateRoom{
		Visibility:   "private",
		Name:         name,
		Topic:        topic,
		InitialState: initialState,
		CreationContent: map[string]interface{}{
			"type": event.RoomTypeSpace,
		},
		PowerLevelOverride: &event.PowerLevelsEventContent{
			Users: map[id.UserID]int{
				user.bridge.Bot.UserID: 9001,
				user.MXID:              50,
			},
		},
		RoomVersion: "11",
	})

	if err != nil {
		user.log.Error().Err(err).Msg("Failed to auto-create space room")
	} else {
		*ptr = resp.RoomID
		user.Update()
		user.ensureInvited(nil, *ptr, false, true)

		if parent != "" {
			_, err = user.bridge.Bot.SendStateEvent(parent, event.StateSpaceChild, resp.RoomID.String(), &event.SpaceChildEventContent{
				Via:   []string{user.bridge.AS.HomeserverDomain},
				Order: " 0000",
			})
			if err != nil {
				user.log.Error().Err(err).
					Str("created_space_id", resp.RoomID.String()).
					Str("parent_space_id", parent.String()).
					Msg("Failed to add created space room to parent space")
			}
		}
	}
	return *ptr
}

func (user *User) GetSpaceRoom() id.RoomID {
	return user.getSpaceRoom(&user.SpaceRoom, "Discord", "Your Discord bridged chats", "")
}

func (user *User) GetDMSpaceRoom() id.RoomID {
	return user.getSpaceRoom(&user.DMSpaceRoom, "Direct Messages", "Your Discord direct messages", user.GetSpaceRoom())
}

func (user *User) ViewingChannel(portal *Portal) bool {
	if portal.GuildID != "" || !user.Session.IsUser {
		return false
	}
	user.markedOpenedLock.Lock()
	defer user.markedOpenedLock.Unlock()
	ts := user.markedOpened[portal.Key.ChannelID]
	// TODO is there an expiry time?
	if ts.IsZero() {
		user.markedOpened[portal.Key.ChannelID] = time.Now()
		err := user.Session.MarkViewing(portal.Key.ChannelID)
		if err != nil {
			user.log.Error().Err(err).
				Str("channel_id", portal.Key.ChannelID).
				Msg("Failed to mark user as viewing channel")
		}
		return true
	}
	return false
}

func (user *User) mutePortal(intent *appservice.IntentAPI, portal *Portal, unmute bool) {
	if len(portal.MXID) == 0 || !user.bridge.Config.Bridge.MuteChannelsOnCreate {
		return
	}
	var err error
	if unmute {
		user.log.Debug().Str("room_id", portal.MXID.String()).Msg("Unmuting portal")
		err = intent.DeletePushRule("global", pushrules.RoomRule, string(portal.MXID))
	} else {
		user.log.Debug().Str("room_id", portal.MXID.String()).Msg("Muting portal")
		err = intent.PutPushRule("global", pushrules.RoomRule, string(portal.MXID), &mautrix.ReqPutPushRule{
			Actions: []pushrules.PushActionType{pushrules.ActionDontNotify},
		})
	}
	if err != nil && !errors.Is(err, mautrix.MNotFound) {
		user.log.Warn().Err(err).
			Str("room_id", portal.MXID.String()).
			Msg("Failed to update push rule through double puppet")
	}
}

func (user *User) syncChatDoublePuppetDetails(portal *Portal, justCreated bool) {
	doublePuppetIntent := portal.bridge.GetPuppetByCustomMXID(user.MXID).CustomIntent()
	if doublePuppetIntent == nil || portal.MXID == "" {
		return
	}

	// TODO sync mute status properly
	if portal.GuildID != "" && user.bridge.Config.Bridge.MuteChannelsOnCreate && justCreated {
		user.mutePortal(doublePuppetIntent, portal, false)
	}
}

func (user *User) NextDiscordUploadID() string {
	val := user.nextDiscordUploadID.Add(2)
	return strconv.Itoa(int(val))
}

func (user *User) Login(token string) error {
	user.bridgeStateLock.Lock()
	user.wasLoggedOut = false
	user.bridgeStateLock.Unlock()
	user.DiscordToken = token
	var err error
	const maxRetries = 3
Loop:
	for i := 0; i < maxRetries; i++ {
		err = user.Connect()
		if err == nil {
			user.Update()
			return nil
		}
		user.log.Error().Err(err).Msg("Error connecting for login")
		closeErr := &websocket.CloseError{}
		errors.As(err, &closeErr)
		switch closeErr.Code {
		case 4004, 4010, 4011, 4012, 4013, 4014:
			break Loop
		case 4000:
			fallthrough
		default:
			if i < maxRetries-1 {
				time.Sleep(time.Duration(i+1) * 2 * time.Second)
			}
		}
	}
	user.DiscordToken = ""
	return err
}

func (user *User) IsLoggedIn() bool {
	user.Lock()
	defer user.Unlock()

	return user.DiscordToken != ""
}

func (user *User) Logout(isOverwriting bool) {
	user.Lock()
	defer user.Unlock()

	if user.DiscordID != "" {
		puppet := user.bridge.GetPuppetByID(user.DiscordID)
		if puppet.CustomMXID != "" {
			err := puppet.SwitchCustomMXID("", "")
			if err != nil {
				user.log.Warn().Err(err).Msg("Failed to disable custom puppet while logging out of Discord")
			}
		}
	}

	// Latch before Close() so the resulting Disconnect event does not reconnect.
	user.stopReconnecting()
	if user.Session != nil {
		if err := user.Session.Close(); err != nil {
			user.log.Warn().Err(err).Msg("Error closing session")
		}
	}

	user.Session = nil
	user.reconstructRelationships(nil)
	user.DiscordToken = ""
	user.ReadStateVersion = 0
	user.presenceLock.Lock()
	user.lastDiscordStatusText = ""
	user.lastMatrixStatusText = ""
	user.matrixStatusEverSet = false
	user.lastSentDiscordStatus = ""
	user.lastSentDiscordStatusText = ""
	user.discordPresenceSetAt = time.Time{}
	user.matrixPresenceSetAt = time.Time{}
	user.lastSentToDiscordStatus = ""
	user.lastSentToDiscordText = ""
	user.lastOwnMatrixPresence = ""
	user.lastOwnMatrixStatusMsg = ""
	if user.presenceDebounceTimer != nil {
		user.presenceDebounceTimer.Stop()
		user.presenceDebounceTimer = nil
	}
	user.pendingDiscordStatus = ""
	user.pendingDiscordStatusText = ""
	user.lastDiscordPresenceSentAt = time.Time{}
	clear(user.presenceCache)
	user.presenceLock.Unlock()
	user.guildSubLock.Lock()
	clear(user.activeGuildSubs)
	user.guildSubLock.Unlock()
	if user.stopKeepalive != nil {
		user.stopKeepalive()
		user.stopKeepalive = nil
	}
	if !isOverwriting {
		user.bridge.usersLock.Lock()
		if user.bridge.usersByID[user.DiscordID] == user {
			delete(user.bridge.usersByID, user.DiscordID)
		}
		user.bridge.usersLock.Unlock()
	}
	user.DiscordID = ""
	user.Update()
	user.log.Info().Msg("User logged out")
}

func (user *User) reconstructRelationships(relationships []*discordgo.Relationship) {
	user.relationshipLock.Lock()
	defer user.relationshipLock.Unlock()

	clear(user.relationships)

	if relationships == nil {
		// Relationships are just being cleared out; we don't actually have
		// them yet.
		user.relationshipsReady = false
	} else {
		// We've received the authoritative list of relationships from the
		// gateway.
		for _, relationship := range relationships {
			user.relationships[relationship.ID] = relationship
		}
		user.relationshipsReady = true
	}
}

func (user *User) Connected() bool {
	user.Lock()
	defer user.Unlock()

	return user.Session != nil
}

const BotIntents = discordgo.IntentGuilds |
	discordgo.IntentGuildMessages |
	discordgo.IntentGuildMessageReactions |
	discordgo.IntentGuildMessageTyping |
	discordgo.IntentGuildBans |
	discordgo.IntentGuildEmojis |
	discordgo.IntentGuildIntegrations |
	discordgo.IntentGuildInvites |
	//discordgo.IntentGuildVoiceStates |
	//discordgo.IntentGuildScheduledEvents |
	discordgo.IntentDirectMessages |
	discordgo.IntentDirectMessageTyping |
	discordgo.IntentDirectMessageTyping |
	// Privileged intents
	discordgo.IntentMessageContent |
	// IntentGuildPresences is a privileged intent that must be explicitly
	// enabled in the Discord developer portal for each bot application.
	// Omit it from BotIntents so bot-token logins don't get rejected with
	// close 4014 when the application hasn't enabled it.
	//discordgo.IntentGuildPresences |
	discordgo.IntentGuildMembers

// Connect establishes the Discord gateway connection on purpose (startup,
// login, or an explicit reconnect command). It clears the manual-disconnect
// latch so future drops reconnect normally.
func (user *User) Connect() error {
	return user.connect(true)
}

// connect performs the actual gateway connection. When intentional is true the
// caller is a deliberate connect and the manual-disconnect latch is cleared;
// when false the caller is the backoff-driven auto-reconnect, which must abort
// if a manual teardown latched concurrently (otherwise it could race a manual
// Disconnect and revive a session the user just closed).
func (user *User) connect(intentional bool) error {
	user.Lock()
	// Clear our in-memory relationship cache as it might've changed while
	// offline; READY will repopulate it.
	user.reconstructRelationships(nil)
	defer user.Unlock()

	if user.DiscordToken == "" {
		return ErrNotLoggedIn
	}

	user.reconnectLock.Lock()
	if intentional {
		user.manualDisconnect = false
		// A deliberate connect (startup / login / reconnect command) supersedes
		// any pending auto-reconnect: cancel its timer and reset the backoff so a
		// stale retry doesn't fire a second session on top of this one.
		user.reconnectAttempts = 0
		if user.reconnectTimer != nil {
			user.reconnectTimer.Stop()
			user.reconnectTimer = nil
		}
	} else if user.manualDisconnect {
		// A manual disconnect/logout latched while this auto-reconnect was in
		// flight; honor it and do not revive the session.
		user.reconnectLock.Unlock()
		return nil
	}
	user.reconnectLock.Unlock()

	user.log.Debug().Msg("Connecting to discord")

	session, err := discordgo.New(user.DiscordToken)
	if err != nil {
		return err
	}
	// Take ownership of reconnection. discordgo's built-in auto-reconnect calls
	// Open() in a loop with exponential backoff, but its Open() treats Discord's
	// Op9 Invalid Session as a non-fatal handshake result and returns success, so
	// the backoff never engages and a rejected session re-IDENTIFYs every ~2s
	// forever — exactly the kind of gateway spam that gets a user token killed.
	// Instead the bridge drives reconnection from disconnectedHandler via
	// scheduleReconnect, which backs off based on failures-since-last-READY.
	session.ShouldReconnectOnError = false

	if user.HeartbeatSession == nil || user.HeartbeatSession.IsExpired() {
		user.log.Debug().Msg("Creating new heartbeat session")
		sess := discordgo.NewHeartbeatSession()
		user.HeartbeatSession = &sess
	}
	user.HeartbeatSession.BumpLastUsed()
	user.Update()
	// make discordgo use our session instead of the one it creates automatically
	session.HeartbeatSession = *user.HeartbeatSession

	if user.bridge.Config.Bridge.Proxy != "" {
		u, _ := url.Parse(user.bridge.Config.Bridge.Proxy)
		tlsConf := &tls.Config{
			InsecureSkipVerify: os.Getenv("DISCORD_SKIP_TLS_VERIFICATION") == "true",
		}
		session.Client.Transport = &http.Transport{
			Proxy:             http.ProxyURL(u),
			TLSClientConfig:   tlsConf,
			ForceAttemptHTTP2: true,
		}
		session.Dialer.Proxy = http.ProxyURL(u)
		session.Dialer.TLSClientConfig = tlsConf
	}
	// TODO move to config
	if os.Getenv("DISCORD_DEBUG") == "1" {
		session.LogLevel = discordgo.LogDebug
	} else {
		session.LogLevel = discordgo.LogInformational
	}
	userDiscordLog := user.log.With().
		Str("component", "discordgo").
		Str("heartbeat_session", session.HeartbeatSession.ID.String()).
		Logger()
	session.Logger = func(msgL, caller int, format string, a ...interface{}) {
		userDiscordLog.WithLevel(discordToZeroLevel(msgL)).Caller(caller+1).Msgf(strings.TrimSpace(format), a...) // zerolog-allow-msgf
	}
	if !session.IsUser {
		session.Identify.Intents = BotIntents
	}
	session.EventHandler = user.eventHandlerSync

	if session.IsUser {
		err = session.LoadMainPage(context.TODO())
		if err != nil {
			user.log.Warn().Err(err).Msg("Failed to load main page")
		}
	}

	user.Session = session

	// New attempt: clear the ready flag before Open() so a stale value from a
	// previous session can't suppress this attempt's watchdog.
	user.reconnectLock.Lock()
	user.sessionReady = false
	user.reconnectLock.Unlock()

	for {
		err = user.Session.Open()
		if errors.Is(err, discordgo.ErrImmediateDisconnect) {
			// Op7 during the handshake. On an auto-reconnect, let it escape so the
			// scheduler backs off instead of looping here every 5s while holding
			// user.Lock (which would also block Logout/Disconnect).
			if !intentional {
				return err
			}
			user.log.Warn().Err(err).Msg("Retrying initial connection in 5 seconds")
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}
	if err == nil {
		// Socket is up. Arm the READY watchdog now — AFTER Open() — so its budget
		// measures time spent waiting for READY rather than the (possibly slow)
		// handshake itself; a large account or a slow proxy must not let the timer
		// expire mid-handshake and close a session that is about to succeed.
		// armReadyWatchdog skips arming if READY was already processed during
		// Open() (sessionReady set by readyHandler), avoiding a false close.
		// discordgo's Open() returns success even when the handshake got Op9
		// Invalid Session instead of READY, so without this a rejected session
		// would re-IDENTIFY in-band forever; the watchdog force-closes it and
		// routes the failure through the backoff path.
		user.armReadyWatchdog()
	}
	return err
}

func (user *User) eventHandlerSync(rawEvt any) {
	go user.eventHandler(rawEvt)
}

func (user *User) eventHandler(rawEvt any) {
	defer func() {
		err := recover()
		if err != nil {
			user.log.Error().
				Bytes(zerolog.ErrorStackFieldName, debug.Stack()).
				Any(zerolog.ErrorFieldName, err).
				Msg("Panic in Discord event handler")
		}
	}()
	switch evt := rawEvt.(type) {
	case *discordgo.Ready:
		user.readyHandler(evt)
	case *discordgo.Resumed:
		user.resumeHandler(evt)
	case *discordgo.Connect:
		user.connectedHandler(evt)
	case *discordgo.Disconnect:
		user.disconnectedHandler(evt)
	case *discordgo.InvalidAuth:
		user.invalidAuthHandler(evt)
	case *discordgo.GuildCreate:
		user.guildCreateHandler(evt)
	case *discordgo.GuildDelete:
		user.guildDeleteHandler(evt)
	case *discordgo.GuildUpdate:
		user.guildUpdateHandler(evt)
	case *discordgo.GuildRoleCreate:
		user.discordRoleToDB(evt.GuildID, evt.Role, nil, nil)
	case *discordgo.GuildRoleUpdate:
		user.discordRoleToDB(evt.GuildID, evt.Role, nil, nil)
	case *discordgo.GuildRoleDelete:
		user.bridge.DB.Role.DeleteByID(evt.GuildID, evt.RoleID)
	case *discordgo.ChannelCreate:
		user.channelCreateHandler(evt)
	case *discordgo.ChannelDelete:
		user.channelDeleteHandler(evt)
	case *discordgo.ChannelUpdate:
		user.channelUpdateHandler(evt)
	case *discordgo.ChannelRecipientAdd:
		user.channelRecipientAdd(evt)
	case *discordgo.ChannelRecipientRemove:
		user.channelRecipientRemove(evt)
	case *discordgo.RelationshipAdd:
		user.relationshipAddHandler(evt)
	case *discordgo.RelationshipRemove:
		user.relationshipRemoveHandler(evt)
	case *discordgo.RelationshipUpdate:
		user.relationshipUpdateHandler(evt)
	case *discordgo.MessageCreate:
		user.pushPortalMessage(evt, "message create", evt.ChannelID, evt.GuildID)
	case *discordgo.MessageDelete:
		user.pushPortalMessage(evt, "message delete", evt.ChannelID, evt.GuildID)
	case *discordgo.MessageDeleteBulk:
		user.pushPortalMessage(evt, "bulk message delete", evt.ChannelID, evt.GuildID)
	case *discordgo.MessageUpdate:
		user.pushPortalMessage(evt, "message update", evt.ChannelID, evt.GuildID)
	case *discordgo.MessageReactionAdd:
		user.pushPortalMessage(evt, "reaction add", evt.ChannelID, evt.GuildID)
	case *discordgo.MessageReactionRemove:
		user.pushPortalMessage(evt, "reaction remove", evt.ChannelID, evt.GuildID)
	case *discordgo.MessageAck:
		user.messageAckHandler(evt)
	case *discordgo.TypingStart:
		user.typingStartHandler(evt)
	case *discordgo.PresenceUpdate:
		user.presenceUpdateHandler(evt)
	case *discordgo.GuildMembersChunk:
		if len(evt.Presences) > 0 {
			go user.seedPresences(evt.Presences)
		}
	case *discordgo.PresencesReplace:
		// PRESENCES_REPLACE is sent by Discord to user-token sessions shortly
		// after READY to deliver an authoritative snapshot of all friend/DM
		// presences. Seeding from it ensures we don't miss updates that
		// arrived in the gap between the READY payload and now.
		presences := []*discordgo.Presence(*evt)
		if len(presences) > 0 {
			go user.seedPresences(presences)
		}
	case *discordgo.InteractionSuccess:
		user.interactionSuccessHandler(evt)
	case *discordgo.ThreadListSync:
		user.threadListSyncHandler(evt)
	case *discordgo.Event:
		// Ignore
	default:
		user.log.Debug().Type("event_type", evt).Msg("Unhandled event")
	}
}

func (user *User) Disconnect() error {
	user.Lock()
	defer user.Unlock()
	if user.Session == nil {
		return ErrNotConnected
	}

	user.log.Info().Msg("Disconnecting session manually")
	// Latch before Close() so the resulting Disconnect event does not reconnect.
	user.stopReconnecting()
	user.reconstructRelationships(nil)
	if err := user.Session.Close(); err != nil {
		return err
	}
	user.Session = nil
	if user.stopKeepalive != nil {
		user.stopKeepalive()
		user.stopKeepalive = nil
	}
	return nil
}

func (user *User) getGuildBridgingMode(guildID string) database.GuildBridgingMode {
	if guildID == "" {
		return database.GuildBridgeEverything
	}
	guild := user.bridge.GetGuildByID(guildID, false)
	if guild == nil {
		return database.GuildBridgeNothing
	}
	return guild.BridgingMode
}

type ChannelSlice []*discordgo.Channel

func (s ChannelSlice) Len() int {
	return len(s)
}

func (s ChannelSlice) Less(i, j int) bool {
	if s[i].Position != 0 || s[j].Position != 0 {
		return s[i].Position < s[j].Position
	}
	return compareMessageIDs(s[i].LastMessageID, s[j].LastMessageID) == 1
}

func (s ChannelSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (user *User) readyHandler(r *discordgo.Ready) {
	user.log.Debug().Msg("Discord connection ready")
	user.bridgeStateLock.Lock()
	user.wasLoggedOut = false
	user.bridgeStateLock.Unlock()
	user.onConnectionEstablished()
	// Clear the sent-presence dedup cache so the first Matrix presence ping
	// after a reconnect always reaches Discord (the new gateway session starts
	// without this Matrix-derived status).
	user.presenceLock.Lock()
	user.lastSentDiscordStatus = ""
	user.lastSentDiscordStatusText = ""
	user.presenceLock.Unlock()

	if user.DiscordID != r.User.ID {
		// Write DiscordID under user.Lock BEFORE taking usersLock. Logout() holds
		// user.Lock for its full duration and later acquires usersLock, so holding
		// usersLock while waiting for user.Lock would be an AB/BA deadlock.
		// Use a local to carry the new value into the usersLock block below.
		user.Lock()
		newDiscordID := r.User.ID
		user.DiscordID = newDiscordID
		user.Unlock()

		user.bridge.usersLock.Lock()
		if previousUser, ok := user.bridge.usersByID[newDiscordID]; ok && previousUser != user {
			user.log.Warn().
				Str("previous_user_id", previousUser.MXID.String()).
				Msg("Another user is logged in with same Discord ID, logging them out")
			// TODO send notice?
			previousUser.Logout(true)
		}
		user.bridge.usersByID[newDiscordID] = user
		user.bridge.usersLock.Unlock()
		user.Update()
	}
	user.BridgeState.Send(status.BridgeState{StateEvent: status.StateBackfilling})
	user.tryAutomaticDoublePuppeting()

	user.reconstructRelationships(r.Relationships)

	updateTS := time.Now()
	portalsInSpace := make(map[string]bool)
	for _, guild := range user.GetPortals() {
		portalsInSpace[guild.DiscordID] = guild.InSpace
	}
	for _, guild := range r.Guilds {
		user.handleGuild(guild, updateTS, portalsInSpace[guild.ID])
	}
	// The private channel list doesn't seem to be sorted by default, so sort it by message IDs (highest=newest first)
	sort.Sort(ChannelSlice(r.PrivateChannels))
	for i, ch := range r.PrivateChannels {
		portal := user.GetPortalByMeta(ch)
		user.handlePrivateChannel(portal, ch, updateTS, i < user.bridge.Config.Bridge.PrivateChannelCreateLimit, portalsInSpace[portal.Key.ChannelID])
	}
	user.PrunePortalList(updateTS)

	if r.ReadState != nil && r.ReadState.Version > user.ReadStateVersion {
		// TODO can we figure out which read states are actually new?
		for _, entry := range r.ReadState.Entries {
			user.messageAckHandler(&discordgo.MessageAck{
				MessageID: string(entry.LastMessageID),
				ChannelID: entry.ID,
			})
		}
		user.ReadStateVersion = r.ReadState.Version
		user.Update()
	}

	go user.subscribeGuilds(2 * time.Second)

	// Clear the stale presence cache and start the keepalive BEFORE launching
	// seedPresences. startPresenceKeepalive calls clear(presenceCache), so if
	// it ran after the goroutine below, any entries seeded before the clear
	// would be lost from the keepalive's view and never re-sent.
	user.startPresenceKeepalive()

	// Seed Matrix presence from the presence snapshots delivered in READY so
	// that users who are already online are reflected immediately rather than
	// waiting for a future PresenceUpdate event.
	go user.seedPresences(r.Presences)

	user.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
}

func (user *User) subscribeGuilds(delay time.Duration) {
	// Snapshot the session under the user lock. Logout() holds user.Lock()
	// while setting Session to nil, so reading it without the lock is a data
	// race and can produce a nil dereference inside discordgo's SubscribeGuild.
	// Subscribing to guilds is what makes Discord deliver presence (and large-guild
	// typing) via opcode 14. Upstream never does this; on user tokens it is a
	// self-bot fingerprint that risks a 4004 token invalidation, so it is gated
	// behind the Discord→Matrix presence option and off by default.
	if !user.bridge.Config.Bridge.SyncDiscordPresenceToMatrix {
		return
	}
	user.Lock()
	sess := user.Session
	user.Unlock()
	if sess == nil || !sess.IsUser {
		return
	}
	// Prevent overlapping runs (connect + repeated resumes) from stacking
	// opcode-14 commands. A run already in progress is subscribing to every
	// guild, so a concurrent invocation has nothing to add.
	if !user.subscribingGuilds.CompareAndSwap(false, true) {
		return
	}
	defer user.subscribingGuilds.Store(false)
	// On-demand mode: don't subscribe every guild on connect. Re-subscribe only
	// the guilds that were active before this (re)connect, so presence resumes
	// for rooms the user was actually using without re-announcing interest in the
	// entire guild list. New activity drives further subscriptions lazily.
	if !user.bridge.Config.Bridge.DiscordPresenceSubscribeAll {
		user.guildSubLock.Lock()
		active := make([]string, 0, len(user.activeGuildSubs))
		for guildID := range user.activeGuildSubs {
			active = append(active, guildID)
		}
		user.guildSubLock.Unlock()
		for _, guildID := range active {
			user.Lock()
			current := user.Session
			user.Unlock()
			if current != sess {
				return
			}
			if !user.sendGuildPresenceSubscribe(sess, guildID) {
				return
			}
			time.Sleep(delay)
		}
		return
	}
	for _, guildMeta := range sess.State.Guilds {
		guild := user.bridge.GetGuildByID(guildMeta.ID, false)
		if guild != nil && guild.MXID != "" {
			// Re-check the session each iteration: Logout() can replace
			// user.Session with nil while we sleep between guilds.
			user.Lock()
			current := user.Session
			user.Unlock()
			if current != sess {
				return
			}
			user.log.Debug().Str("guild_id", guild.ID).Msg("Subscribing to guild")
			if !user.sendGuildPresenceSubscribe(sess, guild.ID) {
				return
			}
			time.Sleep(delay)
		}
	}
}

// guildPresenceActiveTTL is how long a guild stays subscribed for presence in
// on-demand mode after the last Matrix-side activity in one of its rooms.
const guildPresenceActiveTTL = 10 * time.Minute

// defaultDiscordPresenceActiveLimit is used when DiscordPresenceActiveLimit is
// unset (<= 0): the most guilds kept subscribed at once in on-demand mode.
const defaultDiscordPresenceActiveLimit = 10

// guildSubscribeChannels builds the member-list range map Discord needs to
// deliver PRESENCE_UPDATE for a guild's bridged channels. Discord suppresses
// presence for large guilds (~250+ members) unless you subscribe to specific
// channels with ranges; {0,99} covers the first 100 members of each channel.
func (user *User) guildSubscribeChannels(guildID string) map[string][][]int {
	var channels map[string][][]int
	for _, p := range user.bridge.DB.Portal.GetAllInGuild(guildID) {
		if p.MXID != "" {
			if channels == nil {
				channels = make(map[string][][]int)
			}
			channels[p.Key.ChannelID] = [][]int{{0, 99}}
		}
	}
	return channels
}

// sendGuildPresenceSubscribe sends an opcode-14 subscription that makes Discord
// deliver presence (and typing) for the guild's bridged channels. It returns
// false if the websocket is gone so callers can stop iterating.
func (user *User) sendGuildPresenceSubscribe(sess *discordgo.Session, guildID string) bool {
	err := sess.SubscribeGuild(discordgo.GuildSubscribeData{
		GuildID:    guildID,
		Typing:     true,
		Activities: true,
		Threads:    true,
		Channels:   user.guildSubscribeChannels(guildID),
	})
	if err != nil {
		user.log.Warn().Err(err).Str("guild_id", guildID).Msg("Failed to subscribe to guild")
		return !errors.Is(err, discordgo.ErrWSNotFound)
	}
	return true
}

// sendGuildPresenceRelease tells Discord to stop sending presence/typing for a
// guild by re-subscribing with empty member-list ranges and activities off,
// which is how the official client drops a member list it is no longer viewing.
func (user *User) sendGuildPresenceRelease(sess *discordgo.Session, guildID string) {
	channels := make(map[string][][]int)
	for channelID := range user.guildSubscribeChannels(guildID) {
		channels[channelID] = [][]int{}
	}
	err := sess.SubscribeGuild(discordgo.GuildSubscribeData{
		GuildID:    guildID,
		Typing:     false,
		Activities: false,
		Threads:    false,
		Channels:   channels,
	})
	if err != nil {
		user.log.Debug().Err(err).Str("guild_id", guildID).Msg("Failed to release guild presence subscription")
	}
}

// touchGuildPresence marks a guild as actively viewed from Matrix and ensures it
// is subscribed for presence in on-demand mode. It refreshes the activity
// timestamp, subscribes guilds newly added, evicts the least-recently-active
// guild when over the limit, and releases entries idle past guildPresenceActiveTTL.
// It is a no-op when presence sync is off or in subscribe-all mode.
func (user *User) touchGuildPresence(guildID string) {
	if guildID == "" ||
		!user.bridge.Config.Bridge.SyncDiscordPresenceToMatrix ||
		user.bridge.Config.Bridge.DiscordPresenceSubscribeAll {
		return
	}
	user.Lock()
	sess := user.Session
	user.Unlock()
	if sess == nil || !sess.IsUser {
		return
	}

	limit := user.bridge.Config.Bridge.DiscordPresenceActiveLimit
	if limit <= 0 {
		limit = defaultDiscordPresenceActiveLimit
	}

	now := time.Now()
	var toSubscribe string
	var toRelease []string

	user.guildSubLock.Lock()
	// Release entries that have gone idle since the last activity anywhere.
	for gid, last := range user.activeGuildSubs {
		if gid != guildID && now.Sub(last) > guildPresenceActiveTTL {
			delete(user.activeGuildSubs, gid)
			toRelease = append(toRelease, gid)
		}
	}
	if _, ok := user.activeGuildSubs[guildID]; !ok {
		// Newly active guild: subscribe and, if that pushes us over the limit,
		// evict the least-recently-active other guild.
		toSubscribe = guildID
		user.activeGuildSubs[guildID] = now
		for len(user.activeGuildSubs) > limit {
			var oldestID string
			var oldest time.Time
			for gid, last := range user.activeGuildSubs {
				if gid == guildID {
					continue
				}
				if oldestID == "" || last.Before(oldest) {
					oldestID, oldest = gid, last
				}
			}
			if oldestID == "" {
				break
			}
			delete(user.activeGuildSubs, oldestID)
			toRelease = append(toRelease, oldestID)
		}
	} else {
		// Already subscribed: just refresh the activity timestamp.
		user.activeGuildSubs[guildID] = now
	}
	user.guildSubLock.Unlock()

	for _, gid := range toRelease {
		user.sendGuildPresenceRelease(sess, gid)
	}
	if toSubscribe != "" {
		user.log.Debug().Str("guild_id", toSubscribe).Msg("Subscribing to guild on demand")
		user.sendGuildPresenceSubscribe(sess, toSubscribe)
	}
}

func (user *User) resumeHandler(_ *discordgo.Resumed) {
	user.log.Debug().Msg("Discord connection resumed")
	// A RESUMED is a successful reconnect just like READY (the bridge copies
	// HeartbeatSession onto each session, so discordgo may resume). Run the same
	// success cleanup so the watchdog is disarmed and the backoff is reset —
	// otherwise the watchdog would close this healthy resumed session at 60s.
	user.onConnectionEstablished()
	// Clear the dedup cache so the first presence ping after a resume re-sends
	// the current status to Discord (the resumed session may have lost it).
	user.presenceLock.Lock()
	user.lastSentDiscordStatus = ""
	user.lastSentDiscordStatusText = ""
	user.presenceLock.Unlock()
	// Re-subscribe with the same pacing as the initial connect (subscribeGuilds
	// emits one op-14 GuildSubscribe per bridged guild). A 0-delay burst here
	// could blow Discord's 120-commands-per-60s gateway budget for a user in
	// many guilds — especially on a flapping connection that resumes repeatedly
	// — risking a 4008 rate-limit close and reconnect churn. Run it in a
	// goroutine so the resume handler isn't blocked for the duration.
	go user.subscribeGuilds(2 * time.Second)
	user.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
}

func (user *User) addPrivateChannelToSpace(portal *Portal) bool {
	if portal.MXID == "" {
		return false
	}
	_, err := user.bridge.Bot.SendStateEvent(user.GetDMSpaceRoom(), event.StateSpaceChild, portal.MXID.String(), &event.SpaceChildEventContent{
		Via: []string{user.bridge.AS.HomeserverDomain},
	})
	if err != nil {
		user.log.Error().Err(err).
			Str("room_id", portal.MXID.String()).
			Msg("Failed to add DMM room to user DM space")
		return false
	} else {
		return true
	}
}

func (user *User) relationshipAddHandler(r *discordgo.RelationshipAdd) {
	user.log.Debug().Interface("relationship", r.Relationship).Msg("Relationship added")
	user.relationshipLock.Lock()
	defer user.relationshipLock.Unlock()
	user.relationships[r.ID] = r.Relationship
	user.handleRelationshipChange(r.ID, r.Nickname)
}

func (user *User) relationshipUpdateHandler(r *discordgo.RelationshipUpdate) {
	user.relationshipLock.Lock()
	defer user.relationshipLock.Unlock()
	user.log.Debug().Interface("relationship", r.Relationship).Msg("Relationship update")
	user.relationships[r.ID] = r.Relationship
	user.handleRelationshipChange(r.ID, r.Nickname)
}

func (user *User) relationshipRemoveHandler(r *discordgo.RelationshipRemove) {
	user.relationshipLock.Lock()
	defer user.relationshipLock.Unlock()
	user.log.Debug().Str("other_user_id", r.ID).Msg("Relationship removed")
	delete(user.relationships, r.ID)
	user.handleRelationshipChange(r.ID, "")
}

func (user *User) handleRelationshipChange(userID, nickname string) {
	puppet := user.bridge.GetPuppetByID(userID)
	portal := user.FindPrivateChatWith(userID)
	if portal == nil || puppet == nil {
		return
	}

	updated := portal.FriendNick == (nickname != "")
	portal.FriendNick = nickname != ""
	if nickname != "" {
		updated = portal.UpdateNameDirect(nickname, true)
	} else if portal.Name != puppet.Name {
		if portal.shouldSetDMRoomMetadata() {
			updated = portal.UpdateNameDirect(puppet.Name, false)
		} else if portal.NameSet {
			_, err := portal.MainIntent().SendStateEvent(portal.MXID, event.StateRoomName, "", map[string]any{})
			if err != nil {
				portal.log.Warn().Err(err).Msg("Failed to clear room name after friend nickname was removed")
			} else {
				portal.log.Debug().Msg("Cleared room name after friend nickname was removed")
				portal.NameSet = false
				portal.Update()
				updated = true
			}
		}
	}
	if !updated {
		portal.Update()
	}
}

func (user *User) handlePrivateChannel(portal *Portal, meta *discordgo.Channel, timestamp time.Time, create, isInSpace bool) {
	if create && portal.MXID == "" {
		err := portal.CreateMatrixRoom(user, meta)
		if err != nil {
			user.log.Error().Err(err).
				Str("channel_id", portal.Key.ChannelID).
				Msg("Failed to create portal for private channel in create handler")
		}
	} else {
		portal.UpdateInfo(user, meta)
		portal.ForwardBackfillMissed(user, meta.LastMessageID, nil)
	}
	user.MarkInPortal(database.UserPortal{
		DiscordID: portal.Key.ChannelID,
		Type:      database.UserPortalTypeDM,
		Timestamp: timestamp,
		InSpace:   isInSpace || user.addPrivateChannelToSpace(portal),
	})
}

func (user *User) addGuildToSpace(guild *Guild, isInSpace bool, timestamp time.Time) bool {
	if len(guild.MXID) > 0 && !isInSpace {
		_, err := user.bridge.Bot.SendStateEvent(user.GetSpaceRoom(), event.StateSpaceChild, guild.MXID.String(), &event.SpaceChildEventContent{
			Via: []string{user.bridge.AS.HomeserverDomain},
		})
		if err != nil {
			user.log.Error().Err(err).
				Str("guild_space_id", guild.MXID.String()).
				Msg("Failed to add guild space to user space")
		} else {
			isInSpace = true
		}
	}
	user.MarkInPortal(database.UserPortal{
		DiscordID: guild.ID,
		Type:      database.UserPortalTypeGuild,
		Timestamp: timestamp,
		InSpace:   isInSpace,
	})
	return isInSpace
}

func (user *User) discordRoleToDB(guildID string, role *discordgo.Role, dbRole *database.Role, txn dbutil.Execable) bool {
	var changed bool
	if dbRole == nil {
		dbRole = user.bridge.DB.Role.New()
		dbRole.ID = role.ID
		dbRole.GuildID = guildID
		changed = true
	} else {
		changed = dbRole.Name != role.Name ||
			dbRole.Icon != role.Icon ||
			dbRole.Mentionable != role.Mentionable ||
			dbRole.Managed != role.Managed ||
			dbRole.Hoist != role.Hoist ||
			dbRole.Color != role.Color ||
			dbRole.Position != role.Position ||
			dbRole.Permissions != role.Permissions
	}
	dbRole.Role = *role
	if changed {
		dbRole.Upsert(txn)
	}
	return changed
}

func (user *User) handleGuildRoles(guildID string, newRoles []*discordgo.Role) {
	existingRoles := user.bridge.DB.Role.GetAll(guildID)
	existingRoleMap := make(map[string]*database.Role, len(existingRoles))
	for _, role := range existingRoles {
		existingRoleMap[role.ID] = role
	}
	txn, err := user.bridge.DB.Begin()
	if err != nil {
		user.log.Error().Err(err).Msg("Failed to start transaction for guild role sync")
		panic(err)
	}
	for _, role := range newRoles {
		user.discordRoleToDB(guildID, role, existingRoleMap[role.ID], txn)
		delete(existingRoleMap, role.ID)
	}
	for _, removeRole := range existingRoleMap {
		removeRole.Delete(txn)
	}
	err = txn.Commit()
	if err != nil {
		user.log.Error().Err(err).Msg("Failed to commit guild role sync transaction")
		rollbackErr := txn.Rollback()
		if rollbackErr != nil {
			user.log.Error().Err(rollbackErr).Msg("Failed to rollback errored guild role sync transaction")
		}
		panic(err)
	}
}

func (user *User) handleGuild(meta *discordgo.Guild, timestamp time.Time, isInSpace bool) {
	guild := user.bridge.GetGuildByID(meta.ID, true)
	guild.UpdateInfo(user, meta)
	if len(meta.Channels) > 0 {
		for _, ch := range meta.Channels {
			if !user.channelIsBridgeable(ch) {
				continue
			}
			portal := user.GetPortalByMeta(ch)
			if guild.BridgingMode >= database.GuildBridgeEverything && portal.MXID == "" {
				err := portal.CreateMatrixRoom(user, ch)
				if err != nil {
					user.log.Error().Err(err).
						Str("guild_id", guild.ID).
						Str("channel_id", ch.ID).
						Msg("Failed to create portal for guild channel in guild handler")
				}
			} else {
				portal.UpdateInfo(user, ch)
				if user.bridge.Config.Bridge.Backfill.MaxGuildMembers < 0 || meta.MemberCount < user.bridge.Config.Bridge.Backfill.MaxGuildMembers {
					portal.ForwardBackfillMissed(user, ch.LastMessageID, nil)
				}
			}
		}
	}
	if len(meta.Roles) > 0 {
		user.handleGuildRoles(meta.ID, meta.Roles)
	}
	user.addGuildToSpace(guild, isInSpace, timestamp)
}

func (user *User) connectedHandler(_ *discordgo.Connect) {
	user.bridgeStateLock.Lock()
	defer user.bridgeStateLock.Unlock()
	user.log.Debug().Msg("Connected to Discord")
	if user.wasDisconnected {
		user.wasDisconnected = false
	}
}

func (user *User) disconnectedHandler(_ *discordgo.Disconnect) {
	user.bridgeStateLock.Lock()
	defer user.bridgeStateLock.Unlock()
	if user.wasLoggedOut {
		user.log.Debug().Msg("Disconnected from Discord (not updating bridge state as user was just logged out)")
		return
	}
	// A teardown the bridge initiated (manual disconnect / shutdown) must not
	// trigger a reconnect.
	user.reconnectLock.Lock()
	manual := user.manualDisconnect
	user.reconnectLock.Unlock()
	if manual {
		user.log.Debug().Msg("Disconnected from Discord (manual disconnect, not reconnecting)")
		return
	}
	user.log.Debug().Msg("Disconnected from Discord")
	user.wasDisconnected = true
	user.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Error: "dc-transient-disconnect", Message: "Temporarily disconnected from Discord, trying to reconnect"})
	// The dropped session's READY watchdog is moot now; the next connect arms a
	// fresh one. Disarm it so it can't fire during the backoff window.
	user.disarmReadyWatchdog()
	// discordgo's auto-reconnect is disabled; drive it ourselves with backoff.
	user.scheduleReconnect()
}

// reconnectBackoffBase and reconnectBackoffMax bound the exponential backoff
// between gateway reconnect attempts. The first retry after a drop is quick so
// genuine transient blips recover fast; sustained failure (e.g. Discord
// rejecting the session with Op9, or an account in a rate-limit penalty) slows
// to one attempt per reconnectBackoffMax, which keeps IDENTIFY traffic far below
// any level that would endanger the user token.
const (
	reconnectBackoffBase = 2 * time.Second
	reconnectBackoffMax  = 10 * time.Minute
	// readyTimeout is how long a freshly-opened session may take to reach READY
	// before the watchdog force-closes it. A genuine READY/RESUMED arrives within
	// a few seconds even for large accounts (guild payloads stream afterwards),
	// so this is kept short to bound the worst case where Discord holds the socket
	// open and answers only with in-band Op9 re-IDENTIFYs: it caps that burst at
	// ~readyTimeout per backoff cycle instead of a full minute. In practice
	// Discord closes a rejected session within a second or two (4002/4003), which
	// already routes through the backoff path; this is the backstop for when it
	// doesn't.
	readyTimeout = 20 * time.Second
)

// armReadyWatchdog (re)starts the READY watchdog for the current session. Called
// after a successful Open(); cancelled by readyHandler on success or by
// stopReconnecting on teardown.
func (user *User) armReadyWatchdog() {
	user.reconnectLock.Lock()
	defer user.reconnectLock.Unlock()
	if user.readyWatchdog != nil {
		user.readyWatchdog.Stop()
		user.readyWatchdog = nil
	}
	// Skip arming if the bridge is tearing down, or if READY was already
	// processed during Open() (readyHandler set sessionReady) — in that case
	// there is nothing to watch for.
	if user.manualDisconnect || user.sessionReady {
		return
	}
	// Capture the session this watchdog guards so a stale timer for an old
	// session can't close a newer one that has since replaced it. armReadyWatchdog
	// is called from connect() under user.Lock, so user.Session is stable here.
	guarded := user.Session
	user.readyWatchdog = time.AfterFunc(readyTimeout, func() { user.onReadyTimeout(guarded) })
}

// onConnectionEstablished runs the success cleanup shared by READY and RESUMED:
// it marks the session ready, disarms the watchdog, and resets the backoff
// counter. It deliberately does NOT stop reconnectTimer — because gateway events
// dispatch on separate goroutines, a stale READY can run after a Disconnect from
// the same connection has already scheduled a retry, and cancelling that timer
// would leave the bridge disconnected with no pending reconnect. An intentional
// Connect() supersedes a pending timer instead (see connect()).
func (user *User) onConnectionEstablished() {
	user.reconnectLock.Lock()
	user.sessionReady = true
	user.reconnectAttempts = 0
	if user.readyWatchdog != nil {
		user.readyWatchdog.Stop()
		user.readyWatchdog = nil
	}
	user.reconnectLock.Unlock()
}

// disarmReadyWatchdog cancels the READY watchdog if armed.
func (user *User) disarmReadyWatchdog() {
	user.reconnectLock.Lock()
	if user.readyWatchdog != nil {
		user.readyWatchdog.Stop()
		user.readyWatchdog = nil
	}
	user.reconnectLock.Unlock()
}

// onReadyTimeout fires when a session opened but never reached READY. It closes
// the session so discordgo emits a Disconnect, which routes the failure through
// disconnectedHandler -> scheduleReconnect (capped backoff).
func (user *User) onReadyTimeout(guarded *discordgo.Session) {
	user.reconnectLock.Lock()
	user.readyWatchdog = nil
	// Bail if the handshake completed (READY/RESUMED) concurrently with this
	// timer firing, or if the bridge is tearing down — never close a valid
	// session. Both this check and onConnectionEstablished's set are under
	// reconnectLock, so whichever ran first wins deterministically.
	bail := user.sessionReady || user.manualDisconnect
	user.reconnectLock.Unlock()
	if bail {
		return
	}
	// Only close the session this watchdog was armed for. If a newer session has
	// since replaced it (reconnect after this one dropped), the stale timer must
	// not touch the new, possibly-healthy session.
	user.Lock()
	current := user.Session
	user.Unlock()
	if current != guarded {
		return
	}
	user.log.Warn().Msg("No READY received after connect; closing session to trigger backoff reconnect")
	_ = guarded.Close()
}

// scheduleReconnect arms a single backoff timer to reconnect the gateway. The
// delay grows as 2^(failures-since-last-READY), capped at reconnectBackoffMax.
// Safe to call from the disconnect handler and from a failed reconnect attempt;
// it coalesces (one pending timer at a time) and is a no-op after a manual
// disconnect.
func (user *User) scheduleReconnect() {
	user.reconnectLock.Lock()
	defer user.reconnectLock.Unlock()
	if user.manualDisconnect || user.reconnectTimer != nil {
		return
	}
	user.reconnectAttempts++
	shift := user.reconnectAttempts - 1
	if shift > 9 {
		shift = 9
	}
	delay := reconnectBackoffBase << shift
	if delay > reconnectBackoffMax {
		delay = reconnectBackoffMax
	}
	user.log.Warn().
		Int("attempt", user.reconnectAttempts).
		Dur("delay", delay).
		Msg("Scheduling Discord gateway reconnect with backoff")
	user.reconnectTimer = time.AfterFunc(delay, user.doReconnect)
}

// doReconnect is the backoff timer callback. It performs one reconnect attempt
// and, on failure, reschedules with a longer backoff. On success it returns and
// lets the normal lifecycle take over: readyHandler resets the attempt counter,
// or a fresh Disconnect reschedules.
func (user *User) doReconnect() {
	user.reconnectLock.Lock()
	user.reconnectTimer = nil
	manual := user.manualDisconnect
	user.reconnectLock.Unlock()
	if manual {
		return
	}
	user.bridgeStateLock.Lock()
	loggedOut := user.wasLoggedOut
	user.bridgeStateLock.Unlock()
	if loggedOut {
		return
	}

	user.log.Info().Msg("Attempting Discord gateway reconnect")
	user.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
	// connect(false): an auto-reconnect must not clear the manual-disconnect
	// latch, so a manual teardown racing this attempt still wins.
	err := user.connect(false)
	if err == nil {
		// Connected. Either READY fires and clears the backoff, or Discord rejects
		// the session and a Disconnect reschedules with a longer delay.
		return
	}
	if errors.Is(err, ErrNotLoggedIn) {
		// Nothing to reconnect to; stop.
		return
	}
	closeErr := &websocket.CloseError{}
	if errors.As(err, &closeErr) && closeErr.Code == 4004 {
		user.invalidAuthHandler(nil)
		return
	}
	user.log.Warn().Err(err).Msg("Discord gateway reconnect attempt failed")
	user.scheduleReconnect()
}

// stopReconnecting latches manualDisconnect and cancels any pending backoff
// timer. Call it before intentionally closing the session (logout, manual
// disconnect, shutdown) so the resulting Disconnect event is not reconnected.
func (user *User) stopReconnecting() {
	user.reconnectLock.Lock()
	user.manualDisconnect = true
	user.reconnectAttempts = 0
	if user.reconnectTimer != nil {
		user.reconnectTimer.Stop()
		user.reconnectTimer = nil
	}
	if user.readyWatchdog != nil {
		user.readyWatchdog.Stop()
		user.readyWatchdog = nil
	}
	user.reconnectLock.Unlock()
}

func (user *User) invalidAuthHandler(_ *discordgo.InvalidAuth) {
	user.bridgeStateLock.Lock()
	defer user.bridgeStateLock.Unlock()
	user.log.Info().Msg("Got logged out from Discord due to invalid token")
	user.wasLoggedOut = true
	user.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: "dc-websocket-disconnect-4004", Message: "Discord access token is no longer valid, please log in again"})
	go user.Logout(false)
}

func (user *User) handlePossible40002(err error) bool {
	var restErr *discordgo.RESTError
	if !errors.As(err, &restErr) || restErr.Message == nil || restErr.Message.Code != discordgo.ErrCodeActionRequiredVerifiedAccount {
		return false
	}
	user.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: "dc-http-40002", Message: restErr.Message.Message})
	return true
}

func (user *User) guildCreateHandler(g *discordgo.GuildCreate) {
	user.log.Info().
		Str("guild_id", g.ID).
		Str("name", g.Name).
		Bool("unavailable", g.Unavailable).
		Msg("Got guild create event")
	user.handleGuild(g.Guild, time.Now(), false)
	// Seed Matrix presence from GUILD_CREATE presences so reconnect/restart
	// doesn't leave known users as offline until their next status change.
	go user.seedPresences(g.Presences)
}

func (user *User) guildDeleteHandler(g *discordgo.GuildDelete) {
	if g.Unavailable {
		user.log.Info().Str("guild_id", g.ID).Msg("Ignoring guild delete event with unavailable flag")
		return
	}
	user.log.Info().Str("guild_id", g.ID).Msg("Got guild delete event")
	user.MarkNotInPortal(g.ID)
	guild := user.bridge.GetGuildByID(g.ID, false)
	if guild == nil || guild.MXID == "" {
		return
	}
	if user.bridge.Config.Bridge.DeleteGuildOnLeave && !user.PortalHasOtherUsers(g.ID) {
		user.log.Debug().Str("guild_id", g.ID).Msg("No other users in guild, cleaning up all portals")
		err := user.unbridgeGuild(g.ID)
		if err != nil {
			user.log.Warn().Err(err).Msg("Failed to unbridge guild that was deleted")
		}
	}
}

func (user *User) guildUpdateHandler(g *discordgo.GuildUpdate) {
	user.log.Debug().Str("guild_id", g.ID).Msg("Got guild update event")
	user.handleGuild(g.Guild, time.Now(), user.IsInSpace(g.ID))
}

func (user *User) threadListSyncHandler(t *discordgo.ThreadListSync) {
	for _, meta := range t.Threads {
		log := user.log.With().
			Str("action", "thread list sync").
			Str("guild_id", t.GuildID).
			Str("parent_id", meta.ParentID).
			Str("thread_id", meta.ID).
			Logger()
		ctx := log.WithContext(context.Background())
		thread := user.bridge.GetThreadByID(meta.ID, nil)
		if thread == nil {
			msg := user.bridge.DB.Message.GetByDiscordID(database.NewPortalKey(meta.ParentID, ""), meta.ID)
			if len(msg) == 0 {
				log.Debug().Msg("Found unknown thread in thread list sync and don't have message")
			} else {
				log.Debug().Msg("Found unknown thread in thread list sync for existing message, creating thread")
				user.bridge.threadFound(ctx, user, msg[0], meta.ID, meta)
			}
		} else {
			thread.Parent.ForwardBackfillMissed(user, meta.LastMessageID, thread)
		}
	}
}

func (user *User) channelCreateHandler(c *discordgo.ChannelCreate) {
	if user.getGuildBridgingMode(c.GuildID) < database.GuildBridgeEverything {
		user.log.Debug().
			Str("guild_id", c.GuildID).Str("channel_id", c.ID).
			Msg("Ignoring channel create event in unbridged guild")
		return
	}
	user.log.Info().
		Str("guild_id", c.GuildID).Str("channel_id", c.ID).
		Msg("Got channel create event")
	portal := user.GetPortalByMeta(c.Channel)
	if portal.MXID != "" {
		return
	}
	if c.GuildID == "" {
		user.handlePrivateChannel(portal, c.Channel, time.Now(), true, user.IsInSpace(portal.Key.String()))
	} else if user.channelIsBridgeable(c.Channel) {
		err := portal.CreateMatrixRoom(user, c.Channel)
		if err != nil {
			user.log.Error().Err(err).
				Str("guild_id", c.GuildID).Str("channel_id", c.ID).
				Msg("Error creating Matrix room after channel create event")
		}
	} else {
		user.log.Debug().
			Str("guild_id", c.GuildID).Str("channel_id", c.ID).
			Msg("Got channel create event, but it's not bridgeable, ignoring")
	}
}

func (user *User) channelDeleteHandler(c *discordgo.ChannelDelete) {
	portal := user.GetExistingPortalByID(c.ID)
	if portal == nil {
		user.log.Debug().
			Str("guild_id", c.GuildID).Str("channel_id", c.ID).
			Msg("Ignoring channel delete event of unknown channel")
		return
	}
	user.log.Info().
		Str("guild_id", c.GuildID).Str("channel_id", c.ID).
		Msg("Got channel delete event, cleaning up portal")
	portal.Delete()
	portal.cleanup(!user.bridge.Config.Bridge.DeletePortalOnChannelDelete)
	if c.GuildID == "" {
		user.MarkNotInPortal(portal.Key.ChannelID)
	}
	user.log.Debug().
		Str("guild_id", c.GuildID).Str("channel_id", c.ID).
		Msg("Completed cleaning up channel")
}

func (user *User) channelUpdateHandler(c *discordgo.ChannelUpdate) {
	portal := user.GetPortalByMeta(c.Channel)
	if c.GuildID == "" {
		user.handlePrivateChannel(portal, c.Channel, time.Now(), true, user.IsInSpace(portal.Key.String()))
	} else if user.channelIsBridgeable(c.Channel) {
		portal.UpdateInfo(user, c.Channel)
	}
}

func (user *User) channelRecipientAdd(c *discordgo.ChannelRecipientAdd) {
	portal := user.GetExistingPortalByID(c.ChannelID)
	if portal != nil {
		portal.syncParticipant(user, c.User, false)
	}
}

func (user *User) channelRecipientRemove(c *discordgo.ChannelRecipientRemove) {
	portal := user.GetExistingPortalByID(c.ChannelID)
	if portal != nil {
		portal.syncParticipant(user, c.User, true)
	}
}

func (user *User) findPortal(channelID string) (*Portal, *Thread) {
	portal := user.GetExistingPortalByID(channelID)
	if portal != nil {
		return portal, nil
	}
	thread := user.bridge.GetThreadByID(channelID, nil)
	if thread != nil && thread.Parent != nil {
		return thread.Parent, thread
	}
	if !user.Session.IsUser {
		channel, _ := user.Session.State.Channel(channelID)
		if channel == nil {
			user.log.Debug().Str("channel_id", channelID).Msg("Fetching info of unknown channel to handle message")
			var err error
			channel, err = user.Session.Channel(channelID)
			if err != nil {
				user.log.Warn().Err(err).Str("channel_id", channelID).Msg("Failed to get info of unknown channel")
			} else {
				user.log.Debug().Str("channel_id", channelID).Msg("Got info for channel to handle message")
				_ = user.Session.State.ChannelAdd(channel)
			}
		}
		if channel != nil && user.channelIsBridgeable(channel) {
			user.log.Debug().Str("channel_id", channelID).Msg("Creating portal and updating info to handle message")
			portal = user.GetPortalByMeta(channel)
			if channel.GuildID == "" {
				user.handlePrivateChannel(portal, channel, time.Now(), false, false)
			} else {
				user.log.Warn().
					Str("channel_id", channel.ID).Str("guild_id", channel.GuildID).
					Msg("Unexpected unknown guild channel")
			}
			return portal, nil
		}
	}
	return nil, nil
}

func (user *User) pushPortalMessage(msg interface{}, typeName, channelID, guildID string) {
	if user.getGuildBridgingMode(guildID) <= database.GuildBridgeNothing {
		// If guild bridging mode is nothing, don't even check if the portal exists
		return
	}

	portal, thread := user.findPortal(channelID)
	if portal == nil {
		user.log.Debug().
			Str("discord_event", typeName).
			Str("guild_id", guildID).
			Str("channel_id", channelID).
			Msg("Dropping event in unknown channel")
		return
	}
	if mode := user.getGuildBridgingMode(portal.GuildID); mode <= database.GuildBridgeNothing || (portal.MXID == "" && mode <= database.GuildBridgeIfPortalExists) {
		return
	}

	wrappedMsg := portalDiscordMessage{
		msg:    msg,
		user:   user,
		thread: thread,
	}
	select {
	case portal.discordMessages <- wrappedMsg:
	default:
		user.log.Warn().
			Str("discord_event", typeName).
			Str("guild_id", guildID).
			Str("channel_id", channelID).
			Msg("Portal message buffer is full")
		portal.discordMessages <- wrappedMsg
	}
}

type CustomReadReceipt struct {
	Timestamp          int64  `json:"ts,omitempty"`
	DoublePuppetSource string `json:"fi.mau.double_puppet_source,omitempty"`
}

type CustomReadMarkers struct {
	mautrix.ReqSetReadMarkers
	ReadExtra      CustomReadReceipt `json:"com.beeper.read.extra"`
	FullyReadExtra CustomReadReceipt `json:"com.beeper.fully_read.extra"`
}

func (user *User) makeReadMarkerContent(eventID id.EventID) *CustomReadMarkers {
	var extra CustomReadReceipt
	extra.DoublePuppetSource = user.bridge.Name
	return &CustomReadMarkers{
		ReqSetReadMarkers: mautrix.ReqSetReadMarkers{
			Read:      eventID,
			FullyRead: eventID,
		},
		ReadExtra:      extra,
		FullyReadExtra: extra,
	}
}

func (user *User) messageAckHandler(m *discordgo.MessageAck) {
	portal := user.GetExistingPortalByID(m.ChannelID)
	if portal == nil || portal.MXID == "" {
		return
	}
	dp := user.GetIDoublePuppet()
	if dp == nil {
		return
	}
	msg := user.bridge.DB.Message.GetLastByDiscordID(portal.Key, m.MessageID)
	if msg == nil {
		user.log.Debug().
			Str("channel_id", m.ChannelID).Str("message_id", m.MessageID).
			Msg("Dropping message ack event for unknown message")
		return
	}
	err := dp.CustomIntent().SetReadMarkers(portal.MXID, user.makeReadMarkerContent(msg.MXID))
	if err != nil {
		user.log.Error().Err(err).
			Str("event_id", msg.MXID.String()).Str("message_id", msg.DiscordID).
			Msg("Failed to mark event as read")
	} else {
		user.log.Debug().
			Str("event_id", msg.MXID.String()).Str("message_id", msg.DiscordID).
			Msg("Marked event as read after Discord message ack")
		if user.ReadStateVersion < m.Version {
			user.ReadStateVersion = m.Version
			// TODO maybe don't update every time?
			user.Update()
		}
	}
}

func (user *User) typingStartHandler(t *discordgo.TypingStart) {
	if t.UserID == user.DiscordID {
		return
	}
	portal := user.GetExistingPortalByID(t.ChannelID)
	if portal == nil || portal.MXID == "" {
		return
	}
	targetUser := user.bridge.GetCachedUserByID(t.UserID)
	if targetUser != nil {
		return
	}
	portal.handleDiscordTyping(t)
}

func (user *User) interactionSuccessHandler(s *discordgo.InteractionSuccess) {
	user.pendingInteractionsLock.Lock()
	defer user.pendingInteractionsLock.Unlock()
	ce, ok := user.pendingInteractions[s.Nonce]
	if !ok {
		user.log.Debug().Str("nonce", s.Nonce).Str("id", s.ID).Msg("Got interaction success for unknown interaction")
	} else {
		user.log.Debug().Str("nonce", s.Nonce).Str("id", s.ID).Msg("Got interaction success for pending interaction")
		ce.React("✅")
		delete(user.pendingInteractions, s.Nonce)
	}
}

func discordStatusToMatrix(status discordgo.Status) event.Presence {
	switch status {
	case discordgo.StatusOnline:
		return event.PresenceOnline
	case discordgo.StatusIdle:
		return event.PresenceUnavailable
	case discordgo.StatusDoNotDisturb:
		// DND is a visible, connected state — map to unavailable rather than offline
		// so Matrix clients retain the fact that the user is currently present.
		return event.PresenceUnavailable
	case discordgo.StatusInvisible, discordgo.StatusOffline:
		return event.PresenceOffline
	default:
		return event.PresenceOffline
	}
}

// normalizeDiscordStatusForEcho collapses the invisible/offline pair to one
// canonical value for echo detection. matrixPresenceToDiscord maps a Matrix
// offline presence to "invisible" (so the account is hidden rather than shown
// offline), but Discord's gateway reports a user-account's own hidden state
// back as "offline". Without normalization, the echo of a Matrix-originated
// offline change fails strict equality against the stored "invisible" and is
// misread as a genuine Discord change — clobbering the Matrix-side state (e.g.
// a Charm [dnd] marker). All other statuses pass through unchanged; in
// particular idle and dnd are kept distinct even though both map to the same
// Matrix presence, so a genuine idle↔dnd change is never suppressed as an echo.
func normalizeDiscordStatusForEcho(status string) string {
	if status == string(discordgo.StatusInvisible) || status == string(discordgo.StatusOffline) {
		return string(discordgo.StatusOffline)
	}
	return status
}

// setMatrixPresence sets the Matrix presence and optional status message for a
// puppet intent. It makes a raw PUT to the presence endpoint so that status_msg
// can be included alongside the presence state (the high-level SetPresence
// helper in mautrix-go only accepts the presence state).
func setMatrixPresence(intent *appservice.IntentAPI, presence event.Presence, statusMsg string) error {
	reqBody := struct {
		Presence  event.Presence `json:"presence"`
		StatusMsg string         `json:"status_msg,omitempty"`
	}{
		Presence:  presence,
		StatusMsg: statusMsg,
	}
	u := intent.BuildClientURL("v3", "presence", intent.UserID, "status")
	_, err := intent.MakeRequest("PUT", u, reqBody, nil)
	return err
}

// applyPresence updates Matrix presence for a Discord user, but only when a
// puppet for that user already exists. Presence updates for users who have
// never sent a bridged message are skipped to avoid unbounded puppet-table
// churn in large guilds.
func (user *User) applyPresence(userID string, status discordgo.Status, customStatusText string) {
	puppet := user.bridge.GetPuppetByIDIfExists(userID)
	if puppet == nil {
		// No puppet exists yet; skip to avoid creating phantom DB rows.
		return
	}
	matrixPresence := discordStatusToMatrix(status)
	err := setMatrixPresence(puppet.DefaultIntent(), matrixPresence, customStatusText)
	if err != nil {
		user.log.Warn().Err(err).
			Str("discord_user_id", userID).
			Str("discord_status", string(status)).
			Msg("Failed to set Matrix presence for Discord user")
	} else {
		user.log.Debug().
			Str("discord_user_id", userID).
			Str("discord_status", string(status)).
			Str("matrix_presence", string(matrixPresence)).
			Str("status_text", customStatusText).
			Msg("Bridged Discord presence to Matrix")
	}
	// Keep the keepalive cache in sync so that the background goroutine can
	// re-send this presence before Synapse times out the ghost user.
	user.presenceLock.Lock()
	if matrixPresence == event.PresenceOffline {
		delete(user.presenceCache, userID)
	} else {
		user.presenceCache[userID] = presenceCacheEntry{presence: matrixPresence, statusMsg: customStatusText}
	}
	user.presenceLock.Unlock()
	// When double puppeting is active, also update the real Matrix account so
	// clients see the correct presence on the user's own identity, not just on
	// the bridge ghost which may not be present in all rooms.
	// Skip any puppet whose CustomMXID belongs to a logged-in bridge user: the
	// appservice receives the resulting m.presence echo via ephemeral_events and
	// HandleMatrixPresence would treat it as a Matrix-side change, immediately
	// clobbering the Discord status (e.g. DND → unavailable → echoed back → idle).
	// This applies to both the observing user's own account and any other
	// logged-in user whose Discord presence is being observed.
	if customIntent := puppet.CustomIntent(); customIntent != nil {
		// Use GetUserByMXIDIfExists (cache + DB fallback) rather than
		// GetCachedUserByMXID (cache only) so that a user who is stored in the
		// DB but not yet loaded into the in-memory map still triggers the echo-
		// suppression guard. Session is an in-memory field only — a DB-loaded
		// user without an active session will still have Session == nil, so the
		// Session check below remains the authoritative gate.
		customUser := user.bridge.GetUserByMXIDIfExists(puppet.CustomMXID)
		if customUser == nil || customUser.Session == nil {
			// No active bridge session for this double-puppeted account — safe to
			// update the real Matrix account without triggering an echo loop.
			if err := setMatrixPresence(customIntent, matrixPresence, customStatusText); err != nil {
				user.log.Warn().Err(err).
					Str("discord_user_id", userID).
					Msg("Failed to set Matrix presence for double-puppeted user")
			}
		} else if customUser == user {
			// Bridge user's own Discord status changed. Update their real Matrix
			// account so the change is visible in Matrix clients (Discord→Matrix
			// own-user sync).
			//
			// Two guards before calling setMatrixPresence:
			//
			// 1. Echo suppression: if HandleMatrixPresence recently forwarded a
			//    Matrix presence to Discord (matrixPresenceSetAt within 2s), this
			//    Discord event is likely the gateway echo of that action. Skip it
			//    to avoid overwriting the Matrix-side state — in particular the
			//    [dnd] status_msg prefix that Charm uses and that we strip before
			//    forwarding to Discord.
			//
			// 2. Dedup: only call setMatrixPresence when something actually
			//    changed; frequent Discord rich-presence ticks (e.g. game timers)
			//    would otherwise slide discordPresenceSetAt continuously.
			//
			// The timestamp and cache are pre-armed before the PUT under presenceLock
			// so a fast Synapse echo (arriving before the HTTP response) is still
			// suppressed. On PUT failure the pre-armed values are restored, so the
			// next identical Discord event retries the PUT rather than being deduped.
			user.presenceLock.Lock()
			// An event is treated as an echo only when the timestamp AND the
			// Discord status/text match what HandleMatrixPresence just forwarded.
			//
			// A special case: when textToSend was "" (intentional Matrix status
			// clear), HandleMatrixPresence sends activities=nil to Discord, which
			// preserves the old Discord custom status instead of clearing it.
			// Discord's resulting PRESENCE_UPDATE echo therefore contains the
			// old custom status text, not "". We detect this by treating any
			// echo as matching when lastSentToDiscordText == "" — the preserved
			// text is never a new user-set value, so suppressing it is correct.
			//
			// The status is normalized so that an "offline" echo of a Matrix
			// offline change matches the "invisible" we sent (see
			// normalizeDiscordStatusForEcho); without this the echo is misread
			// as a genuine change and overwrites the Matrix-side state.
			isEcho := time.Now().Before(user.matrixPresenceSetAt.Add(2*time.Second)) &&
				normalizeDiscordStatusForEcho(string(status)) == normalizeDiscordStatusForEcho(user.lastSentToDiscordStatus) &&
				(customStatusText == user.lastSentToDiscordText || user.lastSentToDiscordText == "")
			if isEcho {
				// Advance the dedup cache even though we skip the write. Without
				// this, a post-window rich-presence tick (game timer) with the same
				// Discord status/text sees changed=true and writes the stripped
				// customStatusText back, removing any Matrix-client-set [dnd] marker.
				user.lastOwnMatrixPresence = matrixPresence
				user.lastOwnMatrixStatusMsg = customStatusText
			}
			changed := !isEcho && (matrixPresence != user.lastOwnMatrixPresence || customStatusText != user.lastOwnMatrixStatusMsg)
			if changed {
				// Pre-arm suppression and expected-echo values before the PUT so
				// that a fast Synapse push_ephemeral delivery (which can arrive
				// before the HTTP response returns) is still correctly suppressed
				// by HandleMatrixPresence. Restore the previous values on failure
				// so that the next identical Discord event retries the PUT.
				prevPresence := user.lastOwnMatrixPresence
				prevStatus := user.lastOwnMatrixStatusMsg
				user.discordPresenceSetAt = time.Now()
				user.lastOwnMatrixPresence = matrixPresence
				user.lastOwnMatrixStatusMsg = customStatusText
				user.presenceLock.Unlock()
				if err := setMatrixPresence(customIntent, matrixPresence, customStatusText); err != nil {
					user.log.Warn().Err(err).
						Str("discord_user_id", userID).
						Msg("Failed to set Matrix presence for own double-puppeted account")
					user.presenceLock.Lock()
					user.discordPresenceSetAt = time.Time{}
					user.lastOwnMatrixPresence = prevPresence
					user.lastOwnMatrixStatusMsg = prevStatus
					user.presenceLock.Unlock()
				}
			} else {
				user.presenceLock.Unlock()
			}
		}
		// If customUser != nil && customUser.Session != nil && customUser != user:
		// another active bridge user's double-puppeted account — skip to avoid an
		// echo loop through HandleMatrixPresence on their Discord session.
	}
}

// forwardPresenceToDiscord forwards a desired (status, statusText) presence to
// the user's Discord session with content dedup and trailing-debounce rate
// limiting, so the bridge never emits opcode-3 updates faster than
// presenceMinInterval. A change outside the cooldown is sent immediately; a
// change inside it is coalesced into pendingDiscordStatus/Text and dispatched
// by a single trailing timer when the cooldown expires. Must be called without
// presenceLock held; sess must be non-nil.
func (user *User) forwardPresenceToDiscord(sess *discordgo.Session, status, statusText string) {
	user.presenceLock.Lock()
	// Dedup: if the desired state already matches what Discord was last told,
	// there is nothing to forward. Keep the pending target in sync so a timer
	// that fires later flushes to current reality rather than a stale value.
	if status == user.lastSentDiscordStatus && statusText == user.lastSentDiscordStatusText {
		user.pendingDiscordStatus = status
		user.pendingDiscordStatusText = statusText
		user.presenceLock.Unlock()
		return
	}
	user.pendingDiscordStatus = status
	user.pendingDiscordStatusText = statusText
	// A trailing send is already scheduled: it will pick up the updated target.
	if user.presenceDebounceTimer != nil {
		user.presenceLock.Unlock()
		return
	}
	if elapsed := time.Since(user.lastDiscordPresenceSentAt); elapsed < presenceMinInterval {
		// Inside the cooldown: schedule a trailing flush for when it expires.
		user.presenceDebounceTimer = time.AfterFunc(presenceMinInterval-elapsed, user.flushPendingDiscordPresence)
		user.presenceLock.Unlock()
		return
	}
	// Outside the cooldown: send now. Arm the timestamp under the lock so a
	// concurrent event coalesces instead of emitting a second opcode-3.
	user.lastDiscordPresenceSentAt = time.Now()
	user.presenceLock.Unlock()
	user.sendPresenceToDiscord(sess, status, statusText)
}

// flushPendingDiscordPresence is invoked by the debounce timer when the
// rate-limit cooldown expires. It forwards the most recent pending presence
// state to Discord, unless reality already caught up to it while waiting.
func (user *User) flushPendingDiscordPresence() {
	user.presenceLock.Lock()
	user.presenceDebounceTimer = nil
	status := user.pendingDiscordStatus
	statusText := user.pendingDiscordStatusText
	if status == user.lastSentDiscordStatus && statusText == user.lastSentDiscordStatusText {
		user.presenceLock.Unlock()
		return
	}
	user.lastDiscordPresenceSentAt = time.Now()
	user.presenceLock.Unlock()

	// Snapshot the session under user.Lock to avoid racing Logout(), which sets
	// Session to nil. If the user logged out during the cooldown, drop the send.
	user.Lock()
	sess := user.Session
	user.Unlock()
	if sess == nil {
		return
	}
	user.sendPresenceToDiscord(sess, status, statusText)
}

// sendPresenceToDiscord performs the actual opcode-3 UpdateStatusComplex call
// and, on success, records what was sent for dedup and for the echo
// suppression that applyPresence and HandleMatrixPresence rely on. Must be
// called without presenceLock held.
func (user *User) sendPresenceToDiscord(sess *discordgo.Session, status, statusText string) {
	var activities []*discordgo.Activity
	if statusText != "" {
		activities = []*discordgo.Activity{{
			Name:  "Custom Status",
			Type:  discordgo.ActivityTypeCustom,
			State: statusText,
		}}
	}
	// Leave activities nil (JSON null = "don't change activities") when there is
	// no custom status text, so a presence-only change doesn't clear an active
	// game or Spotify session. An empty slice would serialize as [] and wipe them.
	err := sess.UpdateStatusComplex(discordgo.UpdateStatusData{
		Status:     status,
		Activities: activities,
	})
	if err != nil {
		user.log.Warn().Err(err).
			Str("discord_status", status).
			Msg("Failed to update Discord status from Matrix presence")
		return
	}
	now := time.Now()
	user.presenceLock.Lock()
	user.matrixPresenceSetAt = now
	user.lastSentToDiscordStatus = status
	user.lastSentToDiscordText = statusText
	user.lastSentDiscordStatus = status
	user.lastSentDiscordStatusText = statusText
	user.presenceLock.Unlock()
	user.log.Debug().
		Str("discord_status", status).
		Str("status_text", statusText).
		Msg("Bridged Matrix presence to Discord")
}

// startPresenceKeepalive launches a background goroutine that re-sends
// non-offline Matrix presence every presenceKeepaliveInterval. Must be called
// with no locks held. Cancels any previously-running keepalive first so a
// reconnect (readyHandler firing again) does not accumulate goroutines.
func (user *User) startPresenceKeepalive() {
	user.Lock()
	// If we raced with Logout (Session is nil), don't start a new goroutine.
	if user.Session == nil {
		user.Unlock()
		return
	}
	if user.stopKeepalive != nil {
		user.stopKeepalive()
	}
	ctx, cancel := context.WithCancel(context.Background())
	user.stopKeepalive = cancel
	user.Unlock()

	// Discard the ghost-presence cache from the previous connection. Discord's
	// GUILD_CREATE lists only include non-offline members in large guilds, so a
	// ghost whose user went offline during the disconnect may have no entry in the
	// READY/GUILD_CREATE payload to overwrite the stale "online" record. Clearing
	// ensures seedPresences rebuilds from authoritative fresh data; the keepalive
	// fires against an empty map before repopulation, which is safe (does nothing).
	//
	// The own-account dedup (lastOwnMatrixPresence/StatusMsg) is intentionally NOT
	// cleared here. Clearing it would force seedPresences to write the Discord
	// snapshot through customIntent on reconnect, clobbering any Charm-side state
	// (idle, DND, [dnd] marker) set while the bridge was disconnected. Own-account
	// presence is driven by live PRESENCE_UPDATE events; seedPresences skips the
	// bridge user's own Discord ID to avoid the reconnect-seed clobber.
	user.presenceLock.Lock()
	clear(user.presenceCache)
	user.presenceLock.Unlock()

	user.log.Debug().Msg("Starting presence keepalive goroutine")
	go user.runPresenceKeepalive(ctx)
}

func (user *User) runPresenceKeepalive(ctx context.Context) {
	ticker := time.NewTicker(presenceKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			user.presenceLock.Lock()
			snapshot := make(map[string]presenceCacheEntry, len(user.presenceCache))
			for k, v := range user.presenceCache {
				snapshot[k] = v
			}
			user.presenceLock.Unlock()

			user.log.Debug().Int("count", len(snapshot)).Msg("Presence keepalive tick: refreshing cached presences")
			for discordID, entry := range snapshot {
				puppet := user.bridge.GetPuppetByIDIfExists(discordID)
				if puppet == nil {
					continue
				}
				if err := setMatrixPresence(puppet.DefaultIntent(), entry.presence, entry.statusMsg); err != nil {
					user.log.Warn().Err(err).Str("discord_user_id", discordID).Msg("Failed to refresh Matrix presence keepalive")
				}
				if customIntent := puppet.CustomIntent(); customIntent != nil {
					customUser := user.bridge.GetUserByMXIDIfExists(puppet.CustomMXID)
					if customUser == nil || customUser.Session == nil {
						_ = setMatrixPresence(customIntent, entry.presence, entry.statusMsg)
					}
					// If customUser has an active session, skip — the bridge user's
					// real Matrix account is managed by their Matrix client (Charm).
					// The keepalive is only for ghost users who never /sync and whose
					// Synapse presence expires. Discord→Matrix own-presence sync is
					// handled by applyPresence on PRESENCE_UPDATE events instead;
					// asserting it here would override the client's idle/offline state.
				}
			}
		}
	}
}

// seedPresences applies presence for a batch of Presence entries received in
// READY or GUILD_CREATE payloads so that already-online users are reflected in
// Matrix immediately after a connect or reconnect.
func (user *User) seedPresences(presences []*discordgo.Presence) {
	// Snapshot DiscordID under the user lock to avoid a data race with Logout(),
	// which holds user.Lock() while writing user.DiscordID = "".
	user.Lock()
	ownID := user.DiscordID
	user.Unlock()

	// Deduplicate by Discord user ID: READY/GUILD_CREATE presences are guild-scoped
	// so the same user appears once per mutual guild. Without deduplication, a user
	// shared across many guilds would trigger many concurrent Matrix presence PUTs
	// against the same puppet, hitting rate limits and leaving statuses stale.
	seen := make(map[string]struct{}, len(presences))
	var applied, skippedNoPuppet, skippedEmptyStatus int
	for _, p := range presences {
		if p.User == nil {
			continue
		}
		if _, ok := seen[p.User.ID]; ok {
			continue
		}
		seen[p.User.ID] = struct{}{}
		// Skip entries with no status — GUILD_CREATE can include partial
		// member records with Status == "" that map to PresenceOffline in
		// discordStatusToMatrix, which would actively clear a presence that
		// was just set. presenceUpdateHandler has the same guard.
		if p.Status == "" {
			skippedEmptyStatus++
			continue
		}
		if p.User.ID == ownID {
			// Seed the Discord status cache so the first Matrix→Discord sync
			// after reconnect doesn't clobber a pre-existing Discord custom status
			// with an empty fallback, then skip applyPresence for own account.
			// Calling applyPresence here would write Discord's snapshot through
			// customIntent, clobbering any Charm-side state (idle, DND, [dnd] marker)
			// set while the bridge was disconnected. Own-account presence is driven
			// by live PRESENCE_UPDATE events, not by reconnect seeds.
			user.presenceLock.Lock()
			user.lastDiscordStatusText = discordCustomStatusText(p.Activities)
			user.presenceLock.Unlock()
			continue
		}
		// Reflecting other users' presence onto Matrix is the gated Discord→Matrix
		// direction. Own-account caching above still runs so the Matrix→Discord
		// fallback text stays seeded even when this direction is off.
		if !user.bridge.Config.Bridge.SyncDiscordPresenceToMatrix {
			continue
		}
		puppet := user.bridge.GetPuppetByIDIfExists(p.User.ID)
		if puppet == nil {
			skippedNoPuppet++
			continue
		}
		applied++
		user.applyPresence(p.User.ID, p.Status, discordCustomStatusText(p.Activities))
	}
	user.log.Debug().
		Int("total", len(presences)).
		Int("applied", applied).
		Int("skipped_no_puppet", skippedNoPuppet).
		Int("skipped_empty_status", skippedEmptyStatus).
		Msg("Seeded Discord presences")
}

// discordCustomStatusText extracts the State field from the first
// ActivityTypeCustom activity in the slice, which is how Discord represents a
// user's custom status message.
func discordCustomStatusText(activities []*discordgo.Activity) string {
	for _, a := range activities {
		if a.Type == discordgo.ActivityTypeCustom {
			return a.State
		}
	}
	return ""
}

func (user *User) presenceUpdateHandler(p *discordgo.PresenceUpdate) {
	if p.User == nil {
		return
	}
	// Snapshot DiscordID under the user lock to avoid a data race with Logout(),
	// which holds user.Lock() while writing user.DiscordID = "".
	user.Lock()
	ownID := user.DiscordID
	user.Unlock()
	// Discord PRESENCE_UPDATE can be partial — any combination of fields may be
	// absent. An empty Status means only non-presence fields (e.g. username)
	// changed; skip to avoid mapping "" to offline and incorrectly marking the
	// puppet as offline when only unrelated user data was updated.
	if p.Status == "" {
		return
	}
	// If this is a presence update for the bridge user themselves, cache the
	// Discord-side custom status text for fallback in HandleMatrixPresence, then
	// fall through to applyPresence so their Matrix puppet also reflects the change.
	if p.User.ID == ownID {
		user.presenceLock.Lock()
		user.lastDiscordStatusText = discordCustomStatusText(p.Activities)
		user.presenceLock.Unlock()
	}
	// Own-account status is cached above unconditionally because the Matrix→Discord
	// direction relies on it for its fallback text. Reflecting presence onto Matrix
	// (for own or other users) is the Discord→Matrix direction and is gated.
	if !user.bridge.Config.Bridge.SyncDiscordPresenceToMatrix {
		return
	}
	user.applyPresence(p.User.ID, p.Status, discordCustomStatusText(p.Activities))
}

func (user *User) ensureInvited(intent *appservice.IntentAPI, roomID id.RoomID, isDirect, ignoreCache bool) bool {
	if roomID == "" {
		return false
	}
	if intent == nil {
		intent = user.bridge.Bot
	}
	if !ignoreCache && intent.StateStore.IsInvited(roomID, user.MXID) {
		return true
	}
	ret := false

	inviteContent := event.Content{
		Parsed: &event.MemberEventContent{
			Membership: event.MembershipInvite,
			IsDirect:   isDirect,
		},
		Raw: map[string]interface{}{},
	}

	customPuppet := user.bridge.GetPuppetByCustomMXID(user.MXID)
	if customPuppet != nil && customPuppet.CustomIntent() != nil {
		inviteContent.Raw["fi.mau.will_auto_accept"] = true
	}

	_, err := intent.SendStateEvent(roomID, event.StateMember, user.MXID.String(), &inviteContent)

	var httpErr mautrix.HTTPError
	if err != nil && errors.As(err, &httpErr) && httpErr.RespError != nil && strings.Contains(httpErr.RespError.Err, "is already in the room") {
		user.bridge.StateStore.SetMembership(roomID, user.MXID, event.MembershipJoin)
		ret = true
	} else if err != nil {
		user.log.Error().Err(err).Str("room_id", roomID.String()).Msg("Failed to invite user to room")
	} else {
		ret = true
	}

	if customPuppet != nil && customPuppet.CustomIntent() != nil {
		err = customPuppet.CustomIntent().EnsureJoined(roomID, appservice.EnsureJoinedParams{IgnoreCache: true})
		if err != nil {
			user.log.Warn().Err(err).Str("room_id", roomID.String()).Msg("Failed to auto-join room")
			ret = false
		} else {
			ret = true
		}
	}

	return ret
}

func (user *User) getDirectChats() map[id.UserID][]id.RoomID {
	chats := map[id.UserID][]id.RoomID{}

	privateChats := user.bridge.DB.Portal.FindPrivateChatsOf(user.DiscordID)
	for _, portal := range privateChats {
		if portal.MXID != "" {
			puppetMXID := user.bridge.FormatPuppetMXID(portal.Key.Receiver)

			chats[puppetMXID] = []id.RoomID{portal.MXID}
		}
	}

	return chats
}

func (user *User) updateDirectChats(chats map[id.UserID][]id.RoomID) {
	if !user.bridge.Config.Bridge.SyncDirectChatList {
		return
	}

	puppet := user.bridge.GetPuppetByMXID(user.MXID)
	if puppet == nil {
		return
	}

	intent := puppet.CustomIntent()
	if intent == nil {
		return
	}

	method := http.MethodPatch
	if chats == nil {
		chats = user.getDirectChats()
		method = http.MethodPut
	}

	user.log.Debug().Msg("Updating m.direct list on homeserver")

	var err error
	if user.bridge.Config.Homeserver.Software == bridgeconfig.SoftwareAsmux {
		urlPath := intent.BuildURL(mautrix.ClientURLPath{"unstable", "com.beeper.asmux", "dms"})
		_, err = intent.MakeFullRequest(mautrix.FullRequest{
			Method:      method,
			URL:         urlPath,
			Headers:     http.Header{"X-Asmux-Auth": {user.bridge.AS.Registration.AppToken}},
			RequestJSON: chats,
		})
	} else {
		existingChats := map[id.UserID][]id.RoomID{}

		err = intent.GetAccountData(event.AccountDataDirectChats.Type, &existingChats)
		if err != nil {
			user.log.Warn().Err(err).Msg("Failed to get m.direct event to update it")
			return
		}

		for userID, rooms := range existingChats {
			if _, ok := user.bridge.ParsePuppetMXID(userID); !ok {
				// This is not a ghost user, include it in the new list
				chats[userID] = rooms
			} else if _, ok := chats[userID]; !ok && method == http.MethodPatch {
				// This is a ghost user, but we're not replacing the whole list, so include it too
				chats[userID] = rooms
			}
		}

		err = intent.SetAccountData(event.AccountDataDirectChats.Type, &chats)
	}

	if err != nil {
		user.log.Warn().Err(err).Msg("Failed to update m.direct event")
	}
}

func (user *User) bridgeGuild(guildID string, everything bool) error {
	guild := user.bridge.GetGuildByID(guildID, false)
	if guild == nil {
		return errors.New("guild not found")
	}
	meta, _ := user.Session.State.Guild(guildID)
	err := guild.CreateMatrixRoom(user, meta)
	if err != nil {
		return err
	}
	log := user.log.With().Str("guild_id", guild.ID).Logger()
	user.addGuildToSpace(guild, false, time.Now())
	for _, ch := range meta.Channels {
		portal := user.GetPortalByMeta(ch)
		if (everything && user.channelIsBridgeable(ch)) || ch.Type == discordgo.ChannelTypeGuildCategory {
			err = portal.CreateMatrixRoom(user, ch)
			if err != nil {
				log.Error().Err(err).Str("channel_id", ch.ID).
					Msg("Failed to create room for guild channel while bridging guild")
			}
		}
	}
	if everything {
		guild.BridgingMode = database.GuildBridgeEverything
	} else {
		guild.BridgingMode = database.GuildBridgeCreateOnMessage
	}
	guild.Update()

	// In subscribe-all mode, subscribe immediately after bridging. In on-demand
	// mode, skip it: the guild will be subscribed when a Matrix client is next
	// active in one of its rooms.
	if user.Session.IsUser && user.bridge.Config.Bridge.SyncDiscordPresenceToMatrix &&
		user.bridge.Config.Bridge.DiscordPresenceSubscribeAll {
		log.Debug().Msg("Subscribing to guild after bridging")
		user.sendGuildPresenceSubscribe(user.Session, guild.ID)
	}

	return nil
}

func (user *User) unbridgeGuild(guildID string) error {
	if user.PermissionLevel < bridgeconfig.PermissionLevelAdmin && user.PortalHasOtherUsers(guildID) {
		return errors.New("only bridge admins can unbridge guilds with other users")
	}
	guild := user.bridge.GetGuildByID(guildID, false)
	if guild == nil {
		return errors.New("guild not found")
	}
	guild.roomCreateLock.Lock()
	defer guild.roomCreateLock.Unlock()
	if guild.BridgingMode == database.GuildBridgeNothing && guild.MXID == "" {
		return errors.New("that guild is not bridged")
	}
	guild.BridgingMode = database.GuildBridgeNothing
	guild.Update()
	for _, portal := range user.bridge.GetAllPortalsInGuild(guild.ID) {
		portal.cleanup(false)
		portal.RemoveMXID()
	}
	guild.cleanup()
	guild.RemoveMXID()
	return nil
}
