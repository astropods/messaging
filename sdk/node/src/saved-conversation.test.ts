import { describe, expect, it } from 'bun:test';
import { deriveSavedConversationId } from './messaging-client';

// The sidecar derives the same id from the same inputs and the two never
// exchange it. These vectors come from the Go implementation; if they drift,
// every agent link points at a conversation that does not exist.
describe('deriveSavedConversationId', () => {
  it('matches the Go uuid.NewSHA1 vectors', () => {
    expect(deriveSavedConversationId('user_1', 'slack:C1:111.0001')).toBe(
      '438a83d7-8c87-5a1c-91a8-c12a365e1799'
    );
    expect(
      deriveSavedConversationId(
        'user_01KKA2B82N7ZEG96320ZZ6S1NS',
        'slack:C0A4K8RSPU1:1787752050.896659'
      )
    ).toBe('560935f7-9097-5b2c-aada-0bc9467d7bc2');
    expect(deriveSavedConversationId('', '')).toBe(
      '837dad26-ccef-5008-97f1-026f3dc0f7f1'
    );
  });

  it('scopes the copy to one user so participants do not share a row', () => {
    expect(deriveSavedConversationId('user_1', 'k')).not.toBe(
      deriveSavedConversationId('user_2', 'k')
    );
  });
});
