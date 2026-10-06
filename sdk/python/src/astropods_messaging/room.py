"""Room API calls for the agent mesh task or message a conversation came from."""

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, List, Optional, Union

from .astro.messaging.v1.service_pb2 import RoomGrantRequest


class RoomError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status


@dataclass
class RoomGrant:
    room_id: str
    grant: str
    expires_at: datetime
    api_url: str


def get_room_grant(stub: Any, conversation_id: str) -> Optional[RoomGrant]:
    response = stub.GetRoomGrant(RoomGrantRequest(conversation_id=conversation_id))
    if not response.found:
        return None
    return RoomGrant(
        room_id=response.room_id,
        grant=response.grant,
        expires_at=datetime.fromtimestamp(response.expires_at.seconds, tz=timezone.utc),
        api_url=response.api_url,
    )


class RoomClient:
    """Asks the messaging sidecar for the conversation's current grant before
    each call, so a long task keeps working after its first grant expires."""

    def __init__(self, stub: Any, conversation_id: str, timeout: float = 30.0):
        self._stub = stub
        self.conversation_id = conversation_id
        self._timeout = timeout

    def grant(self) -> RoomGrant:
        grant = get_room_grant(self._stub, self.conversation_id)
        if grant is None:
            raise RoomError(403, f"conversation {self.conversation_id} has no room grant: it is not a room task, or the task ended")
        return grant

    def get(self) -> dict:
        return self._request("GET", "")

    def upload_document(self, name: str, body: Union[bytes, str], content_type: str = "application/octet-stream") -> dict:
        data = body.encode() if isinstance(body, str) else body
        return self._request("POST", "/artifacts?name=" + urllib.parse.quote(name), data, content_type)

    def list_tasks(self, scope: str = "assigned") -> dict:
        return self._request("GET", "/tasks?scope=" + urllib.parse.quote(scope))

    def _request(self, method: str, path: str, body: Optional[bytes] = None, content_type: Optional[str] = None) -> dict:
        g = self.grant()
        url = f"{g.api_url}/api/v1/rooms/{urllib.parse.quote(g.room_id)}{path}"
        headers = {"Authorization": f"Bearer {g.grant}"}
        if content_type:
            headers["Content-Type"] = content_type
        req = urllib.request.Request(url, data=body, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=self._timeout) as res:
                text = res.read().decode()
        except urllib.error.HTTPError as err:
            text = err.read().decode()
            try:
                message = json.loads(text).get("error", text)
            except ValueError:
                message = text
            raise RoomError(err.code, message) from None
        return json.loads(text) if text else {}


@dataclass
class RoomTaskInput:
    id: str
    name: str
    content_type: str


@dataclass
class MeshTask:
    room_task_id: Optional[str]
    inputs: List[RoomTaskInput]
    on_behalf_of: Optional[dict]


def mesh_task(message: Any) -> Optional[MeshTask]:
    """The room task details the overseer sends with a mesh task, or None for a
    message that did not come from the agent mesh."""
    if getattr(message, "platform", "") != "mesh":
        return None
    data = dict(message.platform_context.platform_data)
    task = MeshTask(room_task_id=None, inputs=[], on_behalf_of=None)
    try:
        for part in json.loads(data.get("mesh_data", "[]")):
            task.room_task_id = part.get("room_task_id") or task.room_task_id
            for item in part.get("inputs", []):
                task.inputs.append(RoomTaskInput(id=item.get("id", ""), name=item.get("name", ""), content_type=item.get("content_type", "")))
        metadata = json.loads(data.get("mesh_metadata", "{}"))
        task.on_behalf_of = metadata.get("on_behalf_of")
        task.room_task_id = task.room_task_id or metadata.get("room_task_id")
    except (ValueError, AttributeError):
        pass
    return task
