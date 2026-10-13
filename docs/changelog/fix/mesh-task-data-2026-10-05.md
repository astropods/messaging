# Summary

A room task the overseer sends over the agent mesh carries more than its title: a `data` part with the room task ID and its input documents, and metadata naming who asked. The Mesh adapter passed only text parts to the agent, so the agent saw the title and nothing else. It now passes the rest.

# Design

The adapter keeps the agent message's content as the task's text, and adds two platform data entries:

| Entry | Holds |
|---|---|
| `mesh_data` | A JSON array of the task's `data` parts, unchanged |
| `mesh_metadata` | The envelope's `metadata`, unchanged |

The SDKs read them with one helper, so an agent never parses either entry itself:

```ts
const task = meshTask(message);
// { roomTaskId: 'rt_1', inputs: [{ id, name, contentType }], onBehalfOf: { kind: 'user', id } }
```

```python
task = mesh_task(message)
```

Both return null (`None`) for a message that did not come from the agent mesh. The room client reads an input with its grant: `documentLink(id)` / `document_link(id)` returns a short-lived download link, and `readDocument(id)` / `read_document(id)` fetches the bytes.

# Migration

None. Agents that ignore the new entries behave as before.
