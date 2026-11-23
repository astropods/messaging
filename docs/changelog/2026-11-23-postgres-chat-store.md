# Summary

The chat store and the conversation context store can now live in Postgres, so several messaging sidecars can share one chat history. This is the store half of running an agent with more than one replica. Before this change, chat lived in a SQLite file on the agent's disk and contexts lived in memory, so each sidecar had its own copy.

`CHAT_DATABASE_URL` turns it on. Without it, the sidecar behaves exactly as before: SQLite at `CHAT_DB_PATH` and contexts per `STORAGE_TYPE`.

# Design

One implementation serves both databases. The package moves from `internal/store/sqlite` to `internal/store/chatstore`, and `*chatstore.Store` keeps every method it had. `Open(path)` opens SQLite. `OpenPostgres(ctx, url)` opens Postgres through `pgx/v5/stdlib`. A thin wrapper around `*sql.DB` and `*sql.Tx` rewrites `?` placeholders as `$1, $2, ...` on Postgres, so the SQL is shared. The only query change is an alias on the seq-allocation subquery, which Postgres requires.

| Concern | SQLite | Postgres |
|---|---|---|
| Writer serialization | One connection | `pg_advisory_xact_lock` per conversation, taken by `beginConversation` at the start of every write transaction |
| Schema | `CREATE TABLE IF NOT EXISTS` plus `ensureColumn` | Numbered migrations in `postgresMigrations`, tracked in `astro_messaging.schema_version` |
| Startup reaper | Reaps every user-last turn | Reaps only turns idle for 15 minutes (`sharedReapGrace`), so a restarting sidecar leaves another sidecar's live turn alone |
| Conversation contexts | Memory or Redis | `conversation_contexts` table with the `STORAGE_TTL` expiry |

Every table lives in the `astro_messaging` schema. The connection's `search_path` points there, so the sidecar never reads or writes the agent's own tables in the same database. Migrations run in one transaction under an advisory lock, so two sidecars that start together migrate once. A schema newer than the build refuses to start rather than run against unknown columns.

The per-conversation lock is what keeps two writers safe. The seq allocator, the message cap, `UpsertAssistantProgress`, `FinalizeStopped`, branch edits, interactions and saves all read and then write inside one transaction. Holding the conversation's lock for that transaction gives Postgres the same guarantee the single SQLite connection gave: two sidecars streaming into one conversation cannot both append an assistant row or take the same seq.

`Store.Contexts(ctx, ttl)` returns a `store.ConversationStore` on the same pool. `Create` refuses a live context and replaces an expired one. `Update` upserts, as `RedisStore` does. Expired contexts are deleted when the store opens.

Files are unchanged: they stay on disk, which a shared volume can provide.

# Migration

None. Nothing sets `CHAT_DATABASE_URL` yet. astro-server will set it in HA mode, together with the knowledge entry's credentials. The first Postgres start creates the `astro_messaging` schema, so the database user needs `CREATE` on the database. The owner of a CloudNativePG `app` database has it. Importing an existing `chat.db` into Postgres is not part of this change.
