# Standard Files API error envelopes

The Files API now returns JSON errors with stable `error` codes and actionable
`details` for authentication, validation, upload, download, list, delete,
not-found, oversized-file, and insufficient-storage failures. Other messaging
HTTP handlers retain their existing error format.

Malformed create requests are rejected with `invalid_file_request` instead of
being decoded as an empty request. Focused tests pin the response status,
content type, code, and details for common failures.
