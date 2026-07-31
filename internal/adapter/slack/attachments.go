package slack

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/slack-go/slack"
)

// maxInlineImageBytes caps the raw size of a Slack image we download and inline
// as a base64 data URI on the outbound Message. Base64 inflates bytes by ~33%,
// so this stays under the gRPC 4 MiB default frame. Larger images are skipped
// (with a log) rather than risk a dropped message.
const maxInlineImageBytes = 2 * 1024 * 1024

// imageAttachments downloads the image files on a Slack event and returns them
// as inline IMAGE attachments: the bytes ride the wire as a base64 data URI in
// Url so the agent can pass them straight to the model with no Slack token of
// its own. Non-image files, oversized images, and download failures are skipped
// so a single bad file never blocks the message. The adapter's Slack client
// carries the bot token that authenticates the url_private download.
func (a *SlackAdapter) imageAttachments(ctx context.Context, files []slack.File) []*pb.Attachment {
	if len(files) == 0 || a.client == nil {
		return nil
	}
	var out []*pb.Attachment
	for i := range files {
		f := &files[i]
		if !strings.HasPrefix(f.Mimetype, "image/") {
			continue
		}
		if f.Size > maxInlineImageBytes {
			slog.Warn("[Slack] Skipping oversized image attachment",
				"file", f.Name, "mimetype", f.Mimetype, "size", f.Size, "max", maxInlineImageBytes)
			continue
		}
		url := f.URLPrivateDownload
		if url == "" {
			url = f.URLPrivate
		}
		if url == "" {
			continue
		}
		var buf bytes.Buffer
		if err := a.client.GetFileContext(ctx, url, &buf); err != nil {
			slog.Error("[Slack] Failed to download image attachment", "file", f.Name, "err", err)
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
	return out
}
