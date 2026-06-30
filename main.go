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
	_ "embed"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"go.mau.fi/util/configupgrade"
	"go.mau.fi/util/exsync"
	"golang.org/x/sync/semaphore"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/commands"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-discord/config"
	"go.mau.fi/mautrix-discord/database"
)

// Information to find out exactly which commit the bridge was built from.
// These are filled at build time with the -X linker flag.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

//go:embed example-config.yaml
var ExampleConfig string

type DiscordBridge struct {
	bridge.Bridge

	Config *config.Config
	DB     *database.Database

	DMA          *DirectMediaAPI
	provisioning *ProvisioningAPI

	usersByMXID map[id.UserID]*User
	usersByID   map[string]*User
	usersLock   sync.Mutex

	managementRooms     map[id.RoomID]*User
	managementRoomsLock sync.Mutex

	portalsByMXID map[id.RoomID]*Portal
	portalsByID   map[database.PortalKey]*Portal
	portalsLock   sync.Mutex

	threadsByID                 map[string]*Thread
	threadsByRootMXID           map[id.EventID]*Thread
	threadsByCreationNoticeMXID map[id.EventID]*Thread
	threadsLock                 sync.Mutex

	guildsByMXID map[id.RoomID]*Guild
	guildsByID   map[string]*Guild
	guildsLock   sync.Mutex

	puppets             map[string]*Puppet
	puppetsByCustomMXID map[id.UserID]*Puppet
	puppetsLock         sync.Mutex

	attachmentTransfers         *exsync.Map[attachmentKey, *exsync.ReturnableOnce[*database.File]]
	parallelAttachmentSemaphore *semaphore.Weighted
}

func (br *DiscordBridge) GetExampleConfig() string {
	return ExampleConfig
}

func (br *DiscordBridge) GetConfigPtr() interface{} {
	br.Config = &config.Config{
		BaseConfig: &br.Bridge.Config,
	}
	br.Config.BaseConfig.Bridge = &br.Config.Bridge
	return br.Config
}

func (br *DiscordBridge) Init() {
	br.CommandProcessor = commands.NewProcessor(&br.Bridge)
	br.RegisterCommands()
	br.EventProcessor.On(event.StateTombstone, br.HandleTombstone)
	br.EventProcessor.On(event.EphemeralEventPresence, br.HandleMatrixPresence)

	matrixHTMLParser.PillConverter = br.pillConverter

	br.DB = database.New(br.Bridge.DB, br.Log.Sub("Database"))
	discordLog = br.ZLog.With().Str("component", "discordgo").Logger()
}

func matrixPresenceToDiscord(presence event.Presence) string {
	switch presence {
	case event.PresenceOnline:
		return string(discordgo.StatusOnline)
	case event.PresenceUnavailable:
		return string(discordgo.StatusIdle)
	case event.PresenceOffline:
		return string(discordgo.StatusInvisible)
	default:
		return string(discordgo.StatusInvisible)
	}
}

// charmDNDPrefix is the status_msg prefix used by the Charm Matrix client to
// indicate do-not-disturb. When present, it is stripped before forwarding the
// text to Discord and the Discord status is overridden to "dnd".
// charmDNDPrefix is the status_msg prefix used by the Charm Matrix client to
// indicate do-not-disturb. Charm sets status_msg to "[dnd]" (no custom text)
// or "[dnd] <text>" (with custom text). Both forms are detected and stripped.
const charmDNDPrefix = "[dnd]"

// discordCustomStatusMaxRunes is Discord's maximum length for a custom status message.
const discordCustomStatusMaxRunes = 128

func (br *DiscordBridge) HandleMatrixPresence(evt *event.Event) {
	// Forwarding presence to Discord (opcode 3) is opt-in: even with the
	// rate limiting below, some operators on user tokens prefer not to emit
	// presence updates at all, since Discord penalizes user tokens that send
	// them too aggressively (close code 4004).
	if !br.Config.Bridge.SyncMatrixPresenceToDiscord {
		return
	}
	content, ok := evt.Content.Parsed.(*event.PresenceEventContent)
	if !ok {
		return
	}
	// GetUserByMXIDIfExists checks the cache then the DB without creating a new
	// row, preventing phantom bridge-user rows for unrelated Matrix users who
	// share rooms with bridge users and whose presence EDUs the appservice receives.
	user := br.GetUserByMXIDIfExists(evt.Sender)
	if user == nil {
		return
	}
	// Snapshot the session under the user lock to avoid a TOCTOU race with
	// Logout(), which acquires user.Lock() and sets Session to nil. Without this,
	// the nil check and the subsequent call on line ~197 are not atomic and a
	// concurrent Logout() can cause a nil pointer dereference panic.
	user.Lock()
	sess := user.Session
	user.Unlock()
	if sess == nil {
		return
	}
	discordStatus := matrixPresenceToDiscord(content.Presence)

	// rawStatusText is the unmodified status_msg from Matrix (before DND stripping).
	rawStatusText := content.StatusMessage
	statusText := rawStatusText

	// Check for the Charm client DND prefix and strip it if present.
	// Handles both "[dnd]" (no custom text) and "[dnd] <text>" forms.
	// Always strip the prefix so the raw marker never reaches Discord as literal
	// status text, but only override to DND when the Matrix presence is not
	// offline — a client going offline while retaining a [dnd] status_msg should
	// still result in Discord invisible, not DND.
	if rest, ok := strings.CutPrefix(statusText, charmDNDPrefix); ok {
		statusText = strings.TrimPrefix(rest, " ")
		if content.Presence != event.PresenceOffline {
			discordStatus = string(discordgo.StatusDoNotDisturb)
		}
	}

	// Suppress m.presence echoes that Synapse delivers via push_ephemeral when
	// the bridge itself writes own-user presence via applyPresence (Discord→Matrix
	// own-user sync). Without suppression the echo triggers UpdateStatusComplex and
	// clobbers the Discord status. Echo check happens BEFORE state mutation so an
	// echo never sets matrixStatusEverSet or lastMatrixStatusText — those flags must
	// only advance on genuine Matrix-client changes, not bridge self-writes.
	//
	// Value matching narrows the suppression window: a genuine Charm change within
	// the 2s window only matches when both presence and raw status_msg equal what
	// the bridge last wrote, preventing deliberate user changes from being dropped.
	user.presenceLock.Lock()
	suppressUntil := user.discordPresenceSetAt.Add(2 * time.Second)
	expectedEchoPresence := user.lastOwnMatrixPresence
	expectedEchoStatus := user.lastOwnMatrixStatusMsg
	user.presenceLock.Unlock()
	if time.Now().Before(suppressUntil) &&
		content.Presence == expectedEchoPresence &&
		rawStatusText == expectedEchoStatus {
		return
	}

	// Determine the status text to send to Discord using last-writer-wins
	// with intentional-clear detection:
	//
	//  - statusText != "": Matrix set a non-empty text (after prefix stripping) —
	//    cache it and use it.
	//  - statusText == "" && rawStatusText == "" && (matrixStatusEverSet ||
	//    lastOwnMatrixStatusMsg != ""): Matrix previously had a status and now
	//    explicitly sends empty — intentional clear. The lastOwnMatrixStatusMsg
	//    guard catches the case where the Matrix account was set by applyPresence
	//    rather than directly by a Charm user action, so a clear from Charm after
	//    a Discord-originated status is still forwarded correctly.
	//  - otherwise (never set, or raw had content that stripped to empty e.g. bare
	//    "[dnd]"): fall back to the last Discord-side status so we don't clobber it.
	user.presenceLock.Lock()
	var textToSend string
	if statusText != "" {
		user.lastMatrixStatusText = statusText
		user.matrixStatusEverSet = true
		textToSend = statusText
	} else if rawStatusText == "" && (user.matrixStatusEverSet || user.lastOwnMatrixStatusMsg != "") {
		// Intentional clear: truly empty status_msg after Matrix previously set one.
		// Only reset the Matrix-side cache — lastDiscordStatusText is Discord's own
		// status and must remain as a fallback for bare DND and presence-only updates.
		user.lastMatrixStatusText = ""
		user.matrixStatusEverSet = false
		textToSend = ""
	} else {
		// Never set, or raw was a bare DND prefix with no custom text — preserve Discord's status.
		textToSend = user.lastDiscordStatusText
	}
	user.presenceLock.Unlock()

	// Truncate to Discord's custom status character limit.
	if runes := []rune(textToSend); len(runes) > discordCustomStatusMaxRunes {
		textToSend = string(runes[:discordCustomStatusMaxRunes])
	}

	// Forward to Discord with content dedup and trailing-debounce rate limiting.
	// HandleMatrixPresence fires often as Matrix clients oscillate online/idle,
	// and Discord invalidates user-account tokens that emit opcode-3 presence
	// updates at machine cadence, so the actual gateway send is throttled to at
	// most one per presenceMinInterval. forwardPresenceToDiscord skips the send
	// entirely when the effective status/text is unchanged, and records what was
	// sent so applyPresence can suppress the resulting gateway echo.
	user.forwardPresenceToDiscord(sess, discordStatus, textToSend)
}

func (br *DiscordBridge) Start() {
	if br.Config.Bridge.Provisioning.SharedSecret != "disable" {
		br.provisioning = newProvisioningAPI(br)
	}
	if br.Config.Bridge.PublicAddress != "" {
		br.AS.Router.HandleFunc("/mautrix-discord/avatar/{server}/{mediaID}/{checksum}", br.serveMediaProxy).Methods(http.MethodGet)
	}
	br.DMA = newDirectMediaAPI(br)
	br.WaitWebsocketConnected()
	go br.startUsers()
}

func (br *DiscordBridge) Stop() {
	for _, user := range br.usersByMXID {
		// Hold user.Lock across the teardown so it serializes with connect(),
		// which also holds user.Lock for its whole duration. Without this an
		// in-flight auto-reconnect could open a fresh session after we latch and
		// close, leaving Discord connected through shutdown.
		user.Lock()
		sess := user.Session
		if sess == nil {
			user.Unlock()
			continue
		}
		br.Log.Debugln("Disconnecting", user.MXID)
		// Latch before Close() so the shutdown disconnect does not reconnect.
		user.stopReconnecting()
		sess.Close()
		user.Unlock()
	}
}

func (br *DiscordBridge) GetIPortal(mxid id.RoomID) bridge.Portal {
	p := br.GetPortalByMXID(mxid)
	if p == nil {
		return nil
	}
	return p
}

func (br *DiscordBridge) GetIUser(mxid id.UserID, create bool) bridge.User {
	p := br.GetUserByMXID(mxid)
	if p == nil {
		return nil
	}
	return p
}

func (br *DiscordBridge) IsGhost(mxid id.UserID) bool {
	_, isGhost := br.ParsePuppetMXID(mxid)
	return isGhost
}

func (br *DiscordBridge) GetIGhost(mxid id.UserID) bridge.Ghost {
	p := br.GetPuppetByMXID(mxid)
	if p == nil {
		return nil
	}
	return p
}

func (br *DiscordBridge) CreatePrivatePortal(id id.RoomID, user bridge.User, ghost bridge.Ghost) {
	//TODO implement
}

func main() {
	br := &DiscordBridge{
		usersByMXID: make(map[id.UserID]*User),
		usersByID:   make(map[string]*User),

		managementRooms: make(map[id.RoomID]*User),

		portalsByMXID: make(map[id.RoomID]*Portal),
		portalsByID:   make(map[database.PortalKey]*Portal),

		threadsByID:                 make(map[string]*Thread),
		threadsByRootMXID:           make(map[id.EventID]*Thread),
		threadsByCreationNoticeMXID: make(map[id.EventID]*Thread),

		guildsByID:   make(map[string]*Guild),
		guildsByMXID: make(map[id.RoomID]*Guild),

		puppets:             make(map[string]*Puppet),
		puppetsByCustomMXID: make(map[id.UserID]*Puppet),

		attachmentTransfers:         exsync.NewMap[attachmentKey, *exsync.ReturnableOnce[*database.File]](),
		parallelAttachmentSemaphore: semaphore.NewWeighted(3),
	}
	br.Bridge = bridge.Bridge{
		Name:              "mautrix-discord",
		URL:               "https://github.com/mautrix/discord",
		Description:       "A Matrix-Discord puppeting bridge.",
		Version:           "0.7.6",
		ProtocolName:      "Discord",
		BeeperServiceName: "discordgo",
		BeeperNetworkName: "discord",

		CryptoPickleKey: "maunium.net/go/mautrix-whatsapp",

		ConfigUpgrader: &configupgrade.StructUpgrader{
			SimpleUpgrader: configupgrade.SimpleUpgrader(config.DoUpgrade),
			Blocks:         config.SpacedBlocks,
			Base:           ExampleConfig,
		},

		Child: br,
	}
	br.InitVersion(Tag, Commit, BuildTime)

	br.Main()
}
