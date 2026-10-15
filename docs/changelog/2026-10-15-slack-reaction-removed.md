# Summary

An actionable reaction could start work but never undo it. When a person removed the reaction, the agent heard nothing, so whatever the reaction did stayed in place.

Example: Sohum reacts :knife_fork_plate: to a message, and Daily Plate puts it on his plate. He changes his mind and removes the reaction. Daily Plate now gets:

```
[reaction :knife_fork_plate: removed by <@U_SOHUM> on message]
```

and takes the message off the plate.

# Design

The Slack adapter handles `reaction_removed` for the emoji in `actionable_reactions`, the same set that gates `reaction_added`. A removal reaches the agent as `EVENT_KIND_REACTION`, with the content header changed from `added` to `removed`. The agent tells them apart by that header, as it already parses the added one, so the proto does not change.

A removal does not fetch the reacted message. The agent keys on `channel_id` and `message_id` (the reacted message's ts), and the removal needs no reply, so `ConversationId` and `ThreadId` use the message ts and `ThreadRootId` stays empty. A failed dispatch is logged, not posted to the channel. Removals count under `messaging_slack_events_total{event_type="reaction_removed"}`.

# Migration

Subscribe the Slack app to the `reaction_removed` event. Without it Slack never sends removals, and the adapter behaves as before.
