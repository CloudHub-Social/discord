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

func (br *DiscordBridge) HandleMatrixPresence(evt *event.Event) {
	if br.IsGhost(evt.Sender) {
		return
	}
	content, ok := evt.Content.Parsed.(*event.PresenceEventContent)
	if !ok {
		return
	}
	user := br.GetCachedUserByMXID(evt.Sender)
	if user == nil || user.Session == nil {
		return
	}
	discordStatus := matrixPresenceToDiscord(content.Presence)

	// rawStatusText is the unmodified status_msg from Matrix (before DND stripping).
	rawStatusText := content.StatusMessage
	statusText := rawStatusText

	// Check for the Charm client DND prefix and strip it if present.
	// Handles both "[dnd]" (no custom text) and "[dnd] <text>" forms.
	if rest, ok := strings.CutPrefix(statusText, charmDNDPrefix); ok {
		statusText = strings.TrimPrefix(rest, " ")
		discordStatus = string(discordgo.StatusDoNotDisturb)
	}

	// Determine the status text to send to Discord using last-writer-wins
	// with intentional-clear detection:
	//
	//  - rawStatusText != "": Matrix is explicitly setting a status — cache it
	//    and use it.
	//  - rawStatusText == "" && matrixStatusEverSet: Matrix previously had a
	//    value and is now clearing it — treat as intentional clear, wipe both
	//    cached values and send empty.
	//  - rawStatusText == "" && !matrixStatusEverSet: Matrix has never sent a
	//    status in this session — fall back to the last Discord-side status so
	//    we don't clobber it.
	user.presenceLock.Lock()
	var textToSend string
	if rawStatusText != "" {
		user.lastMatrixStatusText = statusText
		user.matrixStatusEverSet = true
		textToSend = statusText
	} else if user.matrixStatusEverSet {
		user.lastMatrixStatusText = ""
		user.lastDiscordStatusText = ""
		textToSend = ""
	} else {
		textToSend = user.lastDiscordStatusText
	}
	user.presenceLock.Unlock()

	var activities []*discordgo.Activity
	if textToSend != "" {
		activities = []*discordgo.Activity{{
			Name:  "Custom Status",
			Type:  discordgo.ActivityTypeCustom,
			State: textToSend,
		}}
	} else {
		activities = make([]*discordgo.Activity, 0)
	}

	err := user.Session.UpdateStatusComplex(discordgo.UpdateStatusData{
		Status:     discordStatus,
		Activities: activities,
	})
	if err != nil {
		br.ZLog.Warn().Err(err).
			Str("user_id", evt.Sender.String()).
			Str("matrix_presence", string(content.Presence)).
			Msg("Failed to update Discord status from Matrix presence")
	} else {
		br.ZLog.Debug().
			Str("user_id", evt.Sender.String()).
			Str("matrix_presence", string(content.Presence)).
			Str("discord_status", discordStatus).
			Str("status_text", textToSend).
			Msg("Bridged Matrix presence to Discord")
	}
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
		if user.Session == nil {
			continue
		}

		br.Log.Debugln("Disconnecting", user.MXID)
		user.Session.Close()
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
