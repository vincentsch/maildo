package imap

import (
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/provider"
	"golang.org/x/text/unicode/norm"
)

var (
	_ provider.FolderManager  = (*accountClient)(nil)
	_ provider.MessageMutator = (*accountClient)(nil)
)

type imapActiveRevision string

const (
	imapActiveRev1 imapActiveRevision = "imap4rev1"
	imapActiveRev2 imapActiveRevision = "imap4rev2"
)

type imapSessionState struct {
	revision     imapActiveRevision
	capabilities goimap.CapSet
}

func (s *imapSession) activeState(ctx context.Context) (imapSessionState, error) {
	if s == nil || s.client == nil {
		return imapSessionState{}, errs.NewSharedProviderContractInvalid()
	}
	if err := ctx.Err(); err != nil {
		return imapSessionState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.client == nil {
		return imapSessionState{}, errs.NewSharedProviderContractInvalid()
	}
	if s.activeKnown {
		return imapSessionState{revision: s.activeRev, capabilities: s.activeCaps.Copy()}, nil
	}
	phaseCtx, cancel := withDeadline(ctx, s.conn, commandTimeout)
	defer cancel()
	caps := s.client.Caps()
	if caps == nil {
		return imapSessionState{}, providerData(s.account, s.provider, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
	}
	hasRev1 := advertisedCapability(caps, goimap.CapIMAP4rev1)
	hasRev2 := advertisedCapability(caps, goimap.CapIMAP4rev2)
	switch {
	case hasRev2 && !hasRev1:
		s.activeKnown = true
		s.activeRev = imapActiveRev2
		s.activeCaps = caps.Copy()
	case hasRev1 && !hasRev2:
		s.activeKnown = true
		s.activeRev = imapActiveRev1
		s.activeCaps = caps.Copy()
	case hasRev1 && hasRev2:
		disarm := func() imapMutationObservation { return imapMutationObservation{} }
		if s.limiter != nil {
			disarm = s.limiter.armMutationObserver("ENABLE")
		}
		data, err := s.client.Enable(goimap.CapIMAP4rev2).Wait()
		obs := disarm()
		if err != nil {
			var imapErr *goimap.Error
			if errors.As(err, &imapErr) && imapErr.Type == goimap.StatusResponseTypeBad {
				if obs.hasAnyEnabled() {
					s.abortLocked()
					return imapSessionState{}, providerData(s.account, s.provider, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
				}
				s.activeKnown = true
				s.activeRev = imapActiveRev1
				s.activeCaps = caps.Copy()
				return imapSessionState{revision: s.activeRev, capabilities: s.activeCaps.Copy()}, nil
			}
			if errors.As(err, &imapErr) && imapErr.Type == goimap.StatusResponseTypeNo {
				s.abortLocked()
				return imapSessionState{}, providerData(s.account, s.provider, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
			}
			s.abortLocked()
			return imapSessionState{}, mapIMAPDependencyFailure(s.account, s.provider, phaseCtx, err)
		}
		if data == nil || !advertisedCapability(data.Caps, goimap.CapIMAP4rev2) || !obs.hasEnabled(goimap.CapIMAP4rev2) {
			s.abortLocked()
			return imapSessionState{}, providerData(s.account, s.provider, errs.ProviderProtocolIMAP, errs.ProviderDataReasonInvalidProtocol)
		}
		s.activeKnown = true
		s.activeRev = imapActiveRev2
		s.activeCaps = caps.Copy()
	default:
		return imapSessionState{}, unsupportedIMAPVersion(s.account, s.provider)
	}
	if err := phaseCtx.Err(); err != nil {
		return imapSessionState{}, mapIMAPDependencyFailure(s.account, s.provider, phaseCtx, err)
	}
	return imapSessionState{revision: s.activeRev, capabilities: s.activeCaps.Copy()}, nil
}

func (s *imapSession) abortLocked() {
	if s == nil || s.closed {
		return
	}
	client := s.client
	conn := s.conn
	stop := s.stop
	s.closed = true
	s.client = nil
	s.conn = nil
	s.stop = nil
	if stop != nil {
		stop()
	}
	if client != nil {
		_ = client.Close()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func advertisedCapability(caps goimap.CapSet, cap goimap.Cap) bool {
	if caps == nil {
		return false
	}
	_, ok := caps[cap]
	return ok
}

func (o imapMutationObservation) hasEnabled(cap goimap.Cap) bool {
	for _, code := range o.Untagged {
		if code.Name != "ENABLED" || code.Malformed {
			continue
		}
		for _, arg := range code.Args {
			if strings.EqualFold(arg, string(cap)) {
				return true
			}
		}
	}
	return false
}

func (o imapMutationObservation) hasAnyEnabled() bool {
	for _, code := range o.Untagged {
		if code.Name == "ENABLED" {
			return true
		}
	}
	return false
}

func (s *imapSession) withMutationCommand(ctx context.Context, attempt errs.ProviderWriteAttempt, expected string, fn func(*imapclient.Client) error, classify func(imapMutationObservation, error) error) (imapMutationObservation, error) {
	if s == nil || s.client == nil || attempt == nil || fn == nil {
		return imapMutationObservation{}, errs.NewSharedProviderContractInvalid()
	}
	if err := ctx.Err(); err != nil {
		return imapMutationObservation{}, errs.NewWriteNotApplied(attempt, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.client == nil {
		return imapMutationObservation{}, errs.NewWriteNotApplied(attempt, errs.NewProviderClientClosed(s.account, imapErrProvider(s.provider)))
	}
	phaseCtx, cancel := withDeadline(ctx, s.conn, commandTimeout)
	defer cancel()
	if err := phaseCtx.Err(); err != nil {
		return imapMutationObservation{}, errs.NewWriteNotApplied(attempt, err)
	}
	disarm := func() imapMutationObservation { return imapMutationObservation{} }
	if s.limiter != nil {
		disarm = s.limiter.armMutationObserver(expected)
	}
	if err := errs.CrossProviderWriteTransmissionBoundary(attempt); err != nil {
		return disarm(), err
	}
	err := fn(s.client)
	obs := disarm()
	if err != nil {
		if classify != nil {
			return obs, classify(obs, err)
		}
		return obs, classifyUnknownMutationFailure(attempt, phaseCtx, err)
	}
	if err := phaseCtx.Err(); err != nil {
		return obs, errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	return obs, nil
}

func (s *imapSession) withObservedReadCommand(ctx context.Context, op errs.ProviderOperation, expected string, fn func(*imapclient.Client) error) (imapMutationObservation, error) {
	if s == nil || s.client == nil || fn == nil {
		return imapMutationObservation{}, errs.NewSharedProviderContractInvalid()
	}
	if err := ctx.Err(); err != nil {
		return imapMutationObservation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.client == nil {
		return imapMutationObservation{}, errs.NewSharedProviderContractInvalid()
	}
	phaseCtx, cancel := withDeadline(ctx, s.conn, commandTimeout)
	defer cancel()
	if err := phaseCtx.Err(); err != nil {
		return imapMutationObservation{}, err
	}
	disarm := func() imapMutationObservation { return imapMutationObservation{} }
	if s.limiter != nil {
		disarm = s.limiter.armMutationObserver(expected)
	}
	err := fn(s.client)
	obs := disarm()
	if err != nil {
		return obs, mapKnownIMAPDependencyFailure(s.account, s.provider, phaseCtx, err)
	}
	if err := phaseCtx.Err(); err != nil {
		return obs, mapContextPhase(s.account, s.provider, errs.ProviderProtocolIMAP, err)
	}
	return obs, nil
}

func classifyUnknownMutationFailure(attempt errs.ProviderWriteAttempt, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if mutationFailureResponseLost(ctx, err) {
		return errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	return errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
}

func mutationFailureResponseLost(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || (ctx != nil && ctx.Err() != nil) {
		return true
	}
	if isFramingError(err, errLineLimit) || isFramingError(err, errLiteralLimit) || isFramingError(err, errInvalidProtocol) {
		return true
	}
	var imapErr *goimap.Error
	if errors.As(err, &imapErr) && imapErr.Type == goimap.StatusResponseTypeBye {
		return true
	}
	var netErr netError
	if errors.As(err, &netErr) {
		return true
	}
	return false
}

func (c *accountClient) PrepareFolderRename(ctx context.Context, req mail.FolderRenamePreflightRequest) (mail.FolderMutationPreflight, error) {
	var out mail.FolderMutationPreflight
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		snapshot, err := c.fetchFolderSnapshot(ctx, session)
		if err != nil {
			return err
		}
		target, err := c.rebindFolder(snapshot, req.Target.ID, string(mail.FolderOperationRename))
		if err != nil {
			return err
		}
		preflight, err := c.folderPreflight(snapshot, target, mail.FolderOperationRename)
		if err != nil {
			return err
		}
		out = preflight
		return nil
	})
	return out, err
}

func (c *accountClient) PrepareFolderDelete(ctx context.Context, req mail.FolderDeletePreflightRequest) (mail.FolderMutationPreflight, error) {
	var out mail.FolderMutationPreflight
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		snapshot, err := c.fetchFolderSnapshot(ctx, session)
		if err != nil {
			return err
		}
		target, err := c.rebindFolder(snapshot, req.Target.ID, string(mail.FolderOperationDelete))
		if err != nil {
			return err
		}
		preflight, err := c.folderPreflight(snapshot, target, mail.FolderOperationDelete)
		if err != nil {
			return err
		}
		status, err := c.statusMessages(ctx, session, target.mailbox, string(mail.FolderOperationDelete))
		if err != nil {
			return err
		}
		preflight.TotalItemCount = int(status.total)
		preflight.TotalItemCountKnown = true
		preflight.ChildFolderCount = target.folder.ChildCount
		preflight.ChildFolderKnown = true
		preflight.CountsSource = mail.FolderCountsSourceRefreshed
		if !preflight.Protected && status.total > 0 {
			preflight.Protected = true
			preflight.ProtectionReason = "not_empty"
		}
		out = preflight
		return nil
	})
	return out, err
}

func (c *accountClient) CreateFolder(ctx context.Context, req mail.FolderCreateRequest) (mail.FolderMutationResult, error) {
	if strings.TrimSpace(req.DisplayName) == "" {
		return mail.FolderMutationResult{}, errs.NewSharedProviderContractInvalid()
	}
	var out mail.FolderMutationResult
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		// LIST "" "" tells the hierarchy delimiter and the namespace root new
		// folders go under. LIST "" "*" never includes that root row.
		root, err := c.discoverDefaultRoot(ctx, session)
		if err != nil {
			return err
		}
		snapshot, err := c.fetchFolderSnapshot(ctx, session)
		if err != nil {
			return err
		}
		if err := c.requireSpecialUseAuthority(snapshot, provider.CapFoldersMutate, "folder_create"); err != nil {
			return err
		}
		var parent *imapFolderRecord
		delimiter := root.delimiter
		base := root.mailbox
		if req.Parent != nil {
			if root.delimiter == 0 {
				return c.invalidFolderName(req.DisplayName, "parent_not_supported", "this IMAP server has no subfolders, so --parent cannot be used")
			}
			record, err := c.rebindFolder(snapshot, req.Parent.ID, "folder_create")
			if err != nil {
				return err
			}
			parent = &record
			delimiter = record.delimiter
			base = record.mailbox
		}
		if delimiter != 0 && strings.ContainsRune(req.DisplayName, delimiter) {
			return c.invalidFolderName(req.DisplayName, "reserved_hierarchy_separator", "the folder name contains the character this server uses to separate folder levels")
		}
		candidate, err := c.candidateMailbox(snapshot.activeRevision, base, delimiter, req.DisplayName)
		if err != nil {
			return err
		}
		if _, exists := snapshot.byMailbox[candidate]; exists {
			return c.invalidFolderName(req.DisplayName, "already_exists", "a folder with that name already exists")
		}
		attempt, err := c.newAttempt(errs.WriteIMAPFolderCreate)
		if err != nil {
			return err
		}
		_, err = session.withMutationCommand(ctx, attempt, "CREATE", func(dep *imapclient.Client) error {
			return dep.Create(candidate, nil).Wait()
		}, func(obs imapMutationObservation, writeErr error) error {
			return c.classifyFolderMutationFailure(attempt, errs.WriteIMAPFolderCreate, mail.FolderOperationCreate, obs, writeErr, "folder_create")
		})
		if err != nil {
			return err
		}
		verified, err := c.verifyFolderCreated(ctx, session, attempt, candidate, parent)
		if err != nil {
			return err
		}
		out = verified
		return nil
	})
	return out, err
}

func (c *accountClient) RenameFolder(ctx context.Context, req mail.FolderRenameRequest) (mail.FolderMutationResult, error) {
	var out mail.FolderMutationResult
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		snapshot, err := c.fetchFolderSnapshot(ctx, session)
		if err != nil {
			return err
		}
		target, err := c.rebindFolder(snapshot, req.Target.ID, string(mail.FolderOperationRename))
		if err != nil {
			return err
		}
		preflight, err := c.folderPreflight(snapshot, target, mail.FolderOperationRename)
		if err != nil {
			return err
		}
		if preflight.Protected {
			return c.protectedFolder(target.folder, preflight.ProtectionReason)
		}
		if target.delimiter != 0 && strings.ContainsRune(req.NewDisplayName, target.delimiter) {
			return c.invalidFolderName(req.NewDisplayName, "reserved_hierarchy_separator", "the folder name contains the character this server uses to separate folder levels")
		}
		candidate, err := c.renameCandidate(snapshot.activeRevision, target, req.NewDisplayName)
		if err != nil {
			return err
		}
		if candidate != target.mailbox {
			if _, exists := snapshot.byMailbox[candidate]; exists {
				return c.invalidFolderName(req.NewDisplayName, "already_exists", "a folder with that name already exists")
			}
		}
		if preflight.DescendantFacts.Count > 0 {
			return c.protectedFolder(target.folder, "has_children")
		}
		targetSummary := mail.SummaryFrom(target.folder)
		if candidate == target.mailbox {
			meta, err := c.folderMetadata(mail.MetadataFolderMutationResult)
			if err != nil {
				return err
			}
			out = mail.FolderMutationResult{
				Operation:              mail.FolderOperationRename,
				TargetFolder:           &targetSummary,
				RenamedFolder:          &targetSummary,
				OldDisplayName:         target.folder.DisplayName,
				NewDisplayName:         req.NewDisplayName,
				VerificationStatus:     mail.FolderVerificationReadBack,
				DescendantFacts:        preflight.DescendantFacts,
				DescendantFactsPresent: true,
				WriteOutcomeFacts:      errs.VerifiedWriteNoOpFacts(),
				ProviderMeta:           meta,
			}
			return nil
		}
		attempt, err := c.newAttempt(errs.WriteIMAPFolderRename)
		if err != nil {
			return err
		}
		_, err = session.withMutationCommand(ctx, attempt, "RENAME", func(dep *imapclient.Client) error {
			return dep.Rename(target.mailbox, candidate, nil).Wait()
		}, func(obs imapMutationObservation, writeErr error) error {
			return c.classifyFolderMutationFailure(attempt, errs.WriteIMAPFolderRename, mail.FolderOperationRename, obs, writeErr, "folder_rename")
		})
		if err != nil {
			return err
		}
		verified, err := c.verifyFolderRenamed(ctx, session, attempt, target, candidate, req.NewDisplayName, preflight.DescendantFacts)
		if err != nil {
			return err
		}
		out = verified
		return nil
	})
	return out, err
}

func (c *accountClient) DeleteFolder(ctx context.Context, req mail.FolderDeleteRequest) (mail.FolderMutationResult, error) {
	var out mail.FolderMutationResult
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		snapshot, err := c.fetchFolderSnapshot(ctx, session)
		if err != nil {
			return err
		}
		target, err := c.rebindFolder(snapshot, req.Target.ID, string(mail.FolderOperationDelete))
		if err != nil {
			return err
		}
		preflight, err := c.folderPreflight(snapshot, target, mail.FolderOperationDelete)
		if err != nil {
			return err
		}
		if preflight.Protected {
			return c.protectedFolder(target.folder, preflight.ProtectionReason)
		}
		status, err := c.statusMessages(ctx, session, target.mailbox, string(mail.FolderOperationDelete))
		if err != nil {
			return err
		}
		preflight.TotalItemCount = int(status.total)
		preflight.TotalItemCountKnown = true
		preflight.ChildFolderCount = target.folder.ChildCount
		preflight.ChildFolderKnown = true
		preflight.CountsSource = mail.FolderCountsSourceRefreshed
		if status.total > 0 {
			return c.protectedFolder(target.folder, "not_empty")
		}
		attempt, err := c.newAttempt(errs.WriteIMAPFolderDelete)
		if err != nil {
			return err
		}
		_, err = session.withMutationCommand(ctx, attempt, "DELETE", func(dep *imapclient.Client) error {
			return dep.Delete(target.mailbox).Wait()
		}, func(obs imapMutationObservation, writeErr error) error {
			return c.classifyFolderMutationFailure(attempt, errs.WriteIMAPFolderDelete, mail.FolderOperationDelete, obs, writeErr, "folder_delete")
		})
		if err != nil {
			return err
		}
		verified, err := c.verifyFolderDeleted(ctx, session, attempt, target, preflight)
		if err != nil {
			return err
		}
		out = verified
		return nil
	})
	return out, err
}

type imapRootFact struct {
	mailbox   string
	delimiter rune
}

func (c *accountClient) discoverDefaultRoot(ctx context.Context, session *imapSession) (imapRootFact, error) {
	var rows []*goimap.ListData
	if err := session.withReadCommand(ctx, errs.ProviderOperationIMAPFoldersRead, func(dep *imapclient.Client) error {
		var err error
		rows, err = dep.List("", "", nil).Collect()
		return err
	}); err != nil {
		return imapRootFact{}, err
	}
	if len(rows) == 0 {
		return imapRootFact{}, errs.NewIMAPDefaultRootInvalid(c.account, imapErrProvider(c.provider), errs.IMAPDefaultRootMissing)
	}
	var root *imapRootFact
	for _, row := range rows {
		if row == nil {
			return imapRootFact{}, errs.NewIMAPDefaultRootInvalid(c.account, imapErrProvider(c.provider), errs.IMAPDefaultRootDelimiterInvalid)
		}
		if row.Delim != 0 && !validDelimiter(row.Delim) {
			return imapRootFact{}, errs.NewIMAPDefaultRootInvalid(c.account, imapErrProvider(c.provider), errs.IMAPDefaultRootDelimiterInvalid)
		}
		next := imapRootFact{mailbox: row.Mailbox, delimiter: row.Delim}
		if root == nil {
			root = &next
			continue
		}
		if root.mailbox == next.mailbox && root.delimiter == next.delimiter {
			continue
		}
		if root.mailbox == next.mailbox {
			return imapRootFact{}, errs.NewIMAPDefaultRootInvalid(c.account, imapErrProvider(c.provider), errs.IMAPDefaultRootDelimiterConflict)
		}
		return imapRootFact{}, errs.NewIMAPDefaultRootInvalid(c.account, imapErrProvider(c.provider), errs.IMAPDefaultRootAmbiguous)
	}
	if root == nil {
		return imapRootFact{}, errs.NewIMAPDefaultRootInvalid(c.account, imapErrProvider(c.provider), errs.IMAPDefaultRootMissing)
	}
	return *root, nil
}

func validDelimiter(r rune) bool {
	return r > 0 && r >= 0x20 && r != 0x7f
}

func (c *accountClient) folderPreflight(snapshot imapFolderSnapshot, target imapFolderRecord, op mail.FolderMutationOperation) (mail.FolderMutationPreflight, error) {
	if err := c.requireSpecialUseAuthority(snapshot, provider.CapFoldersMutate, string(op)); err != nil {
		return mail.FolderMutationPreflight{}, err
	}
	facts := descendantFacts(snapshot, target)
	protected, reason := folderProtected(target, facts)
	meta, err := c.folderMetadata(mail.MetadataFolderMutationPreflight)
	if err != nil {
		return mail.FolderMutationPreflight{}, err
	}
	return mail.FolderMutationPreflight{
		Operation:              op,
		Target:                 mail.SummaryFrom(target.folder),
		Protected:              protected,
		ProtectionReason:       reason,
		ChildFolderCount:       target.folder.ChildCount,
		ChildFolderKnown:       true,
		DescendantFacts:        facts,
		DescendantFactsPresent: true,
		ProviderMeta:           meta,
	}, nil
}

func folderProtected(target imapFolderRecord, facts mail.FolderDescendantFacts) (bool, string) {
	if asciiEqualFold(target.mailbox, "INBOX") || target.folder.WellKnownName != "" || len(target.specialUse) > 0 {
		return true, "special_use_folder"
	}
	if facts.Count > 0 {
		return true, "has_children"
	}
	return false, ""
}

func descendantFacts(snapshot imapFolderSnapshot, target imapFolderRecord) mail.FolderDescendantFacts {
	rows := make([]mail.FolderDescendant, 0)
	if target.delimiter != 0 {
		prefix := target.mailbox + string(target.delimiter)
		for _, folder := range snapshot.folders {
			record, ok := snapshot.byID[folder.ID]
			if !ok || record.mailbox == target.mailbox || !strings.HasPrefix(record.mailbox, prefix) {
				continue
			}
			rows = append(rows, mail.FolderDescendant{ID: folder.ID, DisplayName: folder.DisplayName, Path: folder.Path})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		if rows[i].DisplayName != rows[j].DisplayName {
			return rows[i].DisplayName < rows[j].DisplayName
		}
		return rows[i].ID < rows[j].ID
	})
	facts := mail.FolderDescendantFacts{Count: uint64(len(rows)), Known: true}
	if len(rows) > 20 {
		facts.Sample = append([]mail.FolderDescendant(nil), rows[:20]...)
		facts.Truncated = true
	} else {
		facts.Sample = append([]mail.FolderDescendant(nil), rows...)
	}
	return facts
}

func (c *accountClient) rebindFolder(snapshot imapFolderSnapshot, id string, operation string) (imapFolderRecord, error) {
	record, ok := snapshot.byID[id]
	if !ok {
		return imapFolderRecord{}, c.folderNotFound(operation, id)
	}
	return record, nil
}

func (c *accountClient) candidateMailbox(revision imapActiveRevision, base string, delimiter rune, leaf string) (string, error) {
	name := leaf
	if base != "" {
		if delimiter == 0 {
			return "", c.invalidFolderName(leaf, "parent_not_supported", "this IMAP server has no subfolders")
		}
		name = base + string(delimiter) + leaf
	}
	if revision == imapActiveRev2 {
		name = norm.NFC.String(name)
	}
	if _, err := canonicalMailboxName(name); err != nil {
		return "", c.providerData("folder_create", "invalid_mailbox_encoding")
	}
	return canonicalMailboxName(name)
}

func (c *accountClient) renameCandidate(revision imapActiveRevision, target imapFolderRecord, leaf string) (string, error) {
	base := ""
	if target.delimiter != 0 {
		sep := strings.LastIndex(target.mailbox, string(target.delimiter))
		if sep >= 0 {
			base = target.mailbox[:sep]
		}
	}
	return c.candidateMailbox(revision, base, target.delimiter, leaf)
}

func (c *accountClient) verifyFolderCreated(ctx context.Context, session *imapSession, attempt errs.ProviderWriteAttempt, mailbox string, parent *imapFolderRecord) (mail.FolderMutationResult, error) {
	snapshot, err := c.fetchFolderSnapshot(ctx, session)
	if err != nil {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
	}
	record, ok := snapshot.byMailbox[mailbox]
	// A new folder has no role, except the one its name gives it on a
	// server without SPECIAL-USE ("Trash" is the trash there), which is
	// what doctor asks people to create.
	if !ok || (len(record.specialUse) != 0 && !slices.Equal(record.specialUse, specialUseFromName(record.mailbox, record.delimiter))) {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	if parent != nil && record.folder.ParentID != parent.folder.ID {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	meta, err := c.folderMetadata(mail.MetadataFolderMutationResult)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	created := mail.SummaryFrom(record.folder)
	result := mail.FolderMutationResult{
		Operation:          mail.FolderOperationCreate,
		CreatedFolder:      &created,
		VerificationStatus: mail.FolderVerificationReadBack,
		WriteOutcomeFacts:  facts,
		ProviderMeta:       meta,
	}
	if parent != nil {
		parentSummary := mail.SummaryFrom(parent.folder)
		result.ParentFolder = &parentSummary
	}
	return result, nil
}

func (c *accountClient) verifyFolderRenamed(ctx context.Context, session *imapSession, attempt errs.ProviderWriteAttempt, old imapFolderRecord, newMailbox string, newDisplay string, preFacts mail.FolderDescendantFacts) (mail.FolderMutationResult, error) {
	snapshot, err := c.fetchFolderSnapshot(ctx, session)
	if err != nil {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
	}
	record, ok := snapshot.byMailbox[newMailbox]
	if !ok {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	if _, stillOld := snapshot.byMailbox[old.mailbox]; stillOld || len(record.specialUse) != 0 || descendantFacts(snapshot, record).Count != 0 {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	meta, err := c.folderMetadata(mail.MetadataFolderMutationResult)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	target := mail.SummaryFrom(old.folder)
	renamed := mail.SummaryFrom(record.folder)
	return mail.FolderMutationResult{
		Operation:              mail.FolderOperationRename,
		TargetFolder:           &target,
		RenamedFolder:          &renamed,
		OldDisplayName:         old.folder.DisplayName,
		NewDisplayName:         newDisplay,
		VerificationStatus:     mail.FolderVerificationReadBack,
		DescendantFacts:        preFacts,
		DescendantFactsPresent: true,
		WriteOutcomeFacts:      facts,
		ProviderMeta:           meta,
	}, nil
}

func (c *accountClient) verifyFolderDeleted(ctx context.Context, session *imapSession, attempt errs.ProviderWriteAttempt, old imapFolderRecord, preflight mail.FolderMutationPreflight) (mail.FolderMutationResult, error) {
	snapshot, err := c.fetchFolderSnapshot(ctx, session)
	if err != nil {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
	}
	if _, stillOld := snapshot.byMailbox[old.mailbox]; stillOld {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	meta, err := c.folderMetadata(mail.MetadataFolderMutationResult)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	deleted := mail.SummaryFrom(old.folder)
	return mail.FolderMutationResult{
		Operation:              mail.FolderOperationDelete,
		DeletedFolder:          &deleted,
		TotalItemCount:         preflight.TotalItemCount,
		TotalItemCountKnown:    preflight.TotalItemCountKnown,
		ChildFolderCount:       preflight.ChildFolderCount,
		ChildFolderKnown:       preflight.ChildFolderKnown,
		CountsSource:           preflight.CountsSource,
		VerificationStatus:     mail.FolderVerificationReadBack,
		DescendantFacts:        preflight.DescendantFacts,
		DescendantFactsPresent: preflight.DescendantFactsPresent,
		WriteOutcomeFacts:      facts,
		ProviderMeta:           meta,
	}, nil
}

func (c *accountClient) statusMessages(ctx context.Context, session *imapSession, mailbox string, operation string) (imapMailboxStatus, error) {
	var status *goimap.StatusData
	err := session.withReadCommand(ctx, errs.ProviderOperationIMAPFoldersRead, func(dep *imapclient.Client) error {
		var err error
		status, err = dep.Status(mailbox, &goimap.StatusOptions{NumMessages: true}).Wait()
		return err
	})
	if err != nil {
		return imapMailboxStatus{}, err
	}
	if status == nil || status.NumMessages == nil {
		return imapMailboxStatus{}, c.providerData(operation, "invalid_protocol")
	}
	return imapMailboxStatus{total: *status.NumMessages, known: true}, nil
}

func (c *accountClient) MoveMessage(ctx context.Context, req mail.MessageMoveRequest) (mail.MessageMutationResult, error) {
	return c.copyOrMoveMessage(ctx, mail.MutationMove, req.MessageID, &req.Folder, errs.WriteIMAPMove)
}

func (c *accountClient) CopyMessage(ctx context.Context, req mail.MessageCopyRequest) (mail.MessageMutationResult, error) {
	return c.copyOrMoveMessage(ctx, mail.MutationCopy, req.MessageID, &req.Folder, errs.WriteIMAPCopy)
}

func (c *accountClient) ArchiveMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	return c.copyOrMoveMessage(ctx, mail.MutationArchive, req.MessageID, nil, errs.WriteIMAPArchive)
}

func (c *accountClient) DeleteMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	return c.copyOrMoveMessage(ctx, mail.MutationDelete, req.MessageID, nil, errs.WriteIMAPDelete)
}

func (c *accountClient) RestoreMessage(ctx context.Context, req mail.MessageRestoreRequest) (mail.MessageMutationResult, error) {
	return c.copyOrMoveMessage(ctx, mail.MutationRestore, req.MessageID, &req.Folder, errs.WriteIMAPRestore)
}

func (c *accountClient) JunkMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	return c.copyOrMoveMessage(ctx, mail.MutationJunk, req.MessageID, nil, errs.WriteIMAPJunk)
}

func (c *accountClient) SetMessageReadState(ctx context.Context, req mail.MessageReadStateRequest) (mail.MessageMutationResult, error) {
	op := mail.MutationMarkUnread
	writeOp := errs.WriteIMAPMarkUnread
	if req.IsRead {
		op = mail.MutationMarkRead
		writeOp = errs.WriteIMAPMarkRead
	}
	return c.storeFlag(ctx, op, req.MessageID, req.IsRead, goimap.FlagSeen, writeOp)
}

func (c *accountClient) SetMessageFlag(ctx context.Context, req mail.MessageFlagRequest) (mail.MessageMutationResult, error) {
	op := mail.MutationFlagClear
	writeOp := errs.WriteIMAPFlagClear
	if req.Flagged {
		op = mail.MutationFlagSet
		writeOp = errs.WriteIMAPFlagSet
	}
	return c.storeFlag(ctx, op, req.MessageID, req.Flagged, goimap.FlagFlagged, writeOp)
}

type imapMessagePreflight struct {
	state    imapSessionState
	snapshot imapFolderSnapshot
	source   imapFolderRecord
	target   *imapFolderRecord
	message  imapFetchedMessage
}

func (c *accountClient) copyOrMoveMessage(ctx context.Context, op mail.MessageMutationOperation, messageID string, explicitTarget *mail.Folder, writeOp errs.ProviderWriteOperation) (mail.MessageMutationResult, error) {
	var out mail.MessageMutationResult
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		pre, err := c.messagePreflight(ctx, session, op, messageID, explicitTarget, true)
		if err != nil {
			return err
		}
		if pre.target == nil {
			return errs.NewSharedProviderContractInvalid()
		}
		if pre.source.mailbox == pre.target.mailbox {
			if op == mail.MutationCopy {
				return errs.NewWithDetails(errs.CodeInvalidSelector, "the message is already in that folder, so there is nothing to copy it to", map[string]any{
					"account":    c.account,
					"provider":   c.provider,
					"reason":     "copy_target_is_source",
					"message_id": messageID,
				})
			}
			result, err := c.moveNoOpResult(op, pre)
			if err != nil {
				return err
			}
			out = result
			return nil
		}
		if err := c.requireEffectiveUIDPlus(pre.state, op); err != nil {
			return err
		}
		if op != mail.MutationCopy {
			if err := c.requireEffectiveMove(pre.state, op); err != nil {
				return err
			}
			selectData, err := c.selectMailboxReadWrite(ctx, session, pre.source.mailbox, false, messageCapability(op), string(op))
			if err != nil {
				return err
			}
			if selectData.uidValidity != pre.source.uidValidity {
				return errs.NewIMAPMessageNotFound(c.account, imapErrProvider(c.provider), "id", messageID, errs.IMAPMessageNotFoundUIDValidityChanged)
			}
		}
		attempt, err := c.newAttempt(writeOp)
		if err != nil {
			return err
		}
		var data imapCopyResult
		var obs imapMutationObservation
		expected := "UID COPY"
		if op == mail.MutationCopy {
			obs, err = session.withMutationCommand(ctx, attempt, expected, func(dep *imapclient.Client) error {
				_, err := dep.Copy(goimap.UIDSetNum(goimap.UID(pre.message.uid)), pre.target.mailbox).Wait()
				return err
			}, func(obs imapMutationObservation, writeErr error) error {
				return c.classifyMessageMutationFailure(attempt, writeOp, op, obs, writeErr, pre.message.uid)
			})
		} else {
			expected = "UID MOVE"
			obs, err = session.withMutationCommand(ctx, attempt, expected, func(dep *imapclient.Client) error {
				_, err := dep.Move(goimap.UIDSetNum(goimap.UID(pre.message.uid)), pre.target.mailbox).Wait()
				return err
			}, func(obs imapMutationObservation, writeErr error) error {
				return c.classifyMessageMutationFailure(attempt, writeOp, op, obs, writeErr, pre.message.uid)
			})
		}
		if err != nil {
			return err
		}
		data, err = c.validateCopyUIDObservation(attempt, obs, pre.message.uid, op, pre.state.revision)
		if err != nil {
			return err
		}
		verified, err := c.verifyCopiedMessage(ctx, session, attempt, op, pre, data)
		if err != nil {
			return err
		}
		out = verified
		return nil
	})
	return out, err
}

func (c *accountClient) messagePreflight(ctx context.Context, session *imapSession, op mail.MessageMutationOperation, messageID string, explicitTarget *mail.Folder, needTarget bool) (imapMessagePreflight, error) {
	identity, err := parseMessageID(messageID)
	if err != nil {
		return imapMessagePreflight{}, errs.NewWithDetails(errs.CodeInvalidSelector, "this is neither an IMAP message id nor a short id from maildo's tables", map[string]any{
			"account":       c.account,
			"provider":      c.provider,
			"selector_kind": "id",
			"message_id":    messageID,
		})
	}
	state, err := session.activeState(ctx)
	if err != nil {
		return imapMessagePreflight{}, err
	}
	snapshot, err := c.fetchFolderSnapshot(ctx, session)
	if err != nil {
		return imapMessagePreflight{}, err
	}
	source, ok := snapshot.byMailbox[identity.Mailbox]
	if !ok || !source.selectable {
		return imapMessagePreflight{}, errs.NewIMAPSourceMailboxNotSelectable(c.account, imapErrProvider(c.provider), string(messageCapability(op)), messageID)
	}
	var target *imapFolderRecord
	if needTarget {
		var rec imapFolderRecord
		if explicitTarget != nil {
			rec, err = c.rebindFolder(snapshot, explicitTarget.ID, string(op))
			if err != nil {
				return imapMessagePreflight{}, err
			}
		} else {
			rec, err = c.resolveRoleTarget(snapshot, op)
			if err != nil {
				return imapMessagePreflight{}, err
			}
		}
		if !rec.selectable {
			return imapMessagePreflight{}, c.unsupported(messageCapability(op), "target_mailbox_not_selectable", map[string]any{"folder_id": rec.folder.ID})
		}
		targetStatus, err := c.examineMailbox(ctx, session, rec.mailbox, errs.ProviderOperationIMAPMessagesRead)
		if err != nil {
			return imapMessagePreflight{}, err
		}
		if targetStatus.uidNotSticky {
			return imapMessagePreflight{}, c.unsupported(messageCapability(op), "persistent_uids_unavailable", map[string]any{"folder_id": rec.folder.ID, "requirement": "stable_uid"})
		}
		rec.uidValidity = targetStatus.uidValidity
		rec.uidNext = targetStatus.uidNext
		target = &rec
	}
	status, err := c.examineMailbox(ctx, session, source.mailbox, errs.ProviderOperationIMAPMessagesRead)
	if err != nil {
		return imapMessagePreflight{}, err
	}
	if status.uidNotSticky {
		return imapMessagePreflight{}, c.unsupported(messageCapability(op), "persistent_uids_unavailable", map[string]any{"folder_id": source.folder.ID, "requirement": "stable_uid"})
	}
	source.uidValidity = status.uidValidity
	source.uidNext = status.uidNext
	if source.uidValidity != identity.UIDValidity {
		return imapMessagePreflight{}, errs.NewIMAPMessageNotFound(c.account, imapErrProvider(c.provider), "id", messageID, errs.IMAPMessageNotFoundUIDValidityChanged)
	}
	msg, ok, err := c.fetchMessageRecord(ctx, session, source, identity.UID, errs.ProviderOperationIMAPMessagesRead, imapFetchProjection{
		flags:        true,
		rfc822Size:   true,
		header:       true,
		headerFields: []string{"Message-ID"},
	})
	if err != nil {
		return imapMessagePreflight{}, err
	}
	if !ok {
		return imapMessagePreflight{}, errs.NewIMAPMessageNotFound(c.account, imapErrProvider(c.provider), "id", messageID, errs.IMAPMessageNotFoundMissing)
	}
	return imapMessagePreflight{state: state, snapshot: snapshot, source: source, target: target, message: msg}, nil
}

func (c *accountClient) resolveRoleTarget(snapshot imapFolderSnapshot, op mail.MessageMutationOperation) (imapFolderRecord, error) {
	if err := c.requireSpecialUseAuthority(snapshot, messageCapability(op), string(op)); err != nil {
		return imapFolderRecord{}, err
	}
	role := mail.ProviderSpecialUse("")
	switch op {
	case mail.MutationArchive:
		role = mail.SpecialUseArchive
	case mail.MutationDelete:
		role = mail.SpecialUseTrash
	case mail.MutationJunk:
		role = mail.SpecialUseJunk
	default:
		return imapFolderRecord{}, errs.NewSharedProviderContractInvalid()
	}
	var matches []imapFolderRecord
	for _, folder := range snapshot.folders {
		record := snapshot.byID[folder.ID]
		for _, special := range record.specialUse {
			if special == role {
				matches = append(matches, record)
				break
			}
		}
	}
	switch len(matches) {
	case 0:
		return imapFolderRecord{}, c.unsupported(messageCapability(op), "special_use_role_missing", map[string]any{"role": specialUseRoleName(role)})
	case 1:
		return matches[0], nil
	default:
		return imapFolderRecord{}, c.unsupported(messageCapability(op), "special_use_role_ambiguous", map[string]any{"role": specialUseRoleName(role)})
	}
}

func specialUseRoleName(role mail.ProviderSpecialUse) string {
	switch role {
	case mail.SpecialUseArchive:
		return "archive"
	case mail.SpecialUseTrash:
		return "trash"
	case mail.SpecialUseJunk:
		return "junk"
	default:
		return string(role)
	}
}

func (c *accountClient) requireEffectiveUIDPlus(state imapSessionState, op mail.MessageMutationOperation) error {
	if state.revision == imapActiveRev2 || advertisedCapability(state.capabilities, goimap.CapUIDPlus) {
		return nil
	}
	return c.unsupported(messageCapability(op), "required_extension_missing", map[string]any{"requirement": "UIDPLUS"})
}

func (c *accountClient) requireEffectiveMove(state imapSessionState, op mail.MessageMutationOperation) error {
	if state.revision == imapActiveRev2 || advertisedCapability(state.capabilities, goimap.CapMove) {
		return nil
	}
	return c.unsupported(messageCapability(op), "required_extension_missing", map[string]any{"requirement": "MOVE"})
}

type imapCopyResult struct {
	uidValidity uint32
	sourceUIDs  string
	destUIDs    string
}

type imapObservedCopyUID struct {
	code        imapObservedResponseCode
	uidValidity uint32
	sourceUID   uint32
	destUID     uint32
}

func (c *accountClient) validateCopyUIDObservation(attempt errs.ProviderWriteAttempt, obs imapMutationObservation, sourceUID uint32, op mail.MessageMutationOperation, revision imapActiveRevision) (imapCopyResult, error) {
	candidates := observedCopyUIDCodes(obs)
	if len(candidates) == 0 {
		return imapCopyResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMissing)
	}
	if len(candidates) != 1 {
		return imapCopyResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	observed, ok := parseObservedCopyUID(candidates[0])
	if !ok || observed.sourceUID != sourceUID {
		return imapCopyResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	if !copyUIDPlacementAllowed(observed.code, op, revision) {
		return imapCopyResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	return imapCopyResult{
		uidValidity: observed.uidValidity,
		sourceUIDs:  strconv.FormatUint(uint64(observed.sourceUID), 10),
		destUIDs:    strconv.FormatUint(uint64(observed.destUID), 10),
	}, nil
}

func observedCopyUIDCodes(obs imapMutationObservation) []imapObservedResponseCode {
	codes := observedResponseCodes(obs)
	out := make([]imapObservedResponseCode, 0, len(codes))
	for _, code := range codes {
		if code.Name == "COPYUID" {
			out = append(out, code)
		}
	}
	return out
}

func parseObservedCopyUID(code imapObservedResponseCode) (imapObservedCopyUID, bool) {
	if code.Name != "COPYUID" || code.Malformed || len(code.Args) != 3 {
		return imapObservedCopyUID{}, false
	}
	uidValidity, err := parseCanonicalUint32(code.Args[0])
	if err != nil {
		return imapObservedCopyUID{}, false
	}
	src, err := ParseProviderUIDSet(code.Args[1], 1, math.MaxUint32, 1)
	if err != nil {
		return imapObservedCopyUID{}, false
	}
	sourceUID, ok := src.ExactSingleton()
	if !ok {
		return imapObservedCopyUID{}, false
	}
	dst, err := ParseProviderUIDSet(code.Args[2], 1, math.MaxUint32, 1)
	if err != nil {
		return imapObservedCopyUID{}, false
	}
	destUID, ok := dst.ExactSingleton()
	if !ok {
		return imapObservedCopyUID{}, false
	}
	return imapObservedCopyUID{
		code:        code,
		uidValidity: uidValidity,
		sourceUID:   sourceUID,
		destUID:     destUID,
	}, true
}

func copyUIDPlacementAllowed(code imapObservedResponseCode, op mail.MessageMutationOperation, revision imapActiveRevision) bool {
	if code.Status != "OK" {
		return false
	}
	if op == mail.MutationCopy {
		return code.Tagged
	}
	if revision == imapActiveRev2 {
		return !code.Tagged && !code.RemovalBefore
	}
	return true
}

func (c *accountClient) verifyCopiedMessage(ctx context.Context, session *imapSession, attempt errs.ProviderWriteAttempt, op mail.MessageMutationOperation, pre imapMessagePreflight, data imapCopyResult) (mail.MessageMutationResult, error) {
	if pre.target == nil {
		return mail.MessageMutationResult{}, errs.NewSharedProviderContractInvalid()
	}
	dstSet, err := ParseProviderUIDSet(data.destUIDs, 1, math.MaxUint32, 1)
	if err != nil {
		return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	dstUID, _ := dstSet.ExactSingleton()
	targetStatus, err := c.examineMailbox(ctx, session, pre.target.mailbox, errs.ProviderOperationIMAPMessagesRead)
	if err != nil {
		return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
	}
	if targetStatus.uidValidity != data.uidValidity {
		return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	targetRecord := *pre.target
	targetRecord.uidValidity = targetStatus.uidValidity
	targetRecord.uidNext = targetStatus.uidNext
	dstMsg, ok, err := c.fetchMessageRecord(ctx, session, targetRecord, dstUID, errs.ProviderOperationIMAPMessagesRead, imapFetchProjection{
		rfc822Size:   true,
		header:       true,
		headerFields: []string{"Message-ID"},
	})
	if err != nil {
		return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
	}
	if !ok || dstMsg.rfc822Size != pre.message.rfc822Size || dstMsg.header.messageID != pre.message.header.messageID {
		return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	if op != mail.MutationCopy {
		sourceStatus, err := c.examineMailbox(ctx, session, pre.source.mailbox, errs.ProviderOperationIMAPMessagesRead)
		if err != nil {
			return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
		}
		if sourceStatus.uidValidity != pre.source.uidValidity {
			return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
		}
		sourceRecord := pre.source
		sourceRecord.uidValidity = sourceStatus.uidValidity
		sourceRecord.uidNext = sourceStatus.uidNext
		if _, ok, err := c.fetchMessageRecord(ctx, session, sourceRecord, pre.message.uid, errs.ProviderOperationIMAPMessagesRead, imapFetchProjection{flags: true}); err != nil {
			return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
		} else if ok {
			return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
		}
	}
	resultID, err := formatMessageID(pre.target.mailbox, data.uidValidity, dstUID)
	if err != nil {
		return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	meta, err := c.mutationMetadata(nil)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	changed := true
	source := mail.SummaryFrom(pre.source.folder)
	target := mail.SummaryFrom(pre.target.folder)
	return mail.MessageMutationResult{
		MessageID:         pre.message.id,
		ResultMessageID:   resultID,
		InternetMessageID: pre.message.internetMessageID,
		SourceFolder:      &source,
		TargetFolder:      &target,
		Changed:           &changed,
		WriteOutcomeFacts: facts,
		ProviderMeta:      meta,
	}, nil
}

func (c *accountClient) moveNoOpResult(op mail.MessageMutationOperation, pre imapMessagePreflight) (mail.MessageMutationResult, error) {
	meta, err := c.mutationMetadata(nil)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	changed := false
	source := mail.SummaryFrom(pre.source.folder)
	target := mail.SummaryFrom(pre.target.folder)
	return mail.MessageMutationResult{
		MessageID:         pre.message.id,
		InternetMessageID: pre.message.internetMessageID,
		SourceFolder:      &source,
		TargetFolder:      &target,
		Changed:           &changed,
		WriteOutcomeFacts: errs.VerifiedWriteNoOpFacts(),
		ProviderMeta:      meta,
	}, nil
}

func (c *accountClient) storeFlag(ctx context.Context, op mail.MessageMutationOperation, messageID string, desired bool, flag goimap.Flag, writeOp errs.ProviderWriteOperation) (mail.MessageMutationResult, error) {
	var out mail.MessageMutationResult
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		pre, err := c.messagePreflight(ctx, session, op, messageID, nil, false)
		if err != nil {
			return err
		}
		guarded := false
		condstore := advertisedCapability(pre.state.capabilities, goimap.CapCondStore) || advertisedCapability(pre.state.capabilities, goimap.CapQResync)
		selectData, err := c.selectMailboxReadWrite(ctx, session, pre.source.mailbox, condstore, messageCapability(op), string(op))
		if err != nil {
			return err
		}
		if selectData.uidValidity != pre.source.uidValidity {
			return errs.NewIMAPMessageNotFound(c.account, imapErrProvider(c.provider), "id", messageID, errs.IMAPMessageNotFoundUIDValidityChanged)
		}
		if condstore {
			switch {
			case selectData.highestModSeqKnown:
				guarded = true
			case selectData.noModSeq:
				guarded = false
			default:
				return c.providerData(string(op), "invalid_protocol")
			}
		}
		msg, ok, err := c.fetchStoreMessage(ctx, session, pre.source, pre.message.uid, guarded)
		if err != nil {
			return err
		}
		if !ok {
			return errs.NewIMAPMessageNotFound(c.account, imapErrProvider(c.provider), "id", messageID, errs.IMAPMessageNotFoundMissing)
		}
		if guarded && msg.modSeq == 0 {
			return c.providerData(string(op), "invalid_protocol")
		}
		before := hasFlag(msg.flags, flag)
		if before == desired {
			result, err := c.storeNoOpResult(op, pre, before, flag, guarded)
			if err != nil {
				return err
			}
			out = result
			return nil
		}
		if selectData.permanentFlagsKnown && !flagListContains(selectData.permanentFlags, flag) {
			requirement := "seen"
			if flag == goimap.FlagFlagged {
				requirement = "flagged"
			}
			return c.unsupported(messageCapability(op), "permanent_flag_unavailable", map[string]any{"requirement": requirement})
		}
		storeOp := goimap.StoreFlagsDel
		if desired {
			storeOp = goimap.StoreFlagsAdd
		}
		var storeOptions *goimap.StoreOptions
		if guarded {
			if msg.modSeq == 0 {
				return c.providerData(string(op), "invalid_protocol")
			}
			storeOptions = &goimap.StoreOptions{UnchangedSince: msg.modSeq}
		}
		attempt, err := c.newAttempt(writeOp)
		if err != nil {
			return err
		}
		obs, err := session.withMutationCommand(ctx, attempt, "UID STORE", func(dep *imapclient.Client) error {
			_, err := dep.Store(goimap.UIDSetNum(goimap.UID(pre.message.uid)), &goimap.StoreFlags{Op: storeOp, Flags: []goimap.Flag{flag}}, storeOptions).Collect()
			return err
		}, func(obs imapMutationObservation, writeErr error) error {
			return c.classifyStoreMutationFailure(attempt, writeOp, op, obs, writeErr, pre.message.uid, guarded)
		})
		if err != nil {
			return err
		}
		if guarded {
			if modified, malformed := observedModified(obs, pre.message.uid); modified || malformed {
				if modified {
					return c.rejectConflict(attempt, writeOp)
				}
				return errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
			}
		}
		verified, err := c.verifyStoredFlag(ctx, session, attempt, op, pre, msg.flags, desired, flag, guarded)
		if err != nil {
			return err
		}
		out = verified
		return nil
	})
	return out, err
}

type imapStoreFetchedMessage struct {
	imapFetchedMessage
	modSeq uint64
}

func (c *accountClient) fetchStoreMessage(ctx context.Context, session *imapSession, folder imapFolderRecord, uid uint32, modSeq bool) (imapStoreFetchedMessage, bool, error) {
	options := &goimap.FetchOptions{UID: true, Flags: true, ModSeq: modSeq}
	buffer, ok, err := c.fetchOne(ctx, session, uid, options, errs.ProviderOperationIMAPMessagesRead)
	if err != nil || !ok {
		return imapStoreFetchedMessage{}, ok, err
	}
	if uint32(buffer.UID) != uid {
		return imapStoreFetchedMessage{}, false, c.providerData(string(mail.MutationMarkRead), "invalid_protocol")
	}
	id, err := formatMessageID(folder.mailbox, folder.uidValidity, uid)
	if err != nil {
		return imapStoreFetchedMessage{}, false, c.providerData(string(mail.MutationMarkRead), "invalid_mailbox_encoding")
	}
	return imapStoreFetchedMessage{
		imapFetchedMessage: imapFetchedMessage{
			account:     c.account,
			provider:    c.provider,
			folder:      mail.SummaryFrom(folder.folder),
			mailbox:     folder.mailbox,
			uidValidity: folder.uidValidity,
			uid:         uid,
			id:          id,
			flags:       append([]goimap.Flag(nil), buffer.Flags...),
		},
		modSeq: buffer.ModSeq,
	}, true, nil
}

func (c *accountClient) selectMailboxReadWrite(ctx context.Context, session *imapSession, mailbox string, condstore bool, cap provider.Capability, operation string) (imapMailboxStatus, error) {
	session.forgetSelected()
	var data *goimap.SelectData
	obs, err := session.withObservedReadCommand(ctx, errs.ProviderOperationIMAPMessagesRead, "SELECT", func(dep *imapclient.Client) error {
		var err error
		data, err = dep.Select(mailbox, &goimap.SelectOptions{CondStore: condstore}).Wait()
		return err
	})
	if err != nil {
		return imapMailboxStatus{}, err
	}
	status, err := c.mailboxStatusFromSelectData(data, obs, errs.ProviderOperationIMAPMessagesRead)
	if err != nil {
		return imapMailboxStatus{}, err
	}
	if status.readWriteKnown && !status.readWrite {
		_ = cap
		return imapMailboxStatus{}, c.providerForbidden(operation, "provider_policy")
	}
	session.rememberSelected(mailbox, false, status)
	return status, nil
}

func (c *accountClient) storeNoOpResult(op mail.MessageMutationOperation, pre imapMessagePreflight, before bool, flag goimap.Flag, guarded bool) (mail.MessageMutationResult, error) {
	meta, err := c.mutationMetadata(&guarded)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	changed := false
	source := mail.SummaryFrom(pre.source.folder)
	change := &mail.BoolChange{Before: &before, After: before}
	out := mail.MessageMutationResult{
		MessageID:         pre.message.id,
		InternetMessageID: pre.message.internetMessageID,
		SourceFolder:      &source,
		Changed:           &changed,
		WriteOutcomeFacts: errs.VerifiedWriteNoOpFacts(),
		ProviderMeta:      meta,
	}
	if flag == goimap.FlagSeen {
		out.ReadState = change
	} else {
		out.FlagState = change
	}
	return out, nil
}

func (c *accountClient) verifyStoredFlag(ctx context.Context, session *imapSession, attempt errs.ProviderWriteAttempt, op mail.MessageMutationOperation, pre imapMessagePreflight, beforeFlags []goimap.Flag, desired bool, flag goimap.Flag, guarded bool) (mail.MessageMutationResult, error) {
	verified, ok, err := c.fetchStoreMessage(ctx, session, pre.source, pre.message.uid, false)
	if err != nil {
		return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
	}
	if !ok || hasFlag(verified.flags, flag) != desired || !preservedFlags(beforeFlags, verified.flags, flag) {
		return mail.MessageMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	meta, err := c.mutationMetadata(&guarded)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	before := hasFlag(beforeFlags, flag)
	changed := true
	source := mail.SummaryFrom(pre.source.folder)
	change := &mail.BoolChange{Before: &before, After: desired}
	out := mail.MessageMutationResult{
		MessageID:         pre.message.id,
		InternetMessageID: pre.message.internetMessageID,
		SourceFolder:      &source,
		Changed:           &changed,
		WriteOutcomeFacts: facts,
		ProviderMeta:      meta,
	}
	if flag == goimap.FlagSeen {
		out.ReadState = change
	} else {
		out.FlagState = change
	}
	_ = op
	return out, nil
}

func preservedFlags(before, after []goimap.Flag, changed goimap.Flag) bool {
	for _, flag := range before {
		if flag == changed {
			continue
		}
		if !flagListContains(after, flag) {
			return false
		}
	}
	return true
}

func flagListContains(flags []goimap.Flag, needle goimap.Flag) bool {
	for _, flag := range flags {
		if strings.EqualFold(string(flag), string(needle)) {
			return true
		}
	}
	return false
}

func (c *accountClient) classifyFolderMutationFailure(attempt errs.ProviderWriteAttempt, writeOp errs.ProviderWriteOperation, op mail.FolderMutationOperation, obs imapMutationObservation, err error, operation string) error {
	if mutationFailureResponseLost(context.Background(), err) {
		return errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	var imapErr *goimap.Error
	if !errors.As(err, &imapErr) {
		if obs.Completed && obs.Status == "OK" {
			return errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
		}
		return classifyUnknownMutationFailure(attempt, context.Background(), err)
	}
	cause := c.folderTaggedRejection(op, imapErr, operation)
	return c.reject(attempt, writeOp, cause)
}

func (c *accountClient) folderTaggedRejection(op mail.FolderMutationOperation, imapErr *goimap.Error, operation string) error {
	status := imapErr.Type
	code := strings.ToUpper(string(imapErr.Code))
	if status == goimap.StatusResponseTypeBad {
		return c.providerData(operation, "invalid_protocol")
	}
	if status != goimap.StatusResponseTypeNo {
		return c.providerData(operation, "invalid_protocol")
	}
	switch code {
	case "ALREADYEXISTS":
		if op == mail.FolderOperationCreate || op == mail.FolderOperationRename {
			return c.invalidFolderName("", "already_exists", "a folder with that name already exists")
		}
	case "NONEXISTENT":
		return c.folderNotFound(operation, "")
	case "HASCHILDREN":
		if op == mail.FolderOperationRename || op == mail.FolderOperationDelete {
			return errs.NewWithDetails(errs.CodeProtectedFolder, "folder is protected", map[string]any{"account": c.account, "provider": c.provider, "reason": "has_children"})
		}
	case "NOPERM", "CANNOT", "LIMIT", "OVERQUOTA":
		return c.providerForbidden(operation, "provider_policy")
	case "":
		if op == mail.FolderOperationDelete {
			return c.providerForbidden(operation, "unknown_forbidden")
		}
		return c.invalidFolderName("", "provider_rejected_name", "the server rejected the folder name")
	default:
		if op == mail.FolderOperationDelete {
			return c.providerForbidden(operation, "unknown_forbidden")
		}
		return c.invalidFolderName("", "provider_rejected_name", "the server rejected the folder name")
	}
	return c.providerData(operation, "invalid_protocol")
}

func (c *accountClient) classifyMessageMutationFailure(attempt errs.ProviderWriteAttempt, writeOp errs.ProviderWriteOperation, op mail.MessageMutationOperation, obs imapMutationObservation, err error, sourceUID uint32) error {
	var imapErr *goimap.Error
	if !errors.As(err, &imapErr) {
		if obs.Completed && obs.Status == "OK" {
			if obs.hasMalformedResponseCode("COPYUID") {
				return errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
			}
			return errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMissing)
		}
		return classifyUnknownMutationFailure(attempt, context.Background(), err)
	}
	if op != mail.MutationCopy && imapErr.Type == goimap.StatusResponseTypeNo {
		return errs.NewIMAPMoveRejectionStateUnknown(attempt)
	}
	if (op == mail.MutationCopy || imapErr.Type == goimap.StatusResponseTypeBad) && obs.hasCredibleCopyUID(sourceUID) {
		return errs.NewIMAPContradictoryMutationStateUnknown(attempt)
	}
	cause := c.copyStoreTaggedRejection(writeOp, op, imapErr, "")
	return c.reject(attempt, writeOp, cause)
}

func (c *accountClient) classifyStoreMutationFailure(attempt errs.ProviderWriteAttempt, writeOp errs.ProviderWriteOperation, op mail.MessageMutationOperation, obs imapMutationObservation, err error, sourceUID uint32, guarded bool) error {
	var imapErr *goimap.Error
	if !errors.As(err, &imapErr) {
		if obs.Completed && obs.Status == "OK" {
			return errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
		}
		return classifyUnknownMutationFailure(attempt, context.Background(), err)
	}
	if guarded && strings.EqualFold(string(imapErr.Code), "MODIFIED") {
		modified, malformed := observedModified(obs, sourceUID)
		if modified {
			return c.rejectConflict(attempt, writeOp)
		}
		if malformed {
			return c.reject(attempt, writeOp, c.providerData(string(op), "invalid_protocol"))
		}
		return c.reject(attempt, writeOp, c.providerData(string(op), "invalid_protocol"))
	}
	cause := c.copyStoreTaggedRejection(writeOp, op, imapErr, "")
	return c.reject(attempt, writeOp, cause)
}

func (c *accountClient) copyStoreTaggedRejection(writeOp errs.ProviderWriteOperation, op mail.MessageMutationOperation, imapErr *goimap.Error, targetID string) error {
	status := imapErr.Type
	code := strings.ToUpper(string(imapErr.Code))
	if status == goimap.StatusResponseTypeBad {
		return c.providerData(string(op), "invalid_protocol")
	}
	if status != goimap.StatusResponseTypeNo {
		return c.providerData(string(op), "invalid_protocol")
	}
	switch code {
	case "TRYCREATE", "NONEXISTENT":
		if op == mail.MutationCopy {
			return c.folderNotFound(string(op), targetID)
		}
	case "NOPERM", "CANNOT", "LIMIT", "OVERQUOTA":
		return c.providerForbidden(string(op), "provider_policy")
	case "":
		return c.providerForbidden(string(op), "unknown_forbidden")
	default:
		return c.providerForbidden(string(op), "unknown_forbidden")
	}
	_ = writeOp
	return c.providerData(string(op), "invalid_protocol")
}

func (o imapMutationObservation) hasCredibleCopyUID(sourceUID uint32) bool {
	for _, code := range append(append([]imapObservedResponseCode(nil), o.Tagged...), o.Untagged...) {
		if code.Name != "COPYUID" || code.Malformed || len(code.Args) != 3 {
			continue
		}
		if _, err := parseCanonicalUint32(code.Args[0]); err != nil {
			continue
		}
		src, err := ParseProviderUIDSet(code.Args[1], 1, math.MaxUint32, 1)
		if err != nil {
			continue
		}
		srcUID, ok := src.ExactSingleton()
		if !ok || srcUID != sourceUID {
			continue
		}
		dst, err := ParseProviderUIDSet(code.Args[2], 1, math.MaxUint32, 1)
		if err != nil {
			continue
		}
		if _, ok := dst.ExactSingleton(); ok {
			return true
		}
	}
	return false
}

func (o imapMutationObservation) hasMalformedResponseCode(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	for _, code := range append(append([]imapObservedResponseCode(nil), o.Tagged...), o.Untagged...) {
		if code.Name == name && code.Malformed {
			return true
		}
	}
	return false
}

func observedModified(obs imapMutationObservation, uid uint32) (exact bool, malformed bool) {
	count := 0
	for _, code := range obs.Tagged {
		if code.Name != "MODIFIED" {
			continue
		}
		count++
		if code.Malformed || len(code.Args) != 1 {
			malformed = true
			continue
		}
		set, err := ParseProviderUIDSet(code.Args[0], 1, math.MaxUint32, 1)
		if err != nil {
			malformed = true
			continue
		}
		got, ok := set.ExactSingleton()
		if !ok || got != uid {
			malformed = true
			continue
		}
		exact = true
	}
	if count > 1 {
		return false, true
	}
	return exact, malformed
}

type imapSelectedFacts struct {
	uidNotSticky        bool
	readWriteKnown      bool
	readWrite           bool
	permanentFlagsKnown bool
	permanentFlags      []goimap.Flag
	highestModSeqKnown  bool
	highestModSeq       uint64
	noModSeq            bool
	malformed           bool
}

func selectedMailboxFacts(obs imapMutationObservation) imapSelectedFacts {
	var facts imapSelectedFacts
	counts := map[string]int{}
	for _, code := range observedResponseCodes(obs) {
		switch code.Name {
		case "UIDNOTSTICKY":
			counts[code.Name]++
			if code.Malformed || len(code.Args) != 0 {
				facts.malformed = true
			}
			facts.uidNotSticky = true
		case "READ-WRITE":
			counts[code.Name]++
			if code.Malformed || len(code.Args) != 0 {
				facts.malformed = true
			}
			facts.readWriteKnown = true
			facts.readWrite = true
		case "READ-ONLY":
			counts[code.Name]++
			if code.Malformed || len(code.Args) != 0 {
				facts.malformed = true
			}
			facts.readWriteKnown = true
			facts.readWrite = false
		case "PERMANENTFLAGS":
			counts[code.Name]++
			flags, ok := parsePermanentFlagsCode(code)
			if code.Malformed || !ok {
				facts.malformed = true
			}
			facts.permanentFlagsKnown = true
			facts.permanentFlags = flags
		case "HIGHESTMODSEQ":
			counts[code.Name]++
			if code.Malformed || len(code.Args) != 1 {
				facts.malformed = true
				continue
			}
			value, ok := parseCanonicalUint64Token(code.Args[0])
			if !ok || value == 0 {
				facts.malformed = true
				continue
			}
			facts.highestModSeqKnown = true
			facts.highestModSeq = value
		case "NOMODSEQ":
			counts[code.Name]++
			if code.Malformed || len(code.Args) != 0 {
				facts.malformed = true
			}
			facts.noModSeq = true
		}
	}
	for _, name := range []string{"UIDNOTSTICKY", "PERMANENTFLAGS", "HIGHESTMODSEQ", "NOMODSEQ"} {
		if counts[name] > 1 {
			facts.malformed = true
		}
	}
	if counts["READ-WRITE"]+counts["READ-ONLY"] > 1 || (facts.highestModSeqKnown && facts.noModSeq) {
		facts.malformed = true
	}
	return facts
}

func observedResponseCodes(obs imapMutationObservation) []imapObservedResponseCode {
	out := make([]imapObservedResponseCode, 0, len(obs.Tagged)+len(obs.Untagged))
	out = append(out, obs.Tagged...)
	out = append(out, obs.Untagged...)
	return out
}

func parsePermanentFlagsCode(code imapObservedResponseCode) ([]goimap.Flag, bool) {
	if code.Name != "PERMANENTFLAGS" {
		return nil, false
	}
	raw := strings.TrimSpace(code.Raw)
	if len(raw) < len("PERMANENTFLAGS") || !strings.EqualFold(raw[:len("PERMANENTFLAGS")], "PERMANENTFLAGS") {
		return nil, false
	}
	rest := strings.TrimSpace(raw[len("PERMANENTFLAGS"):])
	if !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return nil, false
	}
	inner := strings.TrimSpace(rest[1 : len(rest)-1])
	if inner == "" {
		return []goimap.Flag{}, true
	}
	fields := strings.Fields(inner)
	flags := make([]goimap.Flag, 0, len(fields))
	for _, field := range fields {
		if field == "" || strings.ContainsAny(field, "\"{}[]()") {
			return nil, false
		}
		flags = append(flags, goimap.Flag(field))
	}
	return flags, true
}

func parseCanonicalUint64Token(raw string) (uint64, bool) {
	if raw == "" || raw == "0" || (len(raw) > 1 && raw[0] == '0') {
		return 0, false
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || strconv.FormatUint(value, 10) != raw {
		return 0, false
	}
	return value, true
}

func (c *accountClient) requireSpecialUseAuthority(snapshot imapFolderSnapshot, cap provider.Capability, operation string) error {
	if snapshot.specialUseAuthority != specialUseUnavailable {
		return nil
	}
	return c.unsupported(cap, "special_use_authority_unavailable", map[string]any{"operation": operation})
}

func (c *accountClient) newAttempt(op errs.ProviderWriteOperation) (errs.ProviderWriteAttempt, error) {
	return errs.NewProviderWriteAttempt(c.account, imapErrProvider(c.provider), op)
}

func (c *accountClient) reject(attempt errs.ProviderWriteAttempt, op errs.ProviderWriteOperation, cause error) error {
	marked := errs.NewDefinitiveProviderRejectionError(c.account, imapErrProvider(c.provider), op, cause)
	rejection, ok := errs.AsDefinitiveProviderRejection(marked)
	if !ok {
		return marked
	}
	return errs.NewWriteRejected(attempt, rejection)
}

func (c *accountClient) rejectConflict(attempt errs.ProviderWriteAttempt, op errs.ProviderWriteOperation) error {
	cause := errs.NewWithDetails(errs.CodeProviderConflict, "the message changed on the server while maildo was changing it", map[string]any{
		"account":  c.account,
		"provider": c.provider,
		"reason":   "concurrent_modification",
	})
	return c.reject(attempt, op, cause)
}

func (c *accountClient) folderMetadata(use mail.ProviderMetadataUse) (*mail.FolderProviderMetadata, error) {
	return mail.NewProviderMetadata(imapErrProvider(c.provider), use, mail.FolderProviderMetadata{Source: mail.MetadataSourceIMAP})
}

func (c *accountClient) mutationMetadata(guarded *bool) (*mail.MutationProviderMetadata, error) {
	return mail.NewProviderMetadata(imapErrProvider(c.provider), mail.MetadataMessageMutation, mail.MutationProviderMetadata{Source: mail.MetadataSourceIMAP, ConcurrencyGuarded: guarded})
}

func (c *accountClient) unsupported(cap provider.Capability, reason string, extra map[string]any) error {
	details := map[string]any{
		"account":    c.account,
		"provider":   c.provider,
		"capability": string(cap),
		"reason":     reason,
	}
	for k, v := range extra {
		details[k] = v
	}
	return errs.NewWithDetails(errs.CodeUnsupportedCapability, "provider capability is unavailable", details)
}

func (c *accountClient) invalidFolderName(name, reason, message string) error {
	details := map[string]any{"field": "display_name", "reason": reason}
	if strings.TrimSpace(name) != "" {
		details["display_name"] = strings.TrimSpace(name)
	}
	return errs.NewWithDetails(errs.CodeInvalidFolderName, message, details)
}

func (c *accountClient) folderNotFound(operation, id string) error {
	details := map[string]any{"account": c.account, "provider": c.provider, "operation": operation}
	if id != "" {
		details["folder_id"] = id
	}
	return errs.NewWithDetails(errs.CodeFolderNotFound, "folder not found", details)
}

func (c *accountClient) protectedFolder(folder mail.Folder, reason string) error {
	if reason == "" {
		reason = "protected_folder"
	}
	return errs.NewWithDetails(errs.CodeProtectedFolder, "folder is protected", map[string]any{
		"account":         c.account,
		"provider":        c.provider,
		"folder_id":       folder.ID,
		"display_name":    folder.DisplayName,
		"path":            folder.Path,
		"well_known_name": folder.WellKnownName,
		"reason":          reason,
	})
}

func (c *accountClient) providerForbidden(operation, reason string) error {
	return errs.NewWithDetails(errs.CodeProviderForbidden, "provider rejected the operation", map[string]any{
		"account":   c.account,
		"provider":  c.provider,
		"operation": operation,
		"reason":    reason,
	})
}

func (c *accountClient) providerData(operation, reason string) error {
	return errs.NewWithDetails(errs.CodeProviderDataError, "provider returned invalid protocol data", map[string]any{
		"account":   c.account,
		"provider":  c.provider,
		"operation": operation,
		"reason":    reason,
	})
}

func messageCapability(op mail.MessageMutationOperation) provider.Capability {
	switch op {
	case mail.MutationMove:
		return provider.CapMessagesMove
	case mail.MutationCopy:
		return provider.CapMessagesCopy
	case mail.MutationArchive:
		return provider.CapMessagesArchive
	case mail.MutationDelete:
		return provider.CapMessagesDelete
	case mail.MutationRestore:
		return provider.CapMessagesRestore
	case mail.MutationJunk:
		return provider.CapMessagesJunk
	case mail.MutationMarkRead, mail.MutationMarkUnread:
		return provider.CapMessagesMarkRead
	case mail.MutationFlagSet, mail.MutationFlagClear:
		return provider.CapMessagesFlag
	default:
		return ""
	}
}
