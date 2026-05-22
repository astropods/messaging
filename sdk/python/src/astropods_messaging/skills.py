"""Convenience helpers for constructing skill ConversationRequests.

This SDK is low-level — these helpers just package a Skill / name into the
right oneof on a ConversationRequest so callers don't have to remember the
field layout. Send the result through your gRPC stream the same way you'd
send a message.
"""

from .astro.messaging.v1.service_pb2 import ConversationRequest
from .astro.messaging.v1.skill_pb2 import AddSkill, RemoveSkill, Skill


def add_skill(skill: Skill) -> ConversationRequest:
    """Build a ConversationRequest that registers a slash-invocable skill.

    Re-sending with the same Skill.name replaces the prior entry server-side.
    """
    return ConversationRequest(add_skill=AddSkill(skill=skill))


def remove_skill(name: str) -> ConversationRequest:
    """Build a ConversationRequest that deregisters a skill by name.

    Unknown names are a no-op on the server.
    """
    return ConversationRequest(remove_skill=RemoveSkill(name=name))
