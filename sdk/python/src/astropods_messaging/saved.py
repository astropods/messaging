"""Conversation-id derivation for SaveConversation.

The sidecar derives the same id from the same inputs and the two never exchange
it, so this must stay byte-identical to savedConversationNamespace in the Go
store. A mismatch orphans every copy an agent has saved.
"""

import uuid

_SAVED_CONVERSATION_NAMESPACE = uuid.UUID("8f2b0a54-6d31-4c9e-9a77-1f0c5b83e2d1")


def derive_saved_conversation_id(user_id: str, idempotency_key: str) -> str:
    """Return the conversation id a save for this user and key lands on.

    Deriving rather than allocating lets an agent link to the copy without a
    round trip, and makes a repeat save resolve to the same conversation.
    """
    return str(
        uuid.uuid5(_SAVED_CONVERSATION_NAMESPACE, f"{user_id}\0{idempotency_key}")
    )
