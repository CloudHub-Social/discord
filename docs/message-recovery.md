# Restore one missed Discord edit

In your existing bridge management room, a bridge administrator can run:

```text
resync-message <matrixRoomID> <discordMessageID>
```

Use your configured command prefix when needed. The caller must be a configured
bridge administrator with a connected Discord session, and the command must be
sent in their own management room. The target room and message must already have
bridge database mappings. The bridge reads the exact current message from Discord
history, validates its identity and author, and queues a normal Discord edit. It
does not accept replacement text or send anything to Discord.

The existing portal writer handles conversion, encryption, attachment changes,
and edit timestamp deduplication. Encrypted rooms stay encrypted. This restores
the current version; Discord does not supply earlier edit revisions here.

A queued response does not mean delivered. Verify the ordinary
`Finished handling Discord edit` log and persisted edit timestamp, then read the
Matrix replacement event. An already applied timestamp is a no-op. A full queue
or upstream fetch failure must be investigated before retrying. Never change
permissions, reset authentication, or recreate a room to perform recovery.
