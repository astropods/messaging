package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/astropods/messaging/internal/store/files"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// chatAttachment is the canonical file-attachment shape shared across the web
// chat contract: the send request, the history read response, the SSE chunk
// event, and the SQLite persistence blob. It is a stable subset of the files
// API metadata keyed by the opaque files-API key.
type chatAttachment struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// marshalAttachments encodes attachments for persistence. Empty input yields the
// empty string (the column default) so "no attachments" costs no JSON.
func marshalAttachments(atts []chatAttachment) string {
	if len(atts) == 0 {
		return ""
	}
	b, err := json.Marshal(atts)
	if err != nil {
		slog.Error("[Web] marshal attachments failed", "err", err)
		return ""
	}
	return string(b)
}

// unmarshalAttachments decodes the persisted attachments blob. An empty or
// malformed blob yields nil (rendered as no attachments) rather than an error —
// one bad row must not fail a whole conversation read.
func unmarshalAttachments(raw string) []chatAttachment {
	if raw == "" {
		return nil
	}
	var atts []chatAttachment
	if err := json.Unmarshal([]byte(raw), &atts); err != nil {
		slog.Warn("[Web] unmarshal attachments failed", "err", err)
		return nil
	}
	return atts
}

// resolveAttachment turns a files-API key into a canonical attachment by reading
// the store's authoritative metadata, enforcing ownership: the file must be
// owned by `owner` (the requesting user). Returns false when the key is unknown
// OR owned by someone else — callers treat both as "not found" so a user can
// neither send a dangling key nor attach another user's file. fileStore may be
// nil (files disabled), which also yields false.
func resolveAttachment(ctx context.Context, fileStore files.FileStore, key, owner string) (chatAttachment, bool) {
	if fileStore == nil || key == "" {
		return chatAttachment{}, false
	}
	meta, err := fileStore.ReadMeta(ctx, key)
	if err != nil {
		return chatAttachment{}, false
	}
	if owner != "" && meta.UploadedBy != owner {
		// Owned by another user — deny (do not leak existence beyond "not found").
		return chatAttachment{}, false
	}
	if !meta.Ready() {
		// A reserved/uploading file has no committed bytes yet; a user can't attach
		// it (it would ride the message as a permanently-broken download chip).
		return chatAttachment{}, false
	}
	return chatAttachment{
		Key:         meta.Key,
		Name:        meta.Name,
		ContentType: meta.ContentType,
		Size:        meta.Size,
	}, true
}

// resolveResponseAttachments converts an agent's response file attachments into
// the canonical shape for the client, attributing each file to `owner` (the
// conversation's owner) so per-user access control applies: the agent writes a
// plain file (no uploader), so we stamp a metadata sidecar recording the owner.
// That keeps the reply's download chip working for that user while hiding the
// file from other members of the same account. Non-file attachments
// (image/card/link) are ignored for now.
func resolveResponseAttachments(ctx context.Context, fileStore files.FileStore, atts []*pb.ResponseAttachment, owner string) []chatAttachment {
	if len(atts) == 0 || fileStore == nil {
		return nil
	}
	if owner == "" {
		// Without an owner the agent's output can't be attributed, so it will not
		// be downloadable under per-user access control (the reply chip renders
		// but 404s). Log it so this is diagnosable rather than a silent dead chip.
		slog.Warn("[Web] agent response attachments have no owner; files will not be user-downloadable")
	}
	out := make([]chatAttachment, 0, len(atts))
	for _, ra := range atts {
		f := ra.GetFile()
		if f == nil || f.GetFilename() == "" {
			continue
		}
		key := f.GetFilename()
		meta, err := fileStore.ReadMeta(ctx, key)
		if err != nil {
			// The agent named a file that isn't in the store (never written, a bogus
			// name, or written after this END chunk). Skip it rather than persist an
			// optimistic chip: with no stored file we can't attribute it to the
			// owner, so the chip would 404 forever — the broken-chip case the files
			// review flagged.
			slog.Warn("[Web] agent referenced an unknown file; skipping attachment", "key", key)
			continue
		}
		if !meta.Ready() {
			// The agent referenced a reserved/uploading file that never committed;
			// don't surface a permanently-broken download chip.
			slog.Warn("[Web] agent referenced a not-ready file; skipping attachment", "key", key)
			continue
		}
		if owner != "" {
			// Attribute the agent's output to the conversation owner, but only if
			// the file is unowned (agent-written). AttributeOwner is compare-and-set:
			// a file already owned by a *different* user is never transferred into
			// this conversation, so a shared agent can't move user A's file to user
			// B by echoing its key. Already-owned-by-owner is a no-op success.
			attributed, aerr := fileStore.AttributeOwner(ctx, key, owner)
			switch {
			case aerr == nil:
				meta = attributed
			case errors.Is(aerr, files.ErrOwnedByOther):
				slog.Warn("[Web] agent returned a file owned by another user; omitting attachment", "key", key)
				continue
			default:
				// Transient attribution failure: render the chip from the file we
				// read, but it won't be downloadable until attribution succeeds.
				slog.Warn("[Web] attribute agent file failed", "err", aerr)
			}
		}
		out = append(out, chatAttachment{
			Key:         meta.Key,
			Name:        meta.Name,
			ContentType: meta.ContentType,
			Size:        meta.Size,
		})
	}
	return out
}

// toProtoAttachment builds the agent-facing attachment. storage_key is the
// opaque key the agent resolves against its own files dir (AGENT_FILES_DIR); the
// filesystem store has no direct download URL, so url is left empty (a future
// presign-capable store would populate it and the agent/client could prefer it).
func toProtoAttachment(att chatAttachment) *pb.Attachment {
	return &pb.Attachment{
		Type:       pb.Attachment_FILE,
		Filename:   att.Name,
		SizeBytes:  att.Size,
		MimeType:   att.ContentType,
		StorageKey: att.Key,
	}
}
