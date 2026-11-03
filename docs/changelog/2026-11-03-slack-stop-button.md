# Summary

Slack shows a Stop button on an agent's loading state only when the app subscribes to `agent_session_stopped`. The adapter ignored that event, so a user in Slack had no way to stop an agent run. Web chat already had a stop action.

A press of Stop in Slack now stops the run the same way web chat's stop does: the agent gets a `StreamControl` STOP, and the rest of the stopped turn never posts.

# Design

`handleInnerEvent` routes `slackevents.AgentSessionStoppedEvent` to `handleAgentSessionStopped`, which does three things:

1. It marks the turn stopped and drops its buffered reply. `HandleAgentResponse` then drops the turn's status updates, content, and error. A START opens the next turn. An END or an error closes the stopped turn, so an agent that ignores the stop does not post into the thread after it.
2. It calls `agents.sessions.setStatus` with `active`. Slack leaves the session in `processing`, with the Stop button showing, until the app moves it.
3. It forwards `PlatformFeedback{stream_control: STOP}` with the session's conversation ID and the Slack user who pressed Stop. adapter-core in TypeScript and Python already aborts the in-flight turn for that conversation, as it does for a web chat stop.

The conversation ID is `<channel>-<thread_ts>`, as for every threaded Slack conversation. A top-level DM is one conversation keyed by the channel alone. When the stopped thread has no turn in flight and the DM does, the stop goes to the DM conversation. Then an agent that set its own session status on a top-level DM message can still be stopped.

Anyone who can see the session can press Stop, the same as the feedback buttons. The stop is not checked against the agent's authz grants.

The stopped-turn set holds at most 1,000 conversations. An agent that honors the stop sends nothing more for the turn, so its entry stays until the next START.

slack-go moves to v0.33.0 for the event type and `SetAgentSessionStatusContext`. Every slack-go release with agent sessions needs Go 1.26, so the module, CI, and the build image move from Go 1.25 to Go 1.26, the version the astro monorepo builds with.

# Migration

To show the Stop button, declare the Slack app as an agent in its app settings, which adds the `assistant:write` scope, and subscribe to the `agent_session_stopped` bot event. Then reinstall the app. Apps without the subscription keep the plain loading indicator and behave as before.
