# Summary

Outside a DM, the Slack adapter dropped every message that carried an upload, text included. Slack sends such a message with the `file_share` subtype. So a thread reply with a file never reached the agent, though the same reply without a file did. An observe channel also lost every top-level message with a file.

A `file_share` message now reaches the agent under the same rules as any other message. Outside a DM, the message names its files but the adapter does not download them. The next mention of the bot in the thread stages them, as it already does for earlier files in a thread.

# Design

`handleMessage` no longer returns early for `file_share` outside a DM. Its usual rules decide what happens to the message:

| Message | Result |
|---------|--------|
| Thread reply in a channel | Forwarded as `EVENT_KIND_THREAD_REPLY` |
| Top-level message in an observe channel, or one that mentions a user in `mention_user_ids` | Forwarded as `EVENT_KIND_OBSERVED` |
| Other top-level channel message | Dropped |
| Message that mentions the bot | Delivered once, through `app_mention` |

A forwarded channel message keeps its text and gets one note for each file:

```
[slack_file_skipped] deals.csv: not downloaded until the bot is mentioned in this thread
```

The adapter downloads nothing for these messages, images included. An app that subscribes to `message.channels` or `message.groups` receives every reply in every thread the bot is in, and in every observe channel. Downloading those files would fill the agent's volume, because staged files have no retention. It would also delay other events while each file downloads. A mention already stages up to 20 earlier files in its thread, keyed by Slack file ID, so a file named in a reply reaches the agent when someone asks the bot about it.

DM uploads are unchanged: images of 2 MiB or less are inlined, and other files are staged after authz.

Slack's Bolt framework also delivers these messages: no default middleware drops `file_share` messages or thread replies. A Bolt app downloads `url_private` itself with its bot token. Agents here hold no Slack token, so a download from a channel waits for a mention.

# Migration

None. An app that subscribes only to `app_mention` receives no thread replies, so nothing changes for it.
