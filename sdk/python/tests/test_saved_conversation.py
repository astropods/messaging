from astropods_messaging.astro.messaging.v1 import response_pb2
from astropods_messaging.saved import derive_saved_conversation_id

# The sidecar derives the same id from the same inputs and the two never
# exchange it. These vectors come from the Go implementation; if they drift,
# every agent link points at a conversation that does not exist.
GO_VECTORS = [
    (("user_1", "slack:C1:111.0001"), "438a83d7-8c87-5a1c-91a8-c12a365e1799"),
    (
        ("user_01KKA2B82N7ZEG96320ZZ6S1NS", "slack:C0A4K8RSPU1:1787752050.896659"),
        "560935f7-9097-5b2c-aada-0bc9467d7bc2",
    ),
    (("", ""), "837dad26-ccef-5008-97f1-026f3dc0f7f1"),
]


def test_derive_matches_go_vectors():
    for args, want in GO_VECTORS:
        assert derive_saved_conversation_id(*args) == want


def test_derive_scopes_the_copy_to_one_user():
    assert derive_saved_conversation_id("user_1", "k") != derive_saved_conversation_id(
        "user_2", "k"
    )


def test_save_conversation_roundtrip():
    resp = response_pb2.AgentResponse(
        save_conversation=response_pb2.SaveConversation(
            user_id="user_1",
            idempotency_key="slack:C1:111.0001",
            title="Thread",
            source_label="#eng",
            messages=[
                response_pb2.SavedMessage(role="user", author="Ada", content="hello"),
                response_pb2.SavedMessage(role="assistant", content="hi"),
            ],
        )
    )
    decoded = response_pb2.AgentResponse()
    decoded.ParseFromString(resp.SerializeToString())
    assert decoded.WhichOneof("payload") == "save_conversation"
    assert decoded.save_conversation.messages[0].author == "Ada"
