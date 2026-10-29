package slack

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/astropods/messaging/internal/store/files"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
	"github.com/slack-go/slack"
)

// maxInlineImageBytes caps the raw size of a Slack image we download and inline
// as a base64 data URI on the outbound Message. Base64 inflates bytes by ~33%,
// so this stays under the gRPC 4 MiB default frame. Larger images are skipped
// (with a log) rather than risk a dropped message.
const maxInlineImageBytes = 2 * 1024 * 1024

// Agent-visible reasons a Slack file did not arrive.
const (
	skipImageTooLarge = "image is larger than 2 MiB"
	skipNoLink        = "Slack gave no download link"
	skipDownload      = "download failed"
	skipRefused       = "Slack refused the download"
	skipStorageFull   = "agent storage is full"
	skipNotSaved      = "file could not be saved"
	skipNoStore       = "file uploads are not enabled for this agent"
	skipExternal      = "linked file, not an upload"
)

func skipFileTooLarge(limit int64) string {
	return fmt.Sprintf("file is larger than %d MiB", limit>>20)
}

type stagedFiles struct {
	// The event's own files; imageAttachments inlines the small images.
	message []slack.File
	// Earlier files in the thread, images included.
	thread []slack.File
}

type fileSkip struct{ name, reason string }

func withSkipNotes(content string, skips []fileSkip) string {
	if len(skips) == 0 {
		return content
	}
	lines := make([]string, 0, len(skips))
	for _, s := range skips {
		lines = append(lines, fmt.Sprintf("[slack_file_skipped] %s: %s", s.name, s.reason))
	}
	if content == "" {
		return strings.Join(lines, "\n")
	}
	return content + "\n\n" + strings.Join(lines, "\n")
}

// imageAttachments downloads the image files on a Slack event and returns them
// as inline IMAGE attachments: the bytes ride the wire as a base64 data URI in
// Url so the agent can pass them straight to the model with no Slack token of
// its own. Non-image files and oversized images are left to fileAttachments;
// download failures are skipped so a single bad file never blocks the message.
// The adapter's Slack client carries the bot token that authenticates the
// url_private download.
func (a *SlackAdapter) imageAttachments(ctx context.Context, files []slack.File) ([]*pb.Attachment, []fileSkip) {
	if len(files) == 0 || a.client == nil {
		return nil, nil
	}
	var out []*pb.Attachment
	var skips []fileSkip
	for i := range files {
		f := &files[i]
		if !strings.HasPrefix(f.Mimetype, "image/") {
			continue
		}
		if f.Size > maxInlineImageBytes {
			continue
		}
		url := downloadURL(f)
		if url == "" {
			skips = append(skips, fileSkip{f.Name, skipNoLink})
			continue
		}
		var buf bytes.Buffer
		if err := a.client.GetFileContext(ctx, url, &buf); err != nil {
			slog.Error("[Slack] Failed to download image attachment", "file", f.Name, "err", err)
			skips = append(skips, fileSkip{f.Name, skipDownload})
			continue
		}
		// An unauthorized url_private download returns HTTP 200 with a Slack
		// sign-in HTML page rather than the bytes. Sniff the payload and skip
		// anything that isn't actually an image so we never hand the model a
		// data URI whose bytes are HTML (which the vision API rejects). The
		// usual cause is the app missing the `files:read` scope.
		sniffed := http.DetectContentType(buf.Bytes())
		if !strings.HasPrefix(sniffed, "image/") {
			slog.Error("[Slack] Downloaded attachment is not an image; skipping (check the Slack app's files:read scope)",
				"file", f.Name, "declared_mimetype", f.Mimetype, "detected", sniffed, "bytes", buf.Len())
			skips = append(skips, fileSkip{f.Name, skipRefused})
			continue
		}
		// Label the data URI with the sniffed type, not Slack's declared
		// Mimetype: Slack mislabels some images (a PNG as image/jpeg) and the
		// model rejects a data URI whose type disagrees with the bytes. Strip
		// any parameters DetectContentType may append.
		mediaType := sniffed
		if j := strings.IndexByte(mediaType, ';'); j >= 0 {
			mediaType = mediaType[:j]
		}
		dataURI := fmt.Sprintf("data:%s;base64,%s", mediaType, base64.StdEncoding.EncodeToString(buf.Bytes()))
		out = append(out, &pb.Attachment{
			Type:      pb.Attachment_IMAGE,
			Url:       dataURI,
			Filename:  f.Name,
			MimeType:  mediaType,
			SizeBytes: int64(buf.Len()),
			Width:     int32(f.OriginalW), //nolint:gosec // G115: image pixel dimensions never overflow int32
			Height:    int32(f.OriginalH), //nolint:gosec // G115: image pixel dimensions never overflow int32
		})
		slog.Debug("[Slack] Inlined image attachment",
			"file", f.Name, "declared_mimetype", f.Mimetype, "sent_mimetype", mediaType, "bytes", buf.Len())
	}
	return out, skips
}

// Staged files use the web upload contract (storage_key), so the agent reads them by path.
func (a *SlackAdapter) fileAttachments(ctx context.Context, slackFiles []slack.File, owner string, stageImages bool) ([]*pb.Attachment, []fileSkip) {
	if len(slackFiles) == 0 || a.client == nil {
		return nil, nil
	}
	var out []*pb.Attachment
	var skips []fileSkip
	for i := range slackFiles {
		f := &slackFiles[i]
		if !stageImages && strings.HasPrefix(f.Mimetype, "image/") && f.Size <= maxInlineImageBytes {
			continue
		}
		if f.IsExternal {
			skips = append(skips, fileSkip{f.Name, skipExternal})
			continue
		}
		if a.fileStore == nil {
			slog.Warn("[Slack] Skipping file attachment: files store disabled (FILES_DIR unset)", "file", f.Name)
			skips = append(skips, fileSkip{f.Name, skipNoStore})
			continue
		}
		if att, reason := a.stageFile(ctx, f, owner); att != nil {
			out = append(out, att)
		} else {
			skips = append(skips, fileSkip{f.Name, reason})
		}
	}
	return out, skips
}

// For the reaction path, which inlines images but stages no files.
func oversizedImageSkips(files []slack.File) []fileSkip {
	var skips []fileSkip
	for i := range files {
		if strings.HasPrefix(files[i].Mimetype, "image/") && files[i].Size > maxInlineImageBytes {
			skips = append(skips, fileSkip{files[i].Name, skipImageTooLarge})
		}
	}
	return skips
}

// Keyed by Slack file ID, so a file already staged is reused without a download.
func (a *SlackAdapter) stageFile(ctx context.Context, f *slack.File, owner string) (*pb.Attachment, string) {
	key := uuid.NewString()
	if f.ID != "" {
		key = "slack-" + f.ID
		if meta, err := a.fileStore.ReadMeta(ctx, key); err == nil && meta.Ready() {
			return fileAttachment(meta), ""
		}
	}
	limit := files.EffectiveLimit(a.fileLimit)
	if files.Exceeds(limit, int64(f.Size)) {
		slog.Warn("[Slack] Skipping oversized file attachment",
			"file", f.Name, "mimetype", f.Mimetype, "size", f.Size, "max", limit)
		return nil, skipFileTooLarge(limit)
	}
	url := downloadURL(f)
	if url == "" {
		return nil, skipNoLink
	}
	if usage, err := a.fileStore.Usage(ctx); err == nil && usage.TotalBytes > 0 &&
		uint64(f.Size)+files.StorageReserveBytes > usage.AvailableBytes { //nolint:gosec // G115: Slack file sizes are non-negative
		slog.Warn("[Slack] Skipping file attachment: storage full", "file", f.Name, "size", f.Size)
		return nil, skipStorageFull
	}

	pr, pw := io.Pipe()
	dlErr := make(chan error, 1)
	go func() {
		err := a.client.GetFileContext(ctx, url, pw)
		_ = pw.CloseWithError(err)
		dlErr <- err
	}()
	stop := func() error {
		_ = pr.Close()
		return <-dlErr
	}

	// Slack's sign-in page sniffs as HTML; text is exempt, since markdown may open with <!--.
	head := make([]byte, 512)
	hn, err := io.ReadFull(pr, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		_ = stop()
		slog.Error("[Slack] Failed to download file attachment", "file", f.Name, "err", err)
		return nil, skipDownload
	}
	head = head[:hn]
	if strings.HasPrefix(http.DetectContentType(head), "text/html") && !strings.HasPrefix(f.Mimetype, "text/") {
		_ = stop()
		slog.Error("[Slack] Downloaded file is an HTML page; skipping (check the Slack app's files:read scope)",
			"file", f.Name, "declared_mimetype", f.Mimetype)
		return nil, skipRefused
	}

	body := io.MultiReader(bytes.NewReader(head), pr)
	if limit != files.NoFileLimit {
		body = io.LimitReader(body, limit+1)
	}
	n, err := a.fileStore.WriteBlob(ctx, key, body)
	derr := stop()
	switch {
	case err == nil && files.Exceeds(limit, n):
		_ = a.fileStore.Delete(ctx, key)
		slog.Warn("[Slack] Skipping oversized file attachment", "file", f.Name, "bytes", n, "max", limit)
		return nil, skipFileTooLarge(limit)
	case derr != nil:
		_ = a.fileStore.Delete(ctx, key)
		slog.Error("[Slack] Failed to download file attachment", "file", f.Name, "err", derr)
		return nil, skipDownload
	case err != nil:
		slog.Error("[Slack] Failed to store file attachment", "file", f.Name, "err", err)
		return nil, skipNotSaved
	}
	meta := files.FileMeta{
		Key:         key,
		Name:        f.Name,
		Size:        n,
		ContentType: f.Mimetype,
		UpdatedAt:   time.Now().UTC(),
		UploadedBy:  owner,
		Status:      files.StatusReady,
	}
	if err := a.fileStore.WriteMeta(ctx, meta); err != nil {
		slog.Error("[Slack] Failed to store file attachment metadata", "file", f.Name, "err", err)
		_ = a.fileStore.Delete(ctx, key)
		return nil, skipNotSaved
	}
	return fileAttachment(meta), ""
}

func fileAttachment(meta files.FileMeta) *pb.Attachment {
	return &pb.Attachment{
		Type:       pb.Attachment_FILE,
		Filename:   meta.Name,
		MimeType:   meta.ContentType,
		SizeBytes:  meta.Size,
		StorageKey: meta.Key,
	}
}

func downloadURL(f *slack.File) string {
	if f.URLPrivateDownload != "" {
		return f.URLPrivateDownload
	}
	return f.URLPrivate
}

// Only an unowned (agent-written) file is posted, so a user's upload never leaves its owner.
func (a *SlackAdapter) postResponseFile(ctx context.Context, channelID, threadTS string, f *pb.FileAttachment) error {
	if a.fileStore == nil {
		return fmt.Errorf("files store disabled (FILES_DIR unset)")
	}
	key := f.GetFilename()
	meta, err := a.fileStore.ReadMeta(ctx, key)
	if err != nil {
		return fmt.Errorf("read %q: %w", key, err)
	}
	if !meta.Ready() || meta.Size <= 0 {
		return fmt.Errorf("file %q is not ready or empty", key)
	}
	if !meta.Unowned() {
		return fmt.Errorf("file %q is owned by a user", key)
	}
	if files.Exceeds(a.fileLimit, meta.Size) {
		return fmt.Errorf("file %q is %s", key, skipFileTooLarge(files.EffectiveLimit(a.fileLimit)))
	}
	rc, err := a.fileStore.OpenBlob(ctx, key)
	if err != nil {
		return fmt.Errorf("open %q: %w", key, err)
	}
	defer func() { _ = rc.Close() }()

	if err := a.rateLimiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limit wait failed: %w", err)
	}
	_, err = a.client.UploadFileContext(ctx, slack.UploadFileParameters{
		Reader:          rc,
		FileSize:        int(meta.Size),
		Filename:        meta.Name,
		Title:           meta.Name,
		Channel:         channelID,
		ThreadTimestamp: threadTS,
	})
	if err != nil {
		return fmt.Errorf("upload %q: %w (check the Slack app's files:write scope)", key, err)
	}
	return nil
}
