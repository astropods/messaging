from astropods_messaging.astro.messaging.v1 import message_pb2, service_pb2_grpc


def test_message_roundtrip():
    msg = message_pb2.Message(
        id="test-id",
        platform="test",
        content="hello",
        conversation_id="conv-1",
    )
    serialized = msg.SerializeToString()
    restored = message_pb2.Message()
    restored.ParseFromString(serialized)
    assert restored.id == "test-id"
    assert restored.content == "hello"
    assert restored.conversation_id == "conv-1"


def test_user_roundtrip():
    user = message_pb2.User(id="user-1", username="testuser", email="test@example.com")
    serialized = user.SerializeToString()
    restored = message_pb2.User()
    restored.ParseFromString(serialized)
    assert restored.id == "user-1"
    assert restored.username == "testuser"


def test_service_servicer_has_expected_methods():
    assert hasattr(service_pb2_grpc.AgentMessagingServicer, "ProcessMessage")
    assert hasattr(service_pb2_grpc.AgentMessagingServicer, "ProcessConversation")
    assert hasattr(service_pb2_grpc.AgentMessagingServicer, "HealthCheck")


def test_platform_context_event_kind_and_bot_user_id():
    """event_kind + bot_user_id let agents distinguish how a message arrived
    (DM, app_mention, observed, etc.) and detect bot mentions on any path."""
    pc = message_pb2.PlatformContext(
        message_id="ts.1",
        channel_id="C123",
        thread_id="ts.1",
        thread_root_id="ts.0",
        event_kind=message_pb2.PlatformContext.EVENT_KIND_THREAD_REPLY,
        bot_user_id="UBOT",
    )
    serialized = pc.SerializeToString()
    restored = message_pb2.PlatformContext()
    restored.ParseFromString(serialized)
    assert restored.event_kind == message_pb2.PlatformContext.EVENT_KIND_THREAD_REPLY
    assert restored.thread_root_id == "ts.0"
    assert restored.bot_user_id == "UBOT"


def test_platform_context_event_kind_default_is_unspecified():
    """Existing code that does not set event_kind reads back as UNSPECIFIED."""
    pc = message_pb2.PlatformContext(message_id="ts.1", channel_id="C1")
    assert pc.event_kind == message_pb2.PlatformContext.EVENT_KIND_UNSPECIFIED
    assert pc.thread_root_id == ""
    assert pc.bot_user_id == ""
