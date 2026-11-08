package gmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
)

const (
	gmailLabelResultFields       = "id,name,type,labelListVisibility,messageListVisibility,color(textColor,backgroundColor)"
	gmailLabelDirectFields       = "id,name,type,messagesTotal,messagesUnread,labelListVisibility,messageListVisibility,color(textColor,backgroundColor)"
	gmailLabelDeleteProofFields  = "id"
	gmailMessageLabelReadFields  = "id,labelIds"
	gmailMessageWriteFields      = "id,labelIds"
	gmailModifyChunkSize         = 100
	gmailFolderCountsSource      = mail.FolderCountsSourceRefreshed
	gmailSystemUnsupportedReason = "system_destination_unsupported"
)

type gmailMutationPhase string

const (
	gmailPhaseModify  gmailMutationPhase = "modify_chunk"
	gmailPhaseTrash   gmailMutationPhase = "trash"
	gmailPhaseUntrash gmailMutationPhase = "untrash"
)

type gmailMessageLabelSnapshot struct {
	ID       string
	LabelIDs []string
}

type gmailMessageMutationPlan struct {
	Operation      mail.MessageMutationOperation
	WriteOperation errs.ProviderWriteOperation
	KnownOperation errs.KnownPartialOperation
	MessageID      string
	Initial        []string
	Final          []string
	TargetFolder   *mail.FolderSummary
	ReadState      *mail.BoolChange
	FlagState      *mail.BoolChange
	Categories     *mail.CategoryChange
	Phases         []gmailPlannedPhase
}

type gmailPlannedPhase struct {
	Kind     gmailMutationPhase
	Add      []string
	Remove   []string
	Expected []string
}

func (c *Client) PrepareFolderRename(ctx context.Context, req mail.FolderRenamePreflightRequest) (mail.FolderMutationPreflight, error) {
	snap, err := c.identityLabels(ctx)
	if err != nil {
		return mail.FolderMutationPreflight{}, err
	}
	folder, label, err := c.userLabelFromFolder(snap, req.Target)
	if err != nil {
		if isProtectedFolderError(err) {
			return mail.FolderMutationPreflight{
				Operation:        mail.FolderOperationRename,
				Target:           mail.SummaryFrom(req.Target),
				Protected:        true,
				ProtectionReason: "provider_managed_folder",
				ProviderMeta:     gmailFolderMutationMeta(mail.MetadataFolderMutationPreflight, false),
			}, nil
		}
		return mail.FolderMutationPreflight{}, err
	}
	if _, err := c.directUserLabel(ctx, label.ID, label.Name); err != nil {
		return mail.FolderMutationPreflight{}, err
	}
	return mail.FolderMutationPreflight{
		Operation:    mail.FolderOperationRename,
		Target:       mail.SummaryFrom(folder),
		ProviderMeta: gmailFolderMutationMeta(mail.MetadataFolderMutationPreflight, false),
	}, nil
}

func (c *Client) PrepareFolderDelete(ctx context.Context, req mail.FolderDeletePreflightRequest) (mail.FolderMutationPreflight, error) {
	snap, err := c.identityLabels(ctx)
	if err != nil {
		return mail.FolderMutationPreflight{}, err
	}
	folder, label, err := c.userLabelFromFolder(snap, req.Target)
	if err != nil {
		if isProtectedFolderError(err) {
			return mail.FolderMutationPreflight{
				Operation:        mail.FolderOperationDelete,
				Target:           mail.SummaryFrom(req.Target),
				Protected:        true,
				ProtectionReason: "provider_managed_folder",
				ProviderMeta:     gmailFolderMutationMeta(mail.MetadataFolderMutationPreflight, true),
			}, nil
		}
		return mail.FolderMutationPreflight{}, err
	}
	detail, err := c.directUserLabel(ctx, label.ID, label.Name)
	if err != nil {
		return mail.FolderMutationPreflight{}, err
	}
	if detail.MessagesTotal == nil {
		return mail.FolderMutationPreflight{}, providerDataError(c.account, "invalid_protocol")
	}
	summary := mail.SummaryFrom(folder)
	summary.DisplayName = displayNameForRawUserLabel(detail.Name)
	summary.Path = detail.Name
	descendants := gmailDescendantFacts(label.Name, snap)
	return mail.FolderMutationPreflight{
		Operation:              mail.FolderOperationDelete,
		Target:                 summary,
		TotalItemCount:         *detail.MessagesTotal,
		TotalItemCountKnown:    true,
		ChildFolderCount:       gmailDirectChildCount(label.Name, snap),
		ChildFolderKnown:       true,
		CountsSource:           gmailFolderCountsSource,
		DescendantFacts:        descendants,
		DescendantFactsPresent: true,
		ProviderMeta:           gmailFolderMutationMeta(mail.MetadataFolderMutationPreflight, true),
	}, nil
}

func (c *Client) CreateFolder(ctx context.Context, req mail.FolderCreateRequest) (mail.FolderMutationResult, error) {
	snap, err := c.identityLabels(ctx)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	candidate := req.DisplayName
	var parent *mail.FolderSummary
	if req.Parent != nil {
		parentFolder, label, err := c.userLabelSummaryFromID(snap, req.Parent.ID)
		if err != nil {
			return mail.FolderMutationResult{}, err
		}
		candidate = label.Name + "/" + req.DisplayName
		summary := mail.SummaryFrom(parentFolder)
		parent = &summary
	}
	if err := rejectLabelNameCollision(snap, candidate, ""); err != nil {
		return mail.FolderMutationResult{}, err
	}
	if len(snap.labels) >= limits.GmailMaxLabels {
		return mail.FolderMutationResult{}, errs.NewResourceLimitExceeded(c.account, errs.ProviderGmail, errs.ResourceLimitLabels, uint64(limits.GmailMaxLabels+1))
	}
	body, err := json.Marshal(struct {
		Name string `json:"name"`
	}{Name: candidate})
	if err != nil {
		return mail.FolderMutationResult{}, providerAPIError(c.account, "/labels")
	}
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, errs.WriteGmailLabelCreate)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	values := url.Values{"fields": {gmailLabelResultFields}}
	status, data, err := c.gmailMutationWriteRequest(ctx, http.MethodPost, "/labels", values, body, "application/json", attempt, limits.GmailMaxJSONResponseBytes)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	if status < 200 || status >= 300 {
		return mail.FolderMutationResult{}, c.gmailWriteStatusError(attempt, errs.WriteGmailLabelCreate, "/labels", status, data)
	}
	var created gmailLabelDetail
	if err := decodeJSONResponse(c.account, data, &created); err != nil {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	if created.ID == "" {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMissing)
	}
	// Gmail's create answer has no type field (seen live on 2026-11-08); the
	// direct read below checks that the label is a user label.
	if created.Name != candidate || (created.Type != "" && created.Type != "user") {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	direct, err := c.directUserLabel(ctx, created.ID, candidate)
	if err != nil {
		return mail.FolderMutationResult{}, c.labelVerificationUnknown(attempt, err)
	}
	if direct.Type != "user" || direct.Name != candidate {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	folder, err := c.overlayCreatedLabel(created.ID, candidate, direct)
	if err != nil {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	createdSummary := mail.SummaryFrom(folder)
	return mail.FolderMutationResult{
		Operation:          mail.FolderOperationCreate,
		ParentFolder:       parent,
		CreatedFolder:      &createdSummary,
		NewDisplayName:     folder.DisplayName,
		VerificationStatus: mail.FolderVerificationProviderResponse,
		WriteOutcomeFacts:  mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)),
		ProviderMeta:       gmailFolderMutationMeta(mail.MetadataFolderMutationResult, false),
	}, nil
}

func (c *Client) RenameFolder(ctx context.Context, req mail.FolderRenameRequest) (mail.FolderMutationResult, error) {
	snap, err := c.identityLabels(ctx)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	targetFolder, label, err := c.userLabelFromFolder(snap, req.Target)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	direct, err := c.directUserLabel(ctx, label.ID, label.Name)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	candidate := renameCandidate(label.Name, req.NewDisplayName)
	if err := rejectLabelNameCollision(snap, candidate, label.ID); err != nil {
		return mail.FolderMutationResult{}, err
	}
	targetSummary := mail.SummaryFrom(targetFolder)
	if candidate == label.Name {
		facts := errs.VerifiedWriteNoOpFacts()
		return mail.FolderMutationResult{
			Operation:          mail.FolderOperationRename,
			TargetFolder:       &targetSummary,
			RenamedFolder:      &targetSummary,
			OldDisplayName:     targetFolder.DisplayName,
			NewDisplayName:     targetFolder.DisplayName,
			VerificationStatus: mail.FolderVerificationProviderResponse,
			WriteOutcomeFacts:  mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)),
			ProviderMeta:       gmailFolderMutationMeta(mail.MetadataFolderMutationResult, false),
		}, nil
	}
	body, err := json.Marshal(struct {
		Name string `json:"name"`
	}{Name: candidate})
	if err != nil {
		return mail.FolderMutationResult{}, providerAPIError(c.account, "/labels/"+url.PathEscape(label.ID))
	}
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, errs.WriteGmailLabelRename)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	path := "/labels/" + url.PathEscape(label.ID)
	values := url.Values{"fields": {gmailLabelResultFields}}
	status, data, err := c.gmailMutationWriteRequest(ctx, http.MethodPatch, path, values, body, "application/json", attempt, limits.GmailMaxJSONResponseBytes)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	if status < 200 || status >= 300 {
		return mail.FolderMutationResult{}, c.gmailWriteStatusError(attempt, errs.WriteGmailLabelRename, path, status, data)
	}
	var patched gmailLabelDetail
	if err := decodeJSONResponse(c.account, data, &patched); err != nil {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	if patched.ID == "" {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMissing)
	}
	if patched.ID != label.ID || patched.Name != candidate || patched.Type != "user" {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	verified, err := c.directUserLabel(ctx, label.ID, candidate)
	if err != nil {
		return mail.FolderMutationResult{}, c.labelVerificationUnknown(attempt, err)
	}
	if !labelSettingsEqual(direct, verified) {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	renamed, err := c.overlayRenamedLabel(label.ID, candidate, verified)
	if err != nil {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	renamedSummary := mail.SummaryFrom(renamed)
	return mail.FolderMutationResult{
		Operation:          mail.FolderOperationRename,
		TargetFolder:       &targetSummary,
		RenamedFolder:      &renamedSummary,
		OldDisplayName:     targetFolder.DisplayName,
		NewDisplayName:     renamed.DisplayName,
		VerificationStatus: mail.FolderVerificationProviderResponse,
		WriteOutcomeFacts:  mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)),
		ProviderMeta:       gmailFolderMutationMeta(mail.MetadataFolderMutationResult, false),
	}, nil
}

func (c *Client) DeleteFolder(ctx context.Context, req mail.FolderDeleteRequest) (mail.FolderMutationResult, error) {
	snap, err := c.identityLabels(ctx)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	targetFolder, label, err := c.userLabelFromFolder(snap, req.Target)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	if _, err := c.directUserLabel(ctx, label.ID, label.Name); err != nil {
		return mail.FolderMutationResult{}, err
	}
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, errs.WriteGmailLabelDelete)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	path := "/labels/" + url.PathEscape(label.ID)
	status, data, err := c.gmailMutationWriteRequest(ctx, http.MethodDelete, path, nil, nil, "", attempt, limits.GmailMaxJSONResponseBytes)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	if status < 200 || status >= 300 {
		return mail.FolderMutationResult{}, c.gmailWriteStatusError(attempt, errs.WriteGmailLabelDelete, path, status, data)
	}
	if status == http.StatusNoContent {
		if len(data) != 0 {
			return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
		}
	} else if !bytes.Equal(bytes.TrimSpace(data), []byte("{}")) {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	if err := c.verifyLabelAbsent(ctx, label.ID); err != nil {
		return mail.FolderMutationResult{}, errToDeleteVerificationUnknown(attempt, err)
	}
	if err := c.overlayDeletedLabel(label.ID); err != nil {
		return mail.FolderMutationResult{}, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	facts, err := errs.VerifiedWriteSuccessFacts(attempt)
	if err != nil {
		return mail.FolderMutationResult{}, err
	}
	target := mail.SummaryFrom(targetFolder)
	return mail.FolderMutationResult{
		Operation:              mail.FolderOperationDelete,
		TargetFolder:           &target,
		DeletedFolder:          &target,
		TotalItemCount:         req.Preflight.TotalItemCount,
		TotalItemCountKnown:    req.Preflight.TotalItemCountKnown,
		ChildFolderCount:       req.Preflight.ChildFolderCount,
		ChildFolderKnown:       req.Preflight.ChildFolderKnown,
		CountsSource:           req.Preflight.CountsSource,
		DescendantFacts:        req.Preflight.DescendantFacts,
		DescendantFactsPresent: req.Preflight.DescendantFactsPresent,
		VerificationStatus:     mail.FolderVerificationProviderResponse,
		WriteOutcomeFacts:      mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)),
		ProviderMeta:           gmailFolderMutationMeta(mail.MetadataFolderMutationResult, true),
	}, nil
}

func (c *Client) MoveMessage(ctx context.Context, req mail.MessageMoveRequest) (mail.MessageMutationResult, error) {
	return c.executeFolderMessageMutation(ctx, mail.MutationMove, req.MessageID, req.Folder)
}

func (c *Client) CopyMessage(ctx context.Context, req mail.MessageCopyRequest) (mail.MessageMutationResult, error) {
	return c.executeFolderMessageMutation(ctx, mail.MutationCopy, req.MessageID, req.Folder)
}

func (c *Client) ArchiveMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	snap, msg, err := c.preMessageMutation(ctx, req.MessageID)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	final := removeLabels(msg.LabelIDs, "INBOX")
	plan := c.baseMessagePlan(mail.MutationArchive, errs.WriteGmailArchive, errs.KnownGmailArchive, req.MessageID, msg.LabelIDs, final)
	plan.Phases = modifyPhases(msg.LabelIDs, final)
	return c.executeMessagePlan(ctx, snap, plan)
}

func (c *Client) DeleteMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	snap, msg, err := c.preMessageMutation(ctx, req.MessageID)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	final := addLabels(removeLabels(msg.LabelIDs, "INBOX", "SPAM"), "TRASH")
	plan := c.baseMessagePlan(mail.MutationDelete, errs.WriteGmailDelete, errs.KnownGmailDelete, req.MessageID, msg.LabelIDs, final)
	if !sameStringSet(msg.LabelIDs, final) {
		plan.Phases = []gmailPlannedPhase{{Kind: gmailPhaseTrash, Expected: final}}
	}
	if trash, ok := snap.byID["TRASH"]; ok {
		target := mail.SummaryFrom(trash)
		plan.TargetFolder = &target
	}
	return c.executeMessagePlan(ctx, snap, plan)
}

func (c *Client) RestoreMessage(ctx context.Context, req mail.MessageRestoreRequest) (mail.MessageMutationResult, error) {
	return c.executeFolderMessageMutation(ctx, mail.MutationRestore, req.MessageID, req.Folder)
}

func (c *Client) JunkMessage(ctx context.Context, req mail.MessageMutationRequest) (mail.MessageMutationResult, error) {
	snap, msg, err := c.preMessageMutation(ctx, req.MessageID)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	final := addLabels(removeLabels(msg.LabelIDs, "INBOX", "TRASH"), "SPAM")
	plan := c.baseMessagePlan(mail.MutationJunk, errs.WriteGmailJunk, errs.KnownGmailJunk, req.MessageID, msg.LabelIDs, final)
	plan.Phases = c.phasesForMaybeUntrash(msg.LabelIDs, final)
	if spam, ok := snap.byID["SPAM"]; ok {
		target := mail.SummaryFrom(spam)
		plan.TargetFolder = &target
	}
	return c.executeMessagePlan(ctx, snap, plan)
}

func (c *Client) SetMessageReadState(ctx context.Context, req mail.MessageReadStateRequest) (mail.MessageMutationResult, error) {
	snap, msg, err := c.preMessageMutation(ctx, req.MessageID)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	final := msg.LabelIDs
	if req.IsRead {
		final = removeLabels(final, "UNREAD")
	} else {
		final = addLabels(final, "UNREAD")
	}
	op := mail.MutationMarkUnread
	writeOp := errs.WriteGmailMarkUnread
	knownOp := errs.KnownGmailMarkUnread
	if req.IsRead {
		op = mail.MutationMarkRead
		writeOp = errs.WriteGmailMarkRead
		knownOp = errs.KnownGmailMarkRead
	}
	before := hasLabel(msg.LabelIDs, "UNREAD") == false
	plan := c.baseMessagePlan(op, writeOp, knownOp, req.MessageID, msg.LabelIDs, final)
	plan.ReadState = &mail.BoolChange{Before: &before, After: req.IsRead}
	plan.Phases = modifyPhases(msg.LabelIDs, final)
	return c.executeMessagePlan(ctx, snap, plan)
}

func (c *Client) SetMessageFlag(ctx context.Context, req mail.MessageFlagRequest) (mail.MessageMutationResult, error) {
	snap, msg, err := c.preMessageMutation(ctx, req.MessageID)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	final := msg.LabelIDs
	if req.Flagged {
		final = addLabels(final, "STARRED")
	} else {
		final = removeLabels(final, "STARRED")
	}
	op := mail.MutationFlagClear
	writeOp := errs.WriteGmailFlagClear
	knownOp := errs.KnownGmailFlagClear
	if req.Flagged {
		op = mail.MutationFlagSet
		writeOp = errs.WriteGmailFlagSet
		knownOp = errs.KnownGmailFlagSet
	}
	before := hasLabel(msg.LabelIDs, "STARRED")
	plan := c.baseMessagePlan(op, writeOp, knownOp, req.MessageID, msg.LabelIDs, final)
	plan.FlagState = &mail.BoolChange{Before: &before, After: req.Flagged}
	plan.Phases = modifyPhases(msg.LabelIDs, final)
	return c.executeMessagePlan(ctx, snap, plan)
}

func (c *Client) ListMessageCategories(ctx context.Context, req mail.MessageCategoryListRequest) (mail.MessageCategoryListResult, error) {
	snap, msg, err := c.preMessageMutation(ctx, req.MessageID)
	if err != nil {
		return mail.MessageCategoryListResult{}, err
	}
	categories := attachedUserLabelNames(msg.LabelIDs, snap)
	return mail.MessageCategoryListResult{
		MessageID:    msg.ID,
		Categories:   categories,
		Count:        len(categories),
		ProviderMeta: gmailMutationMeta(msg.LabelIDs),
	}, nil
}

func (c *Client) SetMessageCategories(ctx context.Context, req mail.MessageCategorySetRequest) (mail.MessageMutationResult, error) {
	snap, msg, err := c.preMessageMutation(ctx, req.MessageID)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	before := attachedUserLabelNames(msg.LabelIDs, snap)
	final, change, err := c.categoryFinalSet(req, msg.LabelIDs, before, snap)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	writeOp := errs.WriteGmailCategoriesSet
	knownOp := errs.KnownGmailCategoriesSet
	op := mail.MutationCategoriesSet
	if req.Mode != mail.CategoryMutationSet {
		writeOp = errs.WriteGmailCategoriesClear
		knownOp = errs.KnownGmailCategoriesClear
		op = mail.MutationCategoriesClear
	}
	plan := c.baseMessagePlan(op, writeOp, knownOp, req.MessageID, msg.LabelIDs, final)
	plan.Categories = change
	plan.Phases = modifyPhases(msg.LabelIDs, final)
	return c.executeMessagePlan(ctx, snap, plan)
}

func (c *Client) executeFolderMessageMutation(ctx context.Context, op mail.MessageMutationOperation, messageID string, folder mail.Folder) (mail.MessageMutationResult, error) {
	snap, msg, err := c.preMessageMutation(ctx, messageID)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	destLabel, err := c.destinationLabel(snap, folder)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	final, phases, err := destinationFinalSet(op, msg.LabelIDs, destLabel)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	writeOp, knownOp := writeOperationForMutation(op)
	plan := c.baseMessagePlan(op, writeOp, knownOp, messageID, msg.LabelIDs, final)
	target := mail.SummaryFrom(folder)
	plan.TargetFolder = &target
	if phases != nil {
		plan.Phases = phases
	} else if op == mail.MutationCopy {
		plan.Phases = modifyPhases(msg.LabelIDs, final)
	} else {
		plan.Phases = c.phasesForMaybeUntrash(msg.LabelIDs, final)
	}
	return c.executeMessagePlan(ctx, snap, plan)
}

func (c *Client) preMessageMutation(ctx context.Context, messageID string) (labelSnapshot, gmailMessageLabelSnapshot, error) {
	snap, err := c.identityLabels(ctx)
	if err != nil {
		return labelSnapshot{}, gmailMessageLabelSnapshot{}, err
	}
	msg, err := c.readMessageLabels(ctx, messageID, snap)
	if err != nil {
		return labelSnapshot{}, gmailMessageLabelSnapshot{}, err
	}
	return snap, msg, nil
}

func (c *Client) baseMessagePlan(op mail.MessageMutationOperation, writeOp errs.ProviderWriteOperation, knownOp errs.KnownPartialOperation, messageID string, initial, final []string) gmailMessageMutationPlan {
	return gmailMessageMutationPlan{
		Operation:      op,
		WriteOperation: writeOp,
		KnownOperation: knownOp,
		MessageID:      messageID,
		Initial:        canonicalLabelIDs(initial),
		Final:          canonicalLabelIDs(final),
	}
}

func (c *Client) executeMessagePlan(ctx context.Context, snap labelSnapshot, plan gmailMessageMutationPlan) (mail.MessageMutationResult, error) {
	if sameStringSet(plan.Initial, plan.Final) || len(plan.Phases) == 0 {
		changed := false
		return c.messageMutationResult(plan, errs.VerifiedWriteNoOpFacts(), changed), nil
	}
	attempt, err := errs.NewProviderWriteAttempt(c.account, errs.ProviderGmail, plan.WriteOperation)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	facts, err := c.executeMessagePhases(ctx, snap, attempt, plan)
	if err != nil {
		return mail.MessageMutationResult{}, err
	}
	changed := true
	return c.messageMutationResult(plan, facts, changed), nil
}

func (c *Client) executeMessagePhases(ctx context.Context, snap labelSnapshot, attempt errs.ProviderWriteAttempt, plan gmailMessageMutationPlan) (*mail.WriteOutcomeFacts, error) {
	current := canonicalLabelIDs(plan.Initial)
	lastVerified := current
	prefix := uint16(0)
	var partialPlan errs.KnownPartialPlan
	phaseIndex := 0
	for phaseIndex < len(plan.Phases) {
		phase := plan.Phases[phaseIndex]
		if err := ctx.Err(); err != nil {
			if prefix > 0 && partialPlan != nil {
				if crossErr := errs.CrossProviderWriteTransmissionBoundary(attempt); crossErr != nil {
					return nil, crossErr
				}
				return nil, errs.NewKnownPartialBeforeNextPhase(attempt, err, partialPlan, prefix, "")
			}
			return nil, c.writeNotApplied(attempt, err)
		}
		status, data, err := c.executeMessagePhaseRequest(ctx, attempt, plan.MessageID, phase)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			if prefix > 0 && partialPlan != nil {
				return nil, c.knownPartialFromStatus(ctx, snap, attempt, partialPlan, prefix, plan, lastVerified, status, data)
			}
			return nil, c.gmailWriteStatusError(attempt, plan.WriteOperation, "/messages/"+url.PathEscape(plan.MessageID), status, data)
		}
		if reason := c.validateMessageWriteResponse(data, plan.MessageID, snap); reason != "" {
			return nil, errs.NewOutcomeUnknown(attempt, reason)
		}
		verified, err := c.readMessageLabels(ctx, plan.MessageID, snap)
		if err != nil {
			return nil, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
		}
		switch phase.Kind {
		case gmailPhaseUntrash:
			if hasLabel(verified.LabelIDs, "TRASH") {
				return nil, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
			}
			current = canonicalLabelIDs(verified.LabelIDs)
			remaining := freezeModifyPhases(current, plan.Final)
			if len(remaining) == 0 {
				if !sameStringSet(current, plan.Final) {
					return nil, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
				}
				facts, err := errs.VerifiedWriteSuccessFacts(attempt)
				return mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)), err
			}
			plan.Phases = append([]gmailPlannedPhase{{Kind: gmailPhaseUntrash}}, remaining...)
			var err error
			partialPlan, err = knownPartialPlan(plan)
			if err != nil {
				return nil, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
			}
		default:
			if !sameStringSet(verified.LabelIDs, phase.Expected) {
				return nil, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
			}
			current = canonicalLabelIDs(verified.LabelIDs)
			if len(plan.Phases) > 1 && partialPlan == nil {
				var err error
				partialPlan, err = knownPartialPlan(plan)
				if err != nil {
					return nil, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
				}
			}
		}
		lastVerified = current
		terminal := phaseIndex == len(plan.Phases)-1
		if terminal {
			if !sameStringSet(current, plan.Final) {
				return nil, errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
			}
			facts, err := errs.VerifiedWriteSuccessFacts(attempt)
			return mail.CloneWriteOutcomeFacts((*mail.WriteOutcomeFacts)(facts)), err
		}
		prefix++
		next, err := errs.AdvanceVerifiedWritePhase(attempt, partialPlan, prefix)
		if err != nil {
			return nil, err
		}
		attempt = next
		phaseIndex++
	}
	return nil, errs.NewSharedProviderContractInvalid()
}

func (c *Client) executeMessagePhaseRequest(ctx context.Context, attempt errs.ProviderWriteAttempt, messageID string, phase gmailPlannedPhase) (int, []byte, error) {
	values := url.Values{"fields": {gmailMessageWriteFields}}
	switch phase.Kind {
	case gmailPhaseTrash:
		return c.gmailMutationWriteRequest(ctx, http.MethodPost, "/messages/"+url.PathEscape(messageID)+"/trash", values, nil, "", attempt, limits.GmailMaxJSONResponseBytes)
	case gmailPhaseUntrash:
		return c.gmailMutationWriteRequest(ctx, http.MethodPost, "/messages/"+url.PathEscape(messageID)+"/untrash", values, nil, "", attempt, limits.GmailMaxJSONResponseBytes)
	case gmailPhaseModify:
		payload := map[string][]string{}
		if len(phase.Add) > 0 {
			payload["addLabelIds"] = append([]string(nil), phase.Add...)
		}
		if len(phase.Remove) > 0 {
			payload["removeLabelIds"] = append([]string(nil), phase.Remove...)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, c.writeNotApplied(attempt, providerAPIError(c.account, "/messages/"+messageID+"/modify"))
		}
		return c.gmailMutationWriteRequest(ctx, http.MethodPost, "/messages/"+url.PathEscape(messageID)+"/modify", values, body, "application/json", attempt, limits.GmailMaxJSONResponseBytes)
	default:
		return 0, nil, c.writeNotApplied(attempt, errs.NewSharedProviderContractInvalid())
	}
}

func (c *Client) knownPartialFromStatus(ctx context.Context, snap labelSnapshot, attempt errs.ProviderWriteAttempt, plan errs.KnownPartialPlan, prefix uint16, mutation gmailMessageMutationPlan, lastVerified []string, status int, body []byte) error {
	rejection := c.gmailWriteStatusRejection(mutation.WriteOperation, "/messages/"+url.PathEscape(mutation.MessageID), status, body)
	if rejection == nil {
		return errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	observed := errs.KnownObservedDiagnosticUnavailable
	if reread, err := c.readMessageLabels(ctx, mutation.MessageID, snap); err == nil {
		switch {
		case sameStringSet(reread.LabelIDs, lastVerified):
			observed = errs.KnownObservedLastVerified
		case sameStringSet(reread.LabelIDs, mutation.Final):
			observed = errs.KnownObservedIntendedFinal
		default:
			observed = errs.KnownObservedOther
		}
	}
	return errs.NewKnownPartial(attempt, rejection, plan, prefix, observed, "")
}

func (c *Client) messageMutationResult(plan gmailMessageMutationPlan, facts *mail.WriteOutcomeFacts, changed bool) mail.MessageMutationResult {
	return mail.MessageMutationResult{
		MessageID:         plan.MessageID,
		TargetFolder:      cloneFolderSummary(plan.TargetFolder),
		ReadState:         cloneBoolChange(plan.ReadState),
		FlagState:         cloneBoolChange(plan.FlagState),
		Categories:        cloneCategoryChange(plan.Categories),
		Changed:           boolPtr(changed),
		WriteOutcomeFacts: mail.CloneWriteOutcomeFacts(facts),
		ProviderMeta:      gmailMutationMeta(plan.Final),
		ResultMessageID:   "",
		SourceFolder:      nil,
		InternetMessageID: "",
	}
}

func (c *Client) categoryFinalSet(req mail.MessageCategorySetRequest, current []string, before []string, snap labelSnapshot) ([]string, *mail.CategoryChange, error) {
	system := make([]string, 0)
	userSet := map[string]bool{}
	for _, id := range current {
		label := snap.labels[id]
		if label.Type == "user" {
			userSet[id] = true
		} else {
			system = append(system, id)
		}
	}
	switch req.Mode {
	case mail.CategoryMutationSet:
		afterIDs := make([]string, 0, len(req.Categories))
		for _, name := range req.Categories {
			label, ok := userLabelByExactName(snap, name)
			if !ok {
				return nil, nil, invalidCategorySelector(c.account, name)
			}
			afterIDs = append(afterIDs, label.ID)
		}
		afterNames := append([]string(nil), req.Categories...)
		removed := categoryRemoved(before, afterNames)
		change := &mail.CategoryChange{
			Before:  cloneStringSliceNonNil(before),
			After:   cloneStringSliceNonNil(afterNames),
			Removed: cloneStringSliceNonNil(removed),
			Skipped: []string{},
			Count:   len(afterNames),
		}
		return canonicalLabelIDs(append(system, afterIDs...)), change, nil
	case mail.CategoryMutationClearAll:
		change := &mail.CategoryChange{
			Before:  cloneStringSliceNonNil(before),
			After:   []string{},
			Removed: cloneStringSliceNonNil(before),
			Skipped: []string{},
			Count:   0,
		}
		return canonicalLabelIDs(system), change, nil
	case mail.CategoryMutationClearNamed:
		removeIDs := map[string]bool{}
		removed := make([]string, 0)
		skipped := make([]string, 0)
		for _, name := range req.Requested {
			label, ok := userLabelByExactName(snap, name)
			if !ok || !userSet[label.ID] {
				skipped = append(skipped, name)
				continue
			}
			removeIDs[label.ID] = true
			removed = append(removed, name)
		}
		final := make([]string, 0, len(current))
		for _, id := range current {
			if !removeIDs[id] {
				final = append(final, id)
			}
		}
		after := attachedUserLabelNames(final, snap)
		change := &mail.CategoryChange{
			Before:  cloneStringSliceNonNil(before),
			After:   cloneStringSliceNonNil(after),
			Removed: cloneStringSliceNonNil(removed),
			Skipped: cloneStringSliceNonNil(skipped),
			Count:   len(after),
		}
		return canonicalLabelIDs(final), change, nil
	default:
		return nil, nil, errs.NewSharedProviderContractInvalid()
	}
}

func (c *Client) destinationLabel(snap labelSnapshot, folder mail.Folder) (gmailLabel, error) {
	label, ok := snap.labels[folder.ID]
	if !ok {
		return gmailLabel{}, errs.NewWithDetails(errs.CodeFolderNotFound, "folder not found", map[string]any{
			"account":   c.account,
			"provider":  "gmail",
			"folder_id": folder.ID,
		})
	}
	if label.Type == "user" {
		return label, nil
	}
	if label.Type == "system" {
		return label, nil
	}
	return gmailLabel{}, providerDataError(c.account, "invalid_protocol")
}

func destinationFinalSet(op mail.MessageMutationOperation, current []string, dest gmailLabel) ([]string, []gmailPlannedPhase, error) {
	switch op {
	case mail.MutationMove:
		if dest.Type == "user" {
			return addLabels(removeLabels(current, "INBOX", "SPAM", "TRASH"), dest.ID), nil, nil
		}
		switch strings.ToUpper(dest.ID) {
		case "INBOX":
			return addLabels(removeLabels(current, "SPAM", "TRASH"), "INBOX"), nil, nil
		case "SPAM":
			return addLabels(removeLabels(current, "INBOX", "TRASH"), "SPAM"), nil, nil
		case "TRASH":
			final := addLabels(removeLabels(current, "INBOX", "SPAM"), "TRASH")
			if sameStringSet(current, final) {
				return final, nil, nil
			}
			return final, []gmailPlannedPhase{{Kind: gmailPhaseTrash, Expected: final}}, nil
		default:
			return nil, nil, invalidSystemDestination(dest.ID)
		}
	case mail.MutationCopy:
		if dest.Type != "user" {
			return nil, nil, invalidSystemDestination(dest.ID)
		}
		return addLabels(current, dest.ID), nil, nil
	case mail.MutationRestore:
		if dest.Type == "user" {
			return addLabels(removeLabels(current, "SPAM", "TRASH"), dest.ID), nil, nil
		}
		if strings.EqualFold(dest.ID, "INBOX") {
			return addLabels(removeLabels(current, "SPAM", "TRASH"), "INBOX"), nil, nil
		}
		return nil, nil, invalidSystemDestination(dest.ID)
	default:
		return nil, nil, errs.NewSharedProviderContractInvalid()
	}
}

func (c *Client) phasesForMaybeUntrash(current, final []string) []gmailPlannedPhase {
	if !hasLabel(current, "TRASH") {
		return freezeModifyPhases(current, final)
	}
	if sameStringSet(current, final) {
		return nil
	}
	return []gmailPlannedPhase{{Kind: gmailPhaseUntrash}}
}

func modifyPhases(current, final []string) []gmailPlannedPhase {
	return freezeModifyPhases(current, final)
}

func freezeModifyPhases(current, final []string) []gmailPlannedPhase {
	add, remove := labelDiff(current, final)
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	phaseCount := (max(len(add), len(remove)) + gmailModifyChunkSize - 1) / gmailModifyChunkSize
	phases := make([]gmailPlannedPhase, 0, phaseCount)
	state := canonicalLabelIDs(current)
	for i := 0; i < phaseCount; i++ {
		a := chunk(add, i, gmailModifyChunkSize)
		r := chunk(remove, i, gmailModifyChunkSize)
		state = addLabels(removeLabels(state, r...), a...)
		phases = append(phases, gmailPlannedPhase{Kind: gmailPhaseModify, Add: a, Remove: r, Expected: state})
	}
	return phases
}

func knownPartialPlan(plan gmailMessageMutationPlan) (errs.KnownPartialPlan, error) {
	if len(plan.Phases) < 2 {
		return nil, nil
	}
	phases := make([]errs.KnownPartialPhase, len(plan.Phases))
	for i, phase := range plan.Phases {
		switch phase.Kind {
		case gmailPhaseModify:
			phases[i] = errs.KnownPhaseModifyChunk
		case gmailPhaseTrash:
			phases[i] = errs.KnownPhaseTrash
		case gmailPhaseUntrash:
			phases[i] = errs.KnownPhaseUntrash
		default:
			return nil, errs.NewSharedProviderContractInvalid()
		}
	}
	return errs.NewKnownPartialPlan(plan.KnownOperation, phases)
}

func writeOperationForMutation(op mail.MessageMutationOperation) (errs.ProviderWriteOperation, errs.KnownPartialOperation) {
	switch op {
	case mail.MutationMove:
		return errs.WriteGmailMove, errs.KnownGmailMove
	case mail.MutationCopy:
		return errs.WriteGmailCopy, errs.KnownGmailCopy
	case mail.MutationRestore:
		return errs.WriteGmailRestore, errs.KnownGmailRestore
	default:
		return "", ""
	}
}

func (c *Client) readMessageLabels(ctx context.Context, id string, snap labelSnapshot) (gmailMessageLabelSnapshot, error) {
	values := url.Values{}
	values.Set("format", "minimal")
	values.Set("fields", gmailMessageLabelReadFields)
	status, body, err := c.gmailRequest(ctx, http.MethodGet, "/messages/"+url.PathEscape(id), values, nil, limits.GmailMaxJSONResponseBytes)
	if err != nil {
		return gmailMessageLabelSnapshot{}, err
	}
	if status < 200 || status >= 300 {
		return gmailMessageLabelSnapshot{}, classifyHTTPStatus(c.account, "/messages/"+url.PathEscape(id), status, body, false)
	}
	var raw struct {
		ID       string          `json:"id"`
		LabelIDs json.RawMessage `json:"labelIds"`
	}
	if err := decodeJSONResponse(c.account, body, &raw); err != nil {
		return gmailMessageLabelSnapshot{}, err
	}
	if raw.ID != id {
		return gmailMessageLabelSnapshot{}, providerDataError(c.account, "invalid_protocol")
	}
	labels, err := parseGmailLabelIDs(c.account, raw.LabelIDs, snap)
	if err != nil {
		return gmailMessageLabelSnapshot{}, err
	}
	return gmailMessageLabelSnapshot{ID: raw.ID, LabelIDs: labels}, nil
}

func (c *Client) validateMessageWriteResponse(body []byte, messageID string, snap labelSnapshot) errs.OutcomeUnknownReason {
	var raw struct {
		ID       string          `json:"id"`
		LabelIDs json.RawMessage `json:"labelIds"`
	}
	if err := decodeJSONResponse(c.account, body, &raw); err != nil {
		return errs.OutcomeProviderResultMalformed
	}
	if raw.ID == "" {
		return errs.OutcomeProviderResultMissing
	}
	if raw.ID != messageID {
		return errs.OutcomeProviderResultMalformed
	}
	if raw.LabelIDs != nil {
		if _, err := parseGmailLabelIDs(c.account, raw.LabelIDs, snap); err != nil {
			return errs.OutcomeProviderResultMalformed
		}
	}
	return ""
}

func parseGmailLabelIDs(account string, raw json.RawMessage, snap labelSnapshot) ([]string, error) {
	if raw == nil {
		return []string{}, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, providerDataError(account, "invalid_protocol")
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, providerDataError(account, "invalid_protocol")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			return nil, providerDataError(account, "invalid_protocol")
		}
		if _, ok := snap.labels[id]; !ok {
			return nil, providerDataError(account, "invalid_protocol")
		}
		seen[id] = true
	}
	return canonicalLabelIDs(ids), nil
}

func (c *Client) gmailMutationWriteRequest(ctx context.Context, method, path string, values url.Values, body []byte, contentType string, attempt errs.ProviderWriteAttempt, limit int64) (int, []byte, error) {
	authHeader, err := c.authorizationHeader()
	if err != nil {
		return 0, nil, c.writeNotApplied(attempt, err)
	}
	target, err := c.buildURL(path, values)
	if err != nil {
		return 0, nil, c.writeNotApplied(attempt, err)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, c.writeNotApplied(attempt, providerAPIError(c.account, path))
	}
	req.Header.Set("Authorization", authHeader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if err := errs.CrossProviderWriteTransmissionBoundary(attempt); err != nil {
		return 0, nil, err
	}
	resp, err := doNoRedirect(c.http, req)
	if err != nil {
		return 0, nil, errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	defer resp.Body.Close()
	data, err := readLimited(resp.Body, limit)
	if err != nil {
		return resp.StatusCode, nil, errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && len(bytes.TrimSpace(data)) > 0 && !gmailJSONContentType(resp.Header.Get("Content-Type")) {
		return resp.StatusCode, nil, errs.NewOutcomeUnknown(attempt, errs.OutcomeProviderResultMalformed)
	}
	return resp.StatusCode, data, nil
}

func gmailJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func (c *Client) gmailWriteStatusError(attempt errs.ProviderWriteAttempt, operation errs.ProviderWriteOperation, path string, status int, body []byte) error {
	rejection := c.gmailWriteStatusRejection(operation, path, status, body)
	if rejection != nil {
		return errs.NewWriteRejected(attempt, rejection)
	}
	return errs.NewOutcomeUnknown(attempt, errs.OutcomeAcceptanceResponseLost)
}

func (c *Client) gmailWriteStatusRejection(operation errs.ProviderWriteOperation, path string, status int, body []byte) errs.DefinitiveProviderRejection {
	cause := c.gmailWriteStatusCause(operation, path, status, body)
	if cause == nil {
		return nil
	}
	return c.definitiveWriteRejection(cause, operation)
}

func (c *Client) gmailWriteStatusCause(operation errs.ProviderWriteOperation, path string, status int, body []byte) error {
	reason := googleErrorReason(body)
	switch status {
	case http.StatusBadRequest:
		if operation == errs.WriteGmailLabelCreate || operation == errs.WriteGmailLabelRename {
			return invalidFolderNameForGmail("", "provider_rejected_name")
		}
		return providerDataError(c.account, nonemptyReason(reason, "api_error"))
	case http.StatusUnauthorized:
		return repairRequired(c.account, "credential_rejected")
	case http.StatusForbidden:
		return classifyHTTPStatus(c.account, path, status, body, false)
	case http.StatusNotFound, http.StatusGone:
		switch operation {
		case errs.WriteGmailLabelRename, errs.WriteGmailLabelDelete:
			return notFoundError(c.account, path)
		case errs.WriteGmailLabelCreate:
			return providerDataError(c.account, "api_error")
		default:
			return messageNotFound(c.account, mail.ResolvedSelectorID)
		}
	case http.StatusConflict:
		if operation == errs.WriteGmailLabelCreate || operation == errs.WriteGmailLabelRename {
			return invalidFolderNameForGmail("", "already_exists")
		}
		return providerDataError(c.account, nonemptyReason(reason, "api_error"))
	case http.StatusRequestEntityTooLarge:
		return providerDataError(c.account, nonemptyReason(reason, "api_error"))
	case http.StatusTooManyRequests:
		return providerRateLimited(c.account, nonemptyReason(reason, "rate_limit"))
	default:
		return nil
	}
}

func (c *Client) directUserLabel(ctx context.Context, id, expectedName string) (gmailLabelDetail, error) {
	values := url.Values{"fields": {gmailLabelDirectFields}}
	var detail gmailLabelDetail
	if err := c.getJSON(ctx, "/labels/"+url.PathEscape(id), values, limits.GmailMaxJSONResponseBytes, &detail); err != nil {
		return gmailLabelDetail{}, err
	}
	if detail.ID != id || detail.Name != expectedName || detail.Type != "user" {
		return gmailLabelDetail{}, providerDataError(c.account, "invalid_protocol")
	}
	if detail.MessagesTotal != nil && *detail.MessagesTotal < 0 {
		return gmailLabelDetail{}, providerDataError(c.account, "invalid_json")
	}
	if detail.MessagesUnread != nil && *detail.MessagesUnread < 0 {
		return gmailLabelDetail{}, providerDataError(c.account, "invalid_json")
	}
	if detail.Color != nil && (!validHexColor(detail.Color.TextColor) || !validHexColor(detail.Color.BackgroundColor)) {
		return gmailLabelDetail{}, providerDataError(c.account, "invalid_protocol")
	}
	return detail, nil
}

func (c *Client) labelVerificationUnknown(attempt errs.ProviderWriteAttempt, err error) error {
	var product *errs.Error
	if errors.As(err, &product) && product.Code() == errs.CodeProviderDataError {
		return errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	return errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
}

func (c *Client) verifyLabelAbsent(ctx context.Context, id string) error {
	values := url.Values{"fields": {gmailLabelDeleteProofFields}}
	status, body, err := c.gmailRequest(ctx, http.MethodGet, "/labels/"+url.PathEscape(id), values, nil, limits.GmailMaxJSONResponseBytes)
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		return nil
	case status >= 200 && status < 300:
		var raw struct {
			ID string `json:"id"`
		}
		if err := decodeJSONResponse(c.account, body, &raw); err != nil {
			return err
		}
		return errs.NewWithDetails(errs.CodeVerificationFailed, "Gmail label still exists after delete", map[string]any{
			"account":  c.account,
			"provider": "gmail",
			"reason":   "label_still_present",
		})
	default:
		return classifyHTTPStatus(c.account, "/labels/"+url.PathEscape(id), status, body, false)
	}
}

func errToDeleteVerificationUnknown(attempt errs.ProviderWriteAttempt, err error) error {
	var product *errs.Error
	if errors.As(err, &product) && product.Code() == errs.CodeVerificationFailed {
		return errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteMismatch)
	}
	return errs.NewOutcomeUnknown(attempt, errs.OutcomePostWriteReadFailed)
}

func (c *Client) userLabelFromFolder(snap labelSnapshot, folder mail.Folder) (mail.Folder, gmailLabel, error) {
	f, label, err := c.userLabelSummaryFromID(snap, folder.ID)
	if err != nil {
		return mail.Folder{}, gmailLabel{}, err
	}
	return f, label, nil
}

func (c *Client) userLabelSummaryFromID(snap labelSnapshot, id string) (mail.Folder, gmailLabel, error) {
	label, ok := snap.labels[id]
	if !ok {
		return mail.Folder{}, gmailLabel{}, errs.NewWithDetails(errs.CodeFolderNotFound, "folder not found", map[string]any{
			"account":   c.account,
			"provider":  "gmail",
			"folder_id": id,
		})
	}
	if label.Type != "user" {
		return mail.Folder{}, gmailLabel{}, protectedGmailFolder(c.account, id)
	}
	folder, ok := snap.byID[id]
	if !ok {
		return mail.Folder{}, gmailLabel{}, providerDataError(c.account, "invalid_protocol")
	}
	return folder, label, nil
}

func protectedGmailFolder(account, id string) error {
	return errs.NewWithDetails(errs.CodeProtectedFolder, "this is one of Gmail's own labels; maildo changes only labels you created", map[string]any{
		"account":   account,
		"provider":  "gmail",
		"folder_id": id,
		"reason":    "provider_managed_folder",
	})
}

func isProtectedFolderError(err error) bool {
	var product *errs.Error
	return errors.As(err, &product) && product.Code() == errs.CodeProtectedFolder
}

func rejectLabelNameCollision(snap labelSnapshot, candidate, excludeID string) error {
	for id, label := range snap.labels {
		if id == excludeID {
			continue
		}
		if label.Name == candidate || strings.EqualFold(label.Name, candidate) {
			return invalidFolderNameForGmail(candidate, "already_exists")
		}
	}
	return nil
}

func invalidFolderNameForGmail(name, reason string) error {
	details := map[string]any{"field": "display_name", "reason": reason}
	if name != "" {
		details["display_name"] = name
	}
	return errs.NewWithDetails(errs.CodeInvalidFolderName, "Gmail rejected the folder name", details)
}

func invalidSystemDestination(id string) error {
	return errs.NewWithDetails(errs.CodeInvalidSelector, "this is one of Gmail's own labels and cannot be the target here; use a label you created", map[string]any{
		"provider":  "gmail",
		"folder_id": id,
		"reason":    gmailSystemUnsupportedReason,
	})
}

func invalidCategorySelector(account, name string) error {
	return errs.NewWithDetails(errs.CodeInvalidSelector, fmt.Sprintf("Gmail has no label named %q; a category must match the name of a label you created exactly", name), map[string]any{
		"account":  account,
		"provider": "gmail",
		"category": name,
		"reason":   "category_not_found",
	})
}

func (c *Client) overlayCreatedLabel(id, name string, detail gmailLabelDetail) (mail.Folder, error) {
	return c.overlayLabelCatalog(func(base []gmailLabel) ([]gmailLabel, error) {
		next := append(append([]gmailLabel(nil), base...), gmailLabel{ID: id, Name: name, Type: "user"})
		return next, nil
	}, id, "", detail)
}

func (c *Client) overlayRenamedLabel(id, name string, detail gmailLabelDetail) (mail.Folder, error) {
	return c.overlayLabelCatalog(func(base []gmailLabel) ([]gmailLabel, error) {
		next := append([]gmailLabel(nil), base...)
		for i := range next {
			if next[i].ID == id {
				next[i].Name = name
				next[i].Type = "user"
				return next, nil
			}
		}
		return nil, providerDataError(c.account, "invalid_protocol")
	}, id, "", detail)
}

func (c *Client) overlayDeletedLabel(id string) error {
	_, err := c.overlayLabelCatalog(func(base []gmailLabel) ([]gmailLabel, error) {
		next := make([]gmailLabel, 0, len(base))
		found := false
		for _, label := range base {
			if label.ID == id {
				found = true
				continue
			}
			next = append(next, label)
		}
		if !found {
			return nil, providerDataError(c.account, "invalid_protocol")
		}
		return next, nil
	}, "", id, gmailLabelDetail{})
	return err
}

func (c *Client) overlayLabelCatalog(update func([]gmailLabel) ([]gmailLabel, error), detailID string, removeDetailID string, detail gmailLabelDetail) (mail.Folder, error) {
	c.mu.Lock()
	base := append([]gmailLabel(nil), c.labelBase...)
	c.mu.Unlock()
	if base == nil {
		return mail.Folder{}, providerDataError(c.account, "invalid_protocol")
	}
	next, err := update(base)
	if err != nil {
		return mail.Folder{}, err
	}
	if err := validateLabelCatalog(c.account, next); err != nil {
		return mail.Folder{}, err
	}
	details := map[string]gmailLabelDetail{}
	if detailID != "" {
		details[detailID] = detail
	}
	snap, err := c.buildSnapshot(next, details)
	if err != nil {
		return mail.Folder{}, err
	}
	c.mu.Lock()
	c.labelBase = append([]gmailLabel(nil), next...)
	if c.labelDetails == nil {
		c.labelDetails = map[string]gmailLabelDetail{}
	}
	if detailID != "" {
		c.labelDetails[detailID] = detail
	}
	if removeDetailID != "" {
		delete(c.labelDetails, removeDetailID)
	}
	c.mu.Unlock()
	if detailID != "" {
		return snap.byID[detailID], nil
	}
	return mail.Folder{}, nil
}

func validateLabelCatalog(account string, labels []gmailLabel) error {
	if len(labels) > limits.GmailMaxLabels {
		return providerDataError(account, "invalid_protocol")
	}
	seen := map[string]bool{}
	for _, label := range labels {
		if label.ID == "" || label.Name == "" || (label.Type != "system" && label.Type != "user") || seen[label.ID] {
			return providerDataError(account, "invalid_protocol")
		}
		seen[label.ID] = true
	}
	return nil
}

func gmailFolderMutationMeta(use mail.ProviderMetadataUse, deleteSemantics bool) *mail.FolderProviderMetadata {
	meta := mail.FolderProviderMetadata{
		Source:    mail.MetadataSourceGmailAPI,
		LabelType: mail.LabelTypeUser,
	}
	if deleteSemantics {
		meta.DeleteSemantics = mail.DeleteSemanticsLabelOnly
	}
	return mustGmailMetadata(use, meta)
}

func gmailMutationMeta(final []string) *mail.MutationProviderMetadata {
	labels := canonicalLabelIDs(final)
	meta := mail.MutationProviderMetadata{
		Source:        mail.MetadataSourceGmailAPI,
		MutationBasis: mail.MutationBasisGmailLabels,
		FinalLabelIDs: &labels,
	}
	return mustGmailMetadata(mail.MetadataMessageMutation, meta)
}

func gmailDescendantFacts(targetName string, snap labelSnapshot) mail.FolderDescendantFacts {
	desc := make([]mail.FolderDescendant, 0)
	prefix := targetName + "/"
	for _, label := range snap.labels {
		if label.Type == "user" && strings.HasPrefix(label.Name, prefix) {
			desc = append(desc, mail.FolderDescendant{ID: label.ID, DisplayName: displayNameForRawUserLabel(label.Name), Path: label.Name})
		}
	}
	sort.SliceStable(desc, func(i, j int) bool {
		if desc[i].Path != desc[j].Path {
			return desc[i].Path < desc[j].Path
		}
		if desc[i].DisplayName != desc[j].DisplayName {
			return desc[i].DisplayName < desc[j].DisplayName
		}
		return desc[i].ID < desc[j].ID
	})
	facts := mail.FolderDescendantFacts{Count: uint64(len(desc)), Known: true, Sample: desc}
	if len(facts.Sample) > 20 {
		facts.Sample = append([]mail.FolderDescendant(nil), facts.Sample[:20]...)
		facts.Truncated = true
	}
	if facts.Sample == nil {
		facts.Sample = []mail.FolderDescendant{}
	}
	return facts
}

func gmailDirectChildCount(targetName string, snap labelSnapshot) int {
	userNames := map[string]bool{}
	for _, label := range snap.labels {
		if label.Type == "user" {
			userNames[label.Name] = true
		}
	}
	count := 0
	prefix := targetName + "/"
	for _, label := range snap.labels {
		if label.Type != "user" || !strings.HasPrefix(label.Name, prefix) {
			continue
		}
		nearest := targetName
		rest := strings.TrimPrefix(label.Name, prefix)
		parts := strings.Split(rest, "/")
		probe := targetName
		for i := 0; i < len(parts)-1; i++ {
			probe += "/" + parts[i]
			if userNames[probe] {
				nearest = probe
			}
		}
		if nearest == targetName {
			count++
		}
	}
	return count
}

func labelSettingsEqual(before, after gmailLabelDetail) bool {
	return stringPtrEqual(before.LabelListVisibility, after.LabelListVisibility) &&
		stringPtrEqual(before.MessageListVisibility, after.MessageListVisibility) &&
		colorEqual(before.Color, after.Color)
}

func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func colorEqual(a, b *gmailColor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.TextColor == b.TextColor && a.BackgroundColor == b.BackgroundColor
}

func renameCandidate(rawName, leaf string) string {
	if idx := strings.LastIndex(rawName, "/"); idx >= 0 {
		return rawName[:idx+1] + leaf
	}
	return leaf
}

func displayNameForRawUserLabel(name string) string {
	if idx := strings.LastIndex(name, "/"); idx >= 0 && idx < len(name)-1 {
		return name[idx+1:]
	}
	return name
}

func userLabelByExactName(snap labelSnapshot, name string) (gmailLabel, bool) {
	for _, label := range snap.labels {
		if label.Type == "user" && label.Name == name {
			return label, true
		}
	}
	return gmailLabel{}, false
}

func attachedUserLabelNames(ids []string, snap labelSnapshot) []string {
	type row struct{ name, id string }
	rows := make([]row, 0)
	for _, id := range ids {
		if label, ok := snap.labels[id]; ok && label.Type == "user" {
			rows = append(rows, row{name: label.Name, id: label.ID})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].name != rows[j].name {
			return rows[i].name < rows[j].name
		}
		return rows[i].id < rows[j].id
	})
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.name
	}
	return out
}

func categoryRemoved(before, after []string) []string {
	afterSet := map[string]bool{}
	for _, name := range after {
		afterSet[name] = true
	}
	removed := make([]string, 0)
	for _, name := range before {
		if !afterSet[name] {
			removed = append(removed, name)
		}
	}
	return removed
}

func addLabels(labels []string, add ...string) []string {
	set := labelSet(labels)
	for _, id := range add {
		set[id] = true
	}
	return sortedLabelSet(set)
}

func removeLabels(labels []string, remove ...string) []string {
	set := labelSet(labels)
	for _, id := range remove {
		delete(set, id)
	}
	return sortedLabelSet(set)
}

func labelDiff(current, final []string) ([]string, []string) {
	cur := labelSet(current)
	dst := labelSet(final)
	add := make([]string, 0)
	remove := make([]string, 0)
	for id := range dst {
		if !cur[id] {
			add = append(add, id)
		}
	}
	for id := range cur {
		if !dst[id] {
			remove = append(remove, id)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	return add, remove
}

func labelSet(labels []string) map[string]bool {
	set := make(map[string]bool, len(labels))
	for _, id := range labels {
		set[id] = true
	}
	return set
}

func sortedLabelSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func canonicalLabelIDs(labels []string) []string {
	out := append([]string(nil), labels...)
	sort.Strings(out)
	return uniqueStrings(out)
}

func sameStringSet(a, b []string) bool {
	x := canonicalLabelIDs(a)
	y := canonicalLabelIDs(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func chunk(values []string, idx, size int) []string {
	start := idx * size
	if start >= len(values) {
		return nil
	}
	end := min(start+size, len(values))
	return append([]string(nil), values[start:end]...)
}

func cloneFolderSummary(in *mail.FolderSummary) *mail.FolderSummary {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneBoolChange(in *mail.BoolChange) *mail.BoolChange {
	if in == nil {
		return nil
	}
	out := *in
	if in.Before != nil {
		before := *in.Before
		out.Before = &before
	}
	return &out
}

func cloneCategoryChange(in *mail.CategoryChange) *mail.CategoryChange {
	if in == nil {
		return nil
	}
	return &mail.CategoryChange{
		Before:  cloneStringSliceMaybeNil(in.Before),
		After:   cloneStringSliceMaybeNil(in.After),
		Removed: cloneStringSliceMaybeNil(in.Removed),
		Skipped: cloneStringSliceMaybeNil(in.Skipped),
		Count:   in.Count,
	}
}

func cloneStringSliceMaybeNil(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}

func cloneStringSliceNonNil(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	return append([]string(nil), in...)
}

func boolPtr(v bool) *bool {
	return &v
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
