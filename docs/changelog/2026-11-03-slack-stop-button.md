# Summary

Slack shows a Stop button on an agent's loading state only when the app subscribes to `agent_session_stopped`. The adapter ignored that event, so a user in Slack had no way to stop an agent run. Web chat already had a stop action.

A press of Stop in Slack now stops the run the same way web chat's stop does: the agent gets a `StreamControl` STOP, and the rest of the stopped turn never posts.

Slack also shows Stop only for a session that the app set to `processing` with `agents.sessions.setStatus`. The adapter set its loading state only with the older `assistant.threads.setStatus`, which never shows Stop.

# Design

`handleInnerEvent` routes `slackevents.AgentSessionStoppedEvent` to `handleAgentSessionStopped`, which does three things, in this order:

1. It marks the turn stopped and drops its buffered reply. `HandleAgentResponse` then drops the turn's status updates, content, and error. A START opens the next turn. An END or an error closes the stopped turn, so an agent that ignores the stop does not post into the thread after it.
2. It forwards `PlatformFeedback{stream_control: STOP}` with the session's conversation ID and the Slack user who pressed Stop. adapter-core in TypeScript and Python already aborts the in-flight turn for that conversation, as it does for a web chat stop. The STOP goes before any Slack call, so Slack latency does not delay the abort.
3. It calls `agents.sessions.setStatus` with `active`. Slack leaves the session in `processing`, with the Stop button showing, until the app moves it.

## Session status

The adapter keeps the session in step with the turn:

| When | Session status |
|------|----------------|
| A mention reaches the adapter | `processing` |
| The reply posts, or an END arrives with nothing to post | `active` |
| The agent returns an error | `active` |
| The mention fails to reach the agent | `active` |
| A user presses Stop | `active` |

Without the `active` steps, Slack keeps the Stop button up after a finished reply until the one-hour processing timeout. The adapter records each session it opens and sends `active` only for those, so a reply in any other thread, or from an app that is not an agent, costs no extra Slack call. A press of Stop always sends `active`, because the event proves a session exists. The adapter still sets `assistant.threads.setStatus` too, because only that call carries status text such as "Running create_file". The adapter sets no session for a DM, the same as it sets no loading state there. An agent that sets its own session in a DM can still be stopped. An app that is not an agent rejects `agents.sessions.setStatus`, and the adapter logs that at debug level.

## The stop gate

A status update or reply can pass the gate just before a stop, then wait on the rate limiter or the thread lookup. The adapter checks the gate again just before the Slack call, so that output is dropped too.

The protocol lets an agent send a turn's status updates before its START. So when the user's next message reaches the agent, the gate lets status updates through again. Content stays dropped until the next START, the same as the web adapter's gate.

## Conversation ID

The conversation ID is `<channel>-<thread_ts>`, as for every threaded Slack conversation. A top-level DM is one conversation keyed by the channel alone. When the stopped thread has no turn in flight and the DM does, the stop goes to the DM conversation. Then an agent that set its own session status on a top-level DM message can still be stopped.

## Access and limits

Anyone who can see the session can press Stop, the same as the feedback buttons. The stop is not checked against the agent's authz grants.

The stopped-turn set and the open-session set each hold at most 1,000 conversations. An agent that honors the stop sends nothing more for the turn, so its entry stays until the next START.

## slack-go upgrade

slack-go moves to v0.33.0 for the event type and `SetAgentSessionStatusContext`. Every slack-go release with agent sessions needs Go 1.26, so the module, CI, and the build image move from Go 1.25 to Go 1.26, the version the astro monorepo builds with.

# Migration

To show the Stop button, turn on the agent experience on the Slack app's **Agents** page, which adds the `assistant:write` scope. An app on the outdated experience gets an **Update Now** prompt there, and it shows no Stop button until it is updated. Then subscribe to the `agent_session_stopped` bot event and reinstall the app. An app without the subscription keeps the plain loading indicator, and the rest of its behavior is unchanged.
