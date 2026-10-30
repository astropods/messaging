# Summary

Files did not travel between a Slack user and an agent, in either direction.

- Inbound: a file a user attached in Slack never reached the agent unless it was an image. A DM with an upload was dropped completely, because Slack sends it as a `file_share` message and the adapter ignored that subtype.
- Outbound: the adapter ignored the files an agent returned on its reply, so an agent could not hand back a generated doc or PDF.

Files on an @-mention or a DM now reach the agent on its files volume, the same way a web chat upload does. Files the agent returns are uploaded into the reply's thread.

# Design

## Inbound

The adapter streams each file with the bot token straight into the files store, so a large file never sits in the sidecar's memory. The message carries it as `Attachment{FILE}` with `storage_key`, so adapter-core resolves `StreamOptions.attachments[].path` with no SDK change. An image of 2 MiB or less keeps the inline data URI path, so the model sees it. A larger image does not fit the gRPC frame inlined, so it is staged as a file like any other upload.

- Files are staged in `dispatch` after authz passes. A denied user's upload never lands on the agent's volume.
- The thread owns a staged file: `UploadedBy` is `slack:<conversation id>`. Everyone in a thread can see its files in Slack, and nobody outside it can, so the thread is the owner that matches Slack. The user who mentions the agent is often not the one who uploaded the file. No web chat user owns a Slack file, so it is in no one's web file list.
- An in-thread mention also stages the files on earlier messages in the thread, the newest 20, each once. The adapter already fetches the thread for `[slack_thread_summary]`, so this costs no extra call. A user can drop a document in a thread and later ask the agent about it. Earlier images are staged as files, not inlined, so a long thread cannot overrun the gRPC frame.
- A staged file is keyed `slack-<file ID>`. A file already in the store is reused without a download, so mentions in a long thread do not download its files again.
- `handleMessage` accepts the `file_share` subtype in DMs and reads the files from `ev.Message`, which slack-go fills from the top-level event. Images in a DM are inlined, the same as on a mention. In channels the bot receives every thread reply, so a `file_share` there is still dropped. A file dropped in a thread without a mention reaches the agent with the next mention in that thread.
- A staged file is held to the files limit (see "One files limit" below). The limit applies to the declared size before the download and to the bytes written, so a file larger than Slack declared is still stopped.
- These files are skipped: external files (linked Google Drive), files over the limit, files that do not fit the volume reserve, failed downloads, and downloads that return the Slack sign-in page. When `FILES_DIR` is unset, every file that is not inlined is skipped. Reactions do not stage files, so a reacted image over 2 MiB is skipped there.

A skipped file is named in the message, one line per file after the user's text:

```
[slack_file_skipped] account-plan.pdf: Slack refused the download
```

Without the line, an agent that got no file asks the user to send it again, though the user already did. With it, the agent can say which file it could not open and why. Mentions, DMs, and reactions all add it.

Extraction stays in agent code. The agent gets the original bytes and chooses how to read them: a PDF part to the model, a parser, or a document API.

`files.StorageReserveBytes` replaces the web adapter's private constant so both adapters keep the same headroom.

## One files limit

`FILES_MAX_MB` sets the largest file, in MiB, that the sidecar moves in either direction, through any adapter:

| Path | Over the limit |
|---|---|
| Web chat upload | The files API answers 413 |
| Slack file a user attaches | Skipped with a `[slack_file_skipped]` note |
| File the agent returns to web chat | No download chip; logged |
| File the agent returns to Slack | Not uploaded; logged, and the text reply still posts |

Unset means 100 MiB, the web upload limit before it was configurable. `0` turns the per-file cap off. A value that is not a whole number of MiB logs a warning and uses the default, so a typo cannot stop the sidecar. The disk-space reserve (`files.StorageReserveBytes`) applies whatever the limit, so an uncapped file still cannot fill the volume. `files.EffectiveLimit` and `files.Exceeds` hold the rule in one place, and `NoFileLimit` (`-1`) is the uncapped value. The web adapter's fixed `filesMaxUploadBytes` is gone.

## Outbound

adapter-core sends the agent's files on the END chunk as `ResponseAttachment.file`, named by their files-store key. On END the adapter streams each file from the store to the thread with slack-go's `UploadFileContext`, after the text reply and any cards.

- Before the upload, the adapter claims the file for the thread with `AttributeOwner`, the compare-and-set the web adapter uses for a conversation's owner. The first thread to post a file owns it. All users' files share one directory and the model picks the name, so without the claim a user in another channel could ask for `summary.md` and get the first thread's file. A file that another thread or a web user owns is not posted. The thread is told the agent did not save it, so a name reveals nothing. A thread can get its own files again, uploads included.
- An END with files and no text now posts the files. Before, an END with no text was skipped whole, cards included.
- An unknown, not-ready, or empty file, a file over the limit, or a failed upload is logged and skipped. The text reply is never blocked. One short note in the thread then names each file that was not posted and why, for example `:warning: report.csv could not be sent: file is larger than 100 MiB`, because the agent's reply usually says the file is attached. The note is escaped, since the agent picks the file name.

# Migration

The Slack app needs two bot scopes:

- `files:read` to read user uploads. Inline images already need it, so most apps have it.
- `files:write` to post agent files. A failed upload logs a hint that names it.

Content grows by one `[slack_file_skipped]` line for each file that does not reach the agent. A message whose files all arrive is unchanged.

To change the file limit, set `FILES_MAX_MB` on the sidecar, for example `250`, or `0` for no per-file limit. Leave it unset for the 100 MiB default. astro-server shows it on the deploy form as "Max File Size (MiB)". Keep the limit under the agent's disk size, because staged files share the volume with chat history and agent output.

An agent reads inbound files from `StreamOptions.attachments`. The Mastra adapter already tells the model the paths. The agent needs a tool that can read each format it accepts. To return a file, the agent writes it to its files dir and calls `hooks.onFile`, the same path web chat uses.
