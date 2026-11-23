# Summary

The chat store and the conversation context store can now live in Postgres, so several messaging sidecars can share one chat history. This is the store half of running an agent with more than one replica. Before this change, chat lived in a SQLite file on the agent's disk and contexts lived in memory, so each sidecar had its own copy.

`CHAT_DATABASE_URL` turns it on. Without it, the sidecar behaves exactly as before: SQLite at `CHAT_DB_PATH` and contexts per `STORAGE_TYPE`.

# Design

One implementation serves both databases. The package moves from `internal/store/sqlite` to `internal/store/chatstore`, and `*chatstore.Store` keeps every method it had. `Open(path)` opens SQLite. `OpenPostgres(ctx, url)` opens Postgres through `pgx/v5/stdlib`. A thin wrapper around `*sql.DB` and `*sql.Tx` rewrites `?` placeholders as `$1, $2, ...` on Postgres, so the SQL is shared. The only query change is an alias on the seq-allocation subquery, which Postgres requires.

| Concern | SQLite | Postgres |
|---|---|---|
| Writer serialization | One connection | `pg_advisory_xact_lock` per conversation, taken by `beginConversation` at the start of every write transaction |
| Schema | `CREATE TABLE IF NOT EXISTS` plus `ensureColumn` | Numbered migrations in `postgresMigrations`, tracked in `astro_messaging.schema_version` |
| Startup reaper | Reaps every user-last turn | Reaps only turns idle for 15 minutes (`sharedReapGrace`) with no pending interaction, so a restarting sidecar leaves another sidecar's live turn alone. Answering an interaction doesn't touch `updated_at`, so the grace alone can't cover a turn waiting on the user. |
| NUL bytes | Stored as given | Stripped from string arguments, because Postgres `TEXT` can't store them |
| Conversation contexts | Memory or Redis | `conversation_contexts` table with the `STORAGE_TTL` expiry |

Every table lives in the `astro_messaging` schema. The connection's `search_path` points there, so the sidecar never reads or writes the agent's own tables in the same database. Migrations run in one transaction under an advisory lock, so two sidecars that start together migrate once. A schema newer than the build refuses to start rather than run against unknown columns.

The per-conversation lock is what keeps two writers safe. The seq allocator, the message cap, `UpsertAssistantProgress`, `FinalizeStopped`, branch edits, interactions and saves all read and then write inside one transaction. Holding the conversation's lock for that transaction gives Postgres the same guarantee the single SQLite connection gave: two sidecars streaming into one conversation cannot both append an assistant row or take the same seq.

On Postgres every statement is a network round trip, so the write paths are kept short:

| Path | Statements after the lock |
|---|---|
| Streaming progress, every 250 ms per reply | 1: a CTE grows the trailing assistant row and touches the conversation. Only the first chunk of a turn takes the general path. |
| Append a message | 2: next seq and message count in one query, then a CTE that inserts and resets the branch head |
| First send (`EnsureForSend`) | 1, no transaction: the upsert's `RETURNING` reports ownership |
| Delete | 2: `UPDATE ... RETURNING` cancels pending interactions |

The next seq is the greater of two `MAX(seq)` lookups, one on messages and one on interactions, so each reads one end of its `(conversation_id, seq)` index instead of scanning the conversation. The sidebar query is served by a partial index on `(user_id, room_id, updated_at DESC) WHERE deleted_at IS NULL`. The seq, `RETURNING` and delete changes also apply to SQLite. The progress and append CTEs are Postgres-only, because SQLite has no data-modifying CTEs.

`Store.Contexts(ctx, ttl)` returns a `store.ConversationStore` on the same pool. `Create` refuses a live context and replaces an expired one. `Update` upserts, as `RedisStore` does. Expired contexts are deleted when the store opens.

Files are unchanged: they stay on disk, which a shared volume can provide.

# Migration

None. Nothing sets `CHAT_DATABASE_URL` yet. astro-server will set it in HA mode, together with the knowledge entry's credentials. The first Postgres start creates the `astro_messaging` schema, so the database user needs `CREATE` on the database. The owner of a CloudNativePG `app` database has it. Importing an existing `chat.db` into Postgres is not part of this change.
