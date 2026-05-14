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


def test_platform_context_trigger_and_bot_user_id():
    """trigger + bot_user_id let agents distinguish observed traffic from
    direct invocations and detect bot mentions on any path."""
    pc = message_pb2.PlatformContext(
        message_id="ts.1",
        channel_id="C123",
        thread_id="ts.1",
        trigger=message_pb2.PlatformContext.TRIGGER_OBSERVED,
        bot_user_id="UBOT",
    )
    serialized = pc.SerializeToString()
    restored = message_pb2.PlatformContext()
    restored.ParseFromString(serialized)
    assert restored.trigger == message_pb2.PlatformContext.TRIGGER_OBSERVED
    assert restored.bot_user_id == "UBOT"


def test_platform_context_trigger_default_is_unspecified():
    """Existing code that does not set the field reads back as UNSPECIFIED;
    agents should treat that as TRIGGER_DIRECT (legacy default)."""
    pc = message_pb2.PlatformContext(message_id="ts.1", channel_id="C1")
    assert pc.trigger == message_pb2.PlatformContext.TRIGGER_UNSPECIFIED
    assert pc.bot_user_id == ""
