import type { Message, MessagingClient, RoomGrant } from './messaging-client';

export interface RoomDocument {
  id: string;
  name: string;
  content_type: string;
  size_bytes: number;
  created_at: string;
}

export interface RoomTask {
  id: string;
  title: string;
  status: string;
  [field: string]: unknown;
}

export class RoomError extends Error {
  constructor(readonly status: number, message: string) {
    super(message);
  }
}

/**
 * Room API calls for the agent mesh task or message a conversation came from.
 * Each call asks the messaging sidecar for the conversation's current grant,
 * so a long task keeps working after its first grant expires.
 */
export class RoomClient {
  constructor(private readonly messaging: MessagingClient, readonly conversationId: string) {}

  async grant(): Promise<RoomGrant> {
    const grant = await this.messaging.getRoomGrant(this.conversationId);
    if (!grant) {
      throw new RoomError(403, `conversation ${this.conversationId} has no room grant: it is not a room task, or the task ended`);
    }
    return grant;
  }

  async get(): Promise<{ id: string; name: string; agents: unknown[]; humans: unknown[] }> {
    return this.request('GET', '');
  }

  async uploadDocument(name: string, body: Uint8Array | string, contentType = 'application/octet-stream'): Promise<RoomDocument> {
    return this.request('POST', `/artifacts?name=${encodeURIComponent(name)}`, body, contentType);
  }

  async documentLink(documentId: string): Promise<{ url: string; expires_at: string }> {
    return this.request('GET', `/artifacts/${encodeURIComponent(documentId)}/download`);
  }

  async readDocument(documentId: string): Promise<ArrayBuffer> {
    const { url } = await this.documentLink(documentId);
    const res = await fetch(url);
    if (!res.ok) {
      throw new RoomError(res.status, `download of document ${documentId} failed`);
    }
    return res.arrayBuffer();
  }

  async listTasks(scope: 'assigned' | 'created' = 'assigned'): Promise<{ tasks: RoomTask[] }> {
    return this.request('GET', `/tasks?scope=${scope}`);
  }

  private async request<T>(method: string, path: string, body?: Uint8Array | string, contentType?: string): Promise<T> {
    const g = await this.grant();
    const res = await fetch(`${g.apiUrl}/api/v1/rooms/${encodeURIComponent(g.roomId)}${path}`, {
      method,
      headers: { Authorization: `Bearer ${g.grant}`, ...(contentType ? { 'Content-Type': contentType } : {}) },
      body,
    });
    const text = await res.text();
    if (!res.ok) {
      let message = text;
      try {
        message = JSON.parse(text).error ?? text;
      } catch {}
      throw new RoomError(res.status, message);
    }
    return (text ? JSON.parse(text) : {}) as T;
  }
}

export interface RoomTaskInput {
  id: string;
  name: string;
  contentType: string;
}

export interface MeshTask {
  roomTaskId?: string;
  inputs: RoomTaskInput[];
  onBehalfOf?: { kind: 'user' | 'agent'; id: string };
}

/**
 * Reads the room task details the overseer sends with a mesh task: the room
 * task ID, its input documents, and who asked. Null for a message that did not
 * come from the agent mesh.
 */
export function meshTask(message: Message): MeshTask | null {
  const data = message.platformContext?.platformData;
  if (!data || message.platform !== 'mesh') {
    return null;
  }
  const task: MeshTask = { inputs: [] };
  try {
    for (const part of JSON.parse(data.mesh_data ?? '[]')) {
      if (part?.room_task_id) task.roomTaskId = part.room_task_id;
      for (const input of part?.inputs ?? []) {
        task.inputs.push({ id: input.id, name: input.name, contentType: input.content_type });
      }
    }
    const metadata = JSON.parse(data.mesh_metadata ?? '{}');
    if (metadata?.on_behalf_of?.kind && metadata.on_behalf_of.id) {
      task.onBehalfOf = { kind: metadata.on_behalf_of.kind, id: metadata.on_behalf_of.id };
    }
    task.roomTaskId ??= metadata?.room_task_id;
  } catch {
    return task;
  }
  return task;
}
