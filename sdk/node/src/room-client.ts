import type { MessagingClient, RoomGrant } from './messaging-client';

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
