-- v25 (compatible with v19+): Track gallery sends with uncertain outcomes
CREATE TABLE gallery_pending (
    mxid TEXT PRIMARY KEY,
    dc_chan_id TEXT NOT NULL,
    dc_chan_receiver TEXT NOT NULL,
    dc_thread_id TEXT NOT NULL,
    dc_sender TEXT NOT NULL,
    sender_mxid TEXT NOT NULL,
    nonce TEXT NOT NULL UNIQUE
);
