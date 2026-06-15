## Summary

Slack assistant replies no longer collapse each long response section behind repeated "Show more" links.

## Design

Astro-generated Slack `section` blocks now set Slack's `expand` flag when posting assistant responses. Slack can otherwise collapse long section text even when the message is valid Block Kit, which made one answer render as several partial previews with separate "Show more" controls.

The existing chunking remains in place for Slack's section text limits; the change only updates the render hint sent with each generated content section.

## Migration

No agent changes are required. Deployed Slack agents pick up the behavior when the messaging service is updated.
