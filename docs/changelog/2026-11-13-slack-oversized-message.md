# Summary

A Slack mention with many screenshots disconnected the agent from Slack until its pod restarted. The adapter inlined every image of 2 MiB or less, with no cap on their total. Nine screenshots came to 6.8 MB of base64, over the sidecar's 4 MiB gRPC frame limit. gRPC ends a stream when a send fails, so the failed send closed the shared agent stream. The agent reconnected, but its bridge sent nothing on the new stream, so the sidecar never registered it. Every later message in every channel failed with `no active agent stream available`, logged at debug only.

Now:

- A Slack message inlines up to 2 MiB of image bytes in total, the same budget web chat uses. Images past the budget are staged on the files volume and reach the agent as files.
- The sidecar checks a message's size before it sends it to the agent. An oversized message fails alone with `ErrMessageTooLarge`, the stream stays open, and Slack tells the user to send fewer or smaller files.
- A message dropped because no agent is connected, or because it is too large, is logged at warn with its platform, conversation, channel, thread, and message ID.

The reconnect half of the incident is fixed in the agent bridge (`astropods/adapters`), which now sends its agent config and registration again on every reconnect.

# Design

`conversationStream.send` wraps every sidecar-to-agent send: incoming messages, feedback, audio, and `SendToAgent`. It compares `proto.Size` with `maxMessageBytes`, the 4 MiB limit the server is built with, and returns `adapter.ErrMessageTooLarge` instead of calling `Send`. grpc-go's `serverStream.SendMsg` writes a terminal status on any send error, so the check has to run before the send, not after.

`imageAttachments` keeps a running inline budget of `maxInlineImageBytes` across one message. It also returns the files it did not inline: non-image files, images past the budget, and images whose download turned out larger than Slack declared. `dispatch` stages exactly those files after authz, so `fileAttachments` no longer guesses which images were inlined from their size. Earlier thread files are staged as before.

The reaction path still stages nothing. An image past the budget there gets a skip note: `the message's images are larger than 2 MiB in total`.

The size check is the backstop for any adapter: a long thread summary with inline images, or a web message, cannot end the stream either.

# Migration

None for the sidecar. Agents built on an `@astropods/adapter-core` without the bridge fix still lose their registration if the stream drops for another reason, such as a sidecar restart, until they rebuild on the new release.

The public Files in Slack page in `astropods/astro` says an image of 2 MiB or less reaches the model inline. It needs the web chat wording, 2 MiB of image bytes per message in total, when this release reaches prod.
