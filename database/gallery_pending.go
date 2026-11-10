// mautrix-discord - A Matrix-Discord puppeting bridge.
// Copyright (C) 2026
// Licensed under the GNU Affero General Public License, version 3 or later.
package database

import (
	"database/sql"
	"errors"

	"maunium.net/go/mautrix/id"
)

// GalleryPending is a durable reservation for an outbound Discord send. An
// unknown outcome remains reserved until the gateway confirms it or an operator
// reconciles it; retrying an unknown send can create duplicate messages.
type GalleryPending struct {
	MXID       id.EventID
	Channel    PortalKey
	ThreadID   string
	SenderID   string
	SenderMXID id.UserID
	Nonce      string
}

func (mq *MessageQuery) ReserveGallery(p GalleryPending) (bool, error) {
	result, err := mq.db.Exec(`INSERT INTO gallery_pending (mxid, dc_chan_id, dc_chan_receiver, dc_thread_id, dc_sender, sender_mxid, nonce) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, p.MXID, p.Channel.ChannelID, p.Channel.Receiver, p.ThreadID, p.SenderID, p.SenderMXID, p.Nonce)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (mq *MessageQuery) PendingGalleryByNonce(key PortalKey, nonce, sender string) (*GalleryPending, error) {
	p := &GalleryPending{Channel: key}
	err := mq.db.QueryRow(`SELECT mxid, dc_thread_id, dc_sender, sender_mxid, nonce FROM gallery_pending WHERE dc_chan_id=$1 AND dc_chan_receiver=$2 AND nonce=$3 AND dc_sender=$4`, key.ChannelID, key.Receiver, nonce, sender).Scan(&p.MXID, &p.ThreadID, &p.SenderID, &p.SenderMXID, &p.Nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (mq *MessageQuery) ClearPendingGallery(mxid id.EventID) error {
	_, err := mq.db.Exec(`DELETE FROM gallery_pending WHERE mxid=$1`, mxid)
	return err
}

func (mq *MessageQuery) HasPendingGallery(mxid id.EventID) (bool, error) {
	var exists bool
	err := mq.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM gallery_pending WHERE mxid=$1)`, mxid).Scan(&exists)
	return exists, err
}
