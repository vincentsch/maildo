package imap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"os"
	"strings"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
)

// Attachments of messages read over IMAP. An attachment's id is its MIME part
// number ("2", "1.3"), which stays the same for the life of the message.

func (c *accountClient) ListMessageAttachments(ctx context.Context, req mail.MessageAttachmentListRequest) (mail.MessageAttachmentList, error) {
	var out mail.MessageAttachmentList
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		msg, err := c.resolveMessage(ctx, session, mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: req.MessageID}, errs.ProviderOperationIMAPAttachmentsRead, true)
		if err != nil {
			return err
		}
		meta, err := mail.NewProviderMetadata(imapErrProvider(c.provider), mail.MetadataAttachmentList, mail.AttachmentProviderMetadata{Source: mail.MetadataSourceIMAP})
		if err != nil {
			return err
		}
		out = mail.MessageAttachmentList{
			MessageID:         msg.id,
			InternetMessageID: msg.internetMessageID,
			Attachments:       c.attachmentsFromStructure(msg.bodyStructure),
			ProviderMeta:      meta,
		}
		return nil
	})
	return out, err
}

func (c *accountClient) DownloadMessageAttachment(ctx context.Context, req mail.MessageAttachmentDownloadRequest) (mail.MessageAttachmentContent, error) {
	var out mail.MessageAttachmentContent
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		identity, err := parseMessageID(req.MessageID)
		if err != nil {
			return invalidIMAPMessageSelector(c, "id", req.MessageID)
		}
		partPath, err := parsePartID(req.AttachmentID)
		if err != nil {
			return attachmentNotFound(c, req.MessageID, req.AttachmentID)
		}
		msg, err := c.fetchByIdentity(ctx, session, identity, req.MessageID, errs.ProviderOperationIMAPAttachmentsRead, false)
		if err != nil {
			return err
		}
		parts, _ := attachmentParts(msg.bodyStructure)
		var part *imapBodyPart
		for _, candidate := range parts {
			if samePath(candidate.path, partPath) {
				part = candidate
			}
		}
		if part == nil {
			return attachmentNotFound(c, req.MessageID, req.AttachmentID)
		}
		if part.encodedSize > uint64(limits.IMAPMaxEncodedAttachmentBytes) {
			return errs.NewIMAPAttachmentTooLarge(c.account, imapErrProvider(c.provider), req.MessageID, req.AttachmentID, errs.IMAPAttachmentSizeEncoded, uint64(limits.IMAPMaxEncodedAttachmentBytes), part.encodedSize)
		}
		content, err := c.fetchAttachmentContent(ctx, session, identity.UID, partPath, part, req.MessageID, req.AttachmentID)
		if err != nil {
			return err
		}
		out = content
		return nil
	})
	return out, err
}

// attachmentsFromStructure lists attachment metadata. IMAP reports the
// encoded size of each part; the decoded size is estimated from it.
func (c *accountClient) attachmentsFromStructure(structure goimap.BodyStructure) []mail.Attachment {
	parts, _ := attachmentParts(structure)
	out := make([]mail.Attachment, 0, len(parts))
	for _, part := range parts {
		id, err := formatPartID(part.path)
		if err != nil {
			continue
		}
		meta, _ := mail.NewProviderMetadata(imapErrProvider(c.provider), mail.MetadataAttachment, mail.AttachmentProviderMetadata{Source: mail.MetadataSourceIMAP})
		estimate := estimatedDecodedSize(part.encodedSize, part.encoding)
		out = append(out, mail.Attachment{
			ID:               id,
			Name:             part.filename,
			ContentType:      part.mediaType,
			Size:             int64(estimate),
			IsInline:         part.disposition == "inline",
			ContentID:        part.contentID,
			Kind:             attachmentKind(part),
			ProviderType:     "imap_mime_part",
			ProviderMeta:     meta,
			SizeFactsPresent: true,
			SizeFacts: mail.AttachmentSizeFacts{
				EncodedSize:      part.encodedSize,
				EncodedSizeKnown: true,
				DecodedSize:      estimate,
			},
		})
	}
	return out
}

// estimatedDecodedSize guesses the file size from the encoded size: base64
// stores 57 bytes in each 78-byte line.
func estimatedDecodedSize(encoded uint64, encoding string) uint64 {
	if strings.EqualFold(strings.TrimSpace(encoding), "base64") {
		return encoded * 57 / 78
	}
	return encoded
}

func attachmentKind(part *imapBodyPart) mail.AttachmentKind {
	if part == nil {
		return mail.AttachmentKindUnknown
	}
	if strings.HasPrefix(part.mediaType, "message/") {
		return mail.AttachmentKindItem
	}
	return mail.AttachmentKindFile
}

func samePath(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fetchAttachmentContent streams one part from the selected mailbox into a
// private temporary file, decoding it on the way, and returns that file. The
// file deletes itself when closed.
func (c *accountClient) fetchAttachmentContent(ctx context.Context, session *imapSession, uid uint32, path []uint32, part *imapBodyPart, messageID, attachmentID string) (mail.MessageAttachmentContent, error) {
	section := &goimap.FetchItemBodySection{Part: uintPathToInt(path), Peek: true}
	var stream io.ReadCloser
	err := session.withAttachmentFetch(ctx, goimap.UID(uid), path, func(dep *imapclient.Client) error {
		cmd := dep.Fetch(goimap.UIDSetNum(goimap.UID(uid)), &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{section}})
		defer func() { _ = cmd.Close() }()
		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}
			for {
				item := msg.Next()
				if item == nil {
					break
				}
				body, ok := item.(imapclient.FetchItemDataBodySection)
				if !ok || stream != nil || !body.MatchCommand(section) {
					continue
				}
				var err error
				stream, err = c.decodeAttachmentLiteral(body.Literal, part.encoding, messageID, attachmentID)
				if err != nil {
					return err
				}
			}
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		if stream == nil {
			return attachmentNotFound(c, messageID, attachmentID)
		}
		return nil
	})
	if err != nil {
		if stream != nil {
			_ = stream.Close()
		}
		return mail.MessageAttachmentContent{}, err
	}
	return mail.MessageAttachmentContent{AttachmentID: attachmentID, Content: stream, ContentType: part.mediaType}, nil
}

func (c *accountClient) decodeAttachmentLiteral(literal io.Reader, encoding, messageID, attachmentID string) (io.ReadCloser, error) {
	if literal == nil {
		literal = strings.NewReader("")
	}
	reader := attachmentTransferReader(literal, encoding)
	f, err := os.CreateTemp("", "maildo-imap-attachment-*")
	if err != nil {
		return nil, attachmentTempError(c)
	}
	path := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(path)
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return nil, attachmentTempError(c)
	}
	limited := &io.LimitedReader{R: reader, N: mail.MaxMessageAttachmentDownloadBytes + 1}
	written, err := io.Copy(f, limited)
	if err != nil && !brokenEncoding(err) {
		cleanup()
		return nil, c.providerDataError(errs.ProviderOperationIMAPAttachmentsRead, errs.ProviderDataReasonInvalidMIME)
	}
	if written > mail.MaxMessageAttachmentDownloadBytes {
		cleanup()
		return nil, errs.NewIMAPAttachmentTooLarge(c.account, imapErrProvider(c.provider), messageID, attachmentID, errs.IMAPAttachmentSizeDecoded, uint64(mail.MaxMessageAttachmentDownloadBytes), uint64(written))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, attachmentTempError(c)
	}
	return &removeOnCloseFile{File: f, path: path}, nil
}

// attachmentTransferReader decodes base64 or quoted-printable. Base64 line
// breaks are ignored by the decoder; unknown encodings pass through as is.
func attachmentTransferReader(raw io.Reader, encoding string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, &skipSpaceReader{r: raw})
	case "quoted-printable":
		return quotedprintable.NewReader(raw)
	default:
		return raw
	}
}

// brokenEncoding reports a decoding error in the attachment itself. The
// file then ends early, but it holds everything that decoded.
func brokenEncoding(err error) bool {
	var corrupt base64.CorruptInputError
	return errors.As(err, &corrupt) || strings.HasPrefix(err.Error(), "quotedprintable:")
}

// skipSpaceReader drops spaces and tabs, which some mailers put into base64
// lines and which Go's decoder would reject.
type skipSpaceReader struct{ r io.Reader }

func (s *skipSpaceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	out := p[:0]
	for _, b := range p[:n] {
		if b != ' ' && b != '\t' {
			out = append(out, b)
		}
	}
	return len(out), err
}

type removeOnCloseFile struct {
	*os.File
	path string
}

func (f *removeOnCloseFile) Close() error {
	if f == nil || f.File == nil {
		return nil
	}
	err := f.File.Close()
	if removeErr := os.Remove(f.path); err == nil && removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = removeErr
	}
	return err
}

func attachmentTempError(c *accountClient) error {
	return errs.NewWithDetails(errs.CodeProviderConnectionError, "could not prepare a temporary file for the attachment", map[string]any{
		"account":  c.account,
		"provider": c.provider,
	})
}

func attachmentNotFound(c *accountClient, messageID, attachmentID string) error {
	return errs.NewWithDetails(errs.CodeAttachmentNotFound, fmt.Sprintf("attachment %s not found", attachmentID), map[string]any{
		"account":       c.account,
		"provider":      c.provider,
		"message_id":    messageID,
		"attachment_id": attachmentID,
	})
}
