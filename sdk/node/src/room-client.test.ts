import { describe, expect, it } from 'bun:test';
import { meshTask } from './room-client';
import type { Message } from './messaging-client';

function message(platform: string, platformData: Record<string, string>): Message {
  return { id: 'm1', platform, content: 'Draft the Q3 summary', platformContext: { messageId: 'm1', channelId: 'c', platformData } } as Message;
}

describe('meshTask', () => {
  it('reads the room task, its inputs and who asked from a mesh task', () => {
    const task = meshTask(
      message('mesh', {
        mesh_data: '[{"room_task_id":"rt_1","inputs":[{"id":"a1","name":"q3.pdf","content_type":"application/pdf"}]}]',
        mesh_metadata: '{"on_behalf_of":{"kind":"user","id":"user-1"},"room_task_id":"rt_1"}',
      }),
    );
    expect(task).toEqual({
      roomTaskId: 'rt_1',
      inputs: [{ id: 'a1', name: 'q3.pdf', contentType: 'application/pdf' }],
      onBehalfOf: { kind: 'user', id: 'user-1' },
    });
  });

  it('returns null for a message that did not come from the agent mesh', () => {
    expect(meshTask(message('slack', { mesh_data: '[]' }))).toBeNull();
  });

  it('returns no inputs for a mesh message without a data part', () => {
    expect(meshTask(message('mesh', {}))).toEqual({ inputs: [] });
  });
});
