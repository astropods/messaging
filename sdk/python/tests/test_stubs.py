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
