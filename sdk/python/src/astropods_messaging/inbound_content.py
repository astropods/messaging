"""Slack inbound content enrichment for agent-facing messages."""

from __future__ import annotations

import json
from typing import Any, Mapping, Optional

SLACK_META_PREFIX = "[slack_meta] "


def format_slack_meta_line(
    channel_id: str,
    message_ts: str,
    thread_root_id: str = "",
    permalink: str = "",
) -> str:
    payload: dict[str, str] = {
        "channel_id": channel_id,
        "message_ts": message_ts,
    }
    if thread_root_id and thread_root_id != message_ts:
        payload["thread_ts"] = thread_root_id
    if permalink:
        payload["permalink"] = permalink
    return SLACK_META_PREFIX + json.dumps(payload, separators=(",", ":"))


def resolve_slack_permalink(team_url: str, channel_id: str, message_ts: str) -> str:
    base = team_url.rstrip("/")
    ts = message_ts.replace(".", "")
    return f"{base}/archives/{channel_id}/p{ts}"


def has_slack_meta(content: str) -> bool:
    return content.startswith(SLACK_META_PREFIX)


def _permalink_from_platform_data(platform_data: Optional[Mapping[str, str]]) -> str:
    if not platform_data:
        return ""
    for key in ("permalink", "slack_permalink"):
        if platform_data.get(key):
            return platform_data[key]
    return ""


def enrich_slack_inbound_message(message: Any) -> Any:
    """Mutate a protobuf Message in place when platform is slack."""
    if getattr(message, "platform", "") != "slack":
        return message
    ctx = getattr(message, "platform_context", None)
    if ctx is None:
        return message
    content = getattr(message, "content", "")
    if has_slack_meta(content):
        return message

    channel_id = getattr(ctx, "channel_id", "")
    message_id = getattr(ctx, "message_id", "")
    thread_root_id = getattr(ctx, "thread_root_id", "") or ""

    platform_data = dict(getattr(ctx, "platform_data", {}) or {})
    permalink = _permalink_from_platform_data(platform_data)
    if not permalink:
        team_url = platform_data.get("team_url") or platform_data.get("workspace_url") or ""
        if team_url and channel_id and message_id:
            permalink = resolve_slack_permalink(team_url, channel_id, message_id)

    meta_line = format_slack_meta_line(channel_id, message_id, thread_root_id, permalink)
    message.content = f"{meta_line}\n{content}" if content else meta_line
    return message
