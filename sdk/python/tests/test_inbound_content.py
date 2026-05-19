from astropods_messaging import (
    enrich_slack_inbound_message,
    format_slack_meta_line,
    has_slack_meta,
    resolve_slack_permalink,
)
from astropods_messaging.astro.messaging.v1.message_pb2 import Message, PlatformContext


def test_format_slack_meta_line_includes_thread():
    line = format_slack_meta_line("C1", "100.1", "100.0", "https://x/p")
    assert '"thread_ts":"100.0"' in line


def test_enrich_slack_inbound_message():
    msg = Message(platform="slack", conversation_id="C1-1.0", content="hello")
    msg.platform_context.CopyFrom(
        PlatformContext(channel_id="C1", message_id="100.1", thread_root_id="100.0")
    )
    enrich_slack_inbound_message(msg)
    assert has_slack_meta(msg.content)
    assert "hello" in msg.content
