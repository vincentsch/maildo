package imap

import (
	"context"
	"sort"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/limits"
	"github.com/vincentsch/maildo/internal/mail"
)

// Threads.
//
// IMAP has no conversation ids, so maildo rebuilds a thread from the
// Message-ID, References and In-Reply-To headers. Starting from the ids one
// message knows, it searches every folder except Trash and Junk for messages
// that carry any of those ids in those headers, learns the ids those messages
// mention, and repeats until no new id turns up.
//
// The result says "complete" only when that search finished inside its limits
// and every message's reference headers could be read. A thread cut short by
// a limit is returned with complete=false, never as complete.

const (
	maxThreadRounds = 6
	// threadIDsPerSearch limits how many ids one UID SEARCH looks for, so its
	// OR chain stays short. Some servers reject very deep ones.
	threadIDsPerSearch = 10
)

func (c *accountClient) ReadThread(ctx context.Context, req mail.ReadThreadRequest) (mail.Thread, error) {
	var out mail.Thread
	err := c.withIMAPSession(ctx, func(session *imapSession) error {
		var seed imapFetchedMessage
		var root, threadID string
		switch req.Selector.Kind {
		case mail.ResolvedSelectorThreadID:
			parsedRoot, singleton, err := parseThreadID(req.Selector.ThreadID)
			if err != nil {
				return invalidIMAPMessageSelector(c, "thread_id", req.Selector.ThreadID)
			}
			threadID = req.Selector.ThreadID
			if singleton {
				msg, err := c.resolveMessage(ctx, session, mail.ResolvedMessageSelector{Kind: mail.ResolvedSelectorID, ID: parsedRoot}, errs.ProviderOperationIMAPThreadsRead, true)
				if err != nil {
					if errorCode(err) == errs.CodeMessageNotFound {
						return errs.NewIMAPThreadNotFound(c.account, imapErrProvider(c.provider), "thread_id", req.Selector.ThreadID)
					}
					return err
				}
				out = c.threadFromMessages(threadID, []imapFetchedMessage{msg}, mail.ThreadBasisIMAPSingleton, true)
				return nil
			}
			root = parsedRoot
		case mail.ResolvedSelectorID, mail.ResolvedSelectorInternetID:
			msg, err := c.resolveMessage(ctx, session, req.Selector, errs.ProviderOperationIMAPThreadsRead, true)
			if err != nil {
				return err
			}
			if msg.internetMessageID == "" {
				id, err := formatSingletonThreadID(msg.id)
				if err != nil {
					return err
				}
				out = c.threadFromMessages(id, []imapFetchedMessage{msg}, mail.ThreadBasisIMAPSingleton, true)
				return nil
			}
			seed = msg
			root = rootMessageID(msg.header, msg.internetMessageID)
			threadID, err = formatThreadID(root)
			if err != nil {
				return c.providerDataError(errs.ProviderOperationIMAPThreadsRead, errs.ProviderDataReasonInvalidHeader)
			}
		default:
			return invalidIMAPMessageSelector(c, string(req.Selector.Kind), "")
		}
		thread, err := c.collectThread(ctx, session, threadID, root, seed)
		if err != nil {
			return err
		}
		out = thread
		return nil
	})
	return out, err
}

func (c *accountClient) collectThread(ctx context.Context, session *imapSession, threadID, root string, seed imapFetchedMessage) (mail.Thread, error) {
	op := errs.ProviderOperationIMAPThreadsRead
	snapshot, err := c.folderSnapshot(ctx, session, false)
	if err != nil {
		return mail.Thread{}, err
	}
	folders := searchableFolders(snapshot)

	known := map[string]bool{root: true}
	messages := map[string]imapFetchedMessage{}
	complete := true
	learn := func(msg imapFetchedMessage) {
		messages[msg.id] = msg
		for _, id := range threadIDsOf(msg) {
			known[id] = true
		}
		if !msg.header.completeRefs {
			complete = false
		}
	}
	if seed.id != "" {
		learn(seed)
	}
	searched := map[string]bool{}
	seedRefetched := false
	for round := 0; ; round++ {
		var pending []string
		for id := range known {
			if !searched[id] {
				pending = append(pending, id)
			}
		}
		if len(pending) == 0 {
			break
		}
		if round >= maxThreadRounds || len(known) > limits.IMAPMaxThreadRelationIDs {
			complete = false
			break
		}
		sort.Strings(pending)
		for _, id := range pending {
			searched[id] = true
		}
		for _, folder := range folders {
			status, refused, err := c.examineForSearch(ctx, session, folder.mailbox, op)
			if err != nil {
				return mail.Thread{}, err
			}
			if refused || status.total == 0 {
				continue
			}
			folder.uidValidity = status.uidValidity
			folder.uidNext = status.uidNext
			for start := 0; start < len(pending); start += threadIDsPerSearch {
				end := start + threadIDsPerSearch
				if end > len(pending) {
					end = len(pending)
				}
				uids, err := c.uidSearch(ctx, session, threadSearchCriteria(pending[start:end]), op)
				if err != nil {
					return mail.Thread{}, err
				}
				var fresh []uint32
				for _, uid := range uids {
					id, err := formatMessageID(folder.mailbox, folder.uidValidity, uid)
					// The seed was read without a preview; fetch it once more
					// with one when the search finds it.
					if err == nil && (messages[id].id == "" || (id == seed.id && !seedRefetched)) {
						fresh = append(fresh, uid)
						if id == seed.id {
							seedRefetched = true
						}
					}
				}
				if len(fresh) == 0 {
					continue
				}
				if len(messages)+len(fresh) > limits.IMAPMaxThreadCandidates {
					complete = false
					fresh = fresh[:max(0, limits.IMAPMaxThreadCandidates-len(messages))]
				}
				msgs, skipped, err := c.fetchMessages(ctx, session, folder, fresh, true, op)
				if err != nil {
					return mail.Thread{}, err
				}
				if len(skipped) > 0 {
					complete = false
				}
				for _, msg := range msgs {
					// HEADER search also matches parts of an id, so take only
					// messages that name one of the ids in full.
					if mentionsAny(msg, pending[start:end]) {
						learn(msg)
					}
				}
			}
		}
	}
	// The message we started from has its own Message-ID, so a search that
	// does not find it cannot be trusted to find its replies either.
	// Some servers, GreenMail among them, answer header searches that way.
	if seed.id != "" && !seedRefetched && folderSearched(folders, seed.mailbox) {
		complete = false
	}
	if len(messages) == 0 {
		return mail.Thread{}, errs.NewIMAPThreadNotFound(c.account, imapErrProvider(c.provider), "thread_id", threadID)
	}
	rows := make([]imapFetchedMessage, 0, len(messages))
	for _, msg := range messages {
		rows = append(rows, msg)
	}
	return c.threadFromMessages(threadID, rows, mail.ThreadBasisIMAPReferences, complete), nil
}

func folderSearched(folders []imapFolderRecord, mailbox string) bool {
	for _, folder := range folders {
		if folder.mailbox == mailbox {
			return true
		}
	}
	return false
}

// threadSearchCriteria matches messages whose Message-ID, References or
// In-Reply-To contains any of ids.
func threadSearchCriteria(ids []string) goimap.SearchCriteria {
	var alternatives []goimap.SearchCriteria
	for _, id := range ids {
		for _, field := range []string{"Message-ID", "References", "In-Reply-To"} {
			alternatives = append(alternatives, goimap.SearchCriteria{Header: []goimap.SearchCriteriaHeaderField{{Key: field, Value: id}}})
		}
	}
	return anyOf(alternatives)
}

// anyOf builds an IMAP OR chain: OR a (OR b (OR c d)).
func anyOf(alternatives []goimap.SearchCriteria) goimap.SearchCriteria {
	if len(alternatives) == 1 {
		return alternatives[0]
	}
	return goimap.SearchCriteria{Or: [][2]goimap.SearchCriteria{{alternatives[0], anyOf(alternatives[1:])}}}
}

// threadIDsOf lists the ids a message ties together: its own and the ones
// it refers to.
func threadIDsOf(msg imapFetchedMessage) []string {
	var out []string
	if msg.internetMessageID != "" {
		out = append(out, msg.internetMessageID)
	}
	out = append(out, msg.header.references...)
	out = append(out, msg.header.inReplyTo...)
	return out
}

func mentionsAny(msg imapFetchedMessage, ids []string) bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	for _, id := range threadIDsOf(msg) {
		if set[id] {
			return true
		}
	}
	return false
}

func (c *accountClient) threadFromMessages(threadID string, messages []imapFetchedMessage, basis mail.ThreadBasis, complete bool) mail.Thread {
	sort.SliceStable(messages, func(i, j int) bool {
		ti, tj := imapMessageTime(messages[i]), imapMessageTime(messages[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return messages[i].id < messages[j].id
	})
	rows := make([]mail.ThreadMessage, 0, len(messages))
	for _, msg := range messages {
		summary := c.toSummary(msg)
		rows = append(rows, mail.ThreadMessage{
			Account:           summary.Account,
			Provider:          summary.Provider,
			ID:                summary.ID,
			InternetMessageID: summary.InternetMessageID,
			ThreadID:          threadID,
			Folder:            summary.Folder,
			From:              summary.From,
			Subject:           summary.Subject,
			ReceivedAt:        summary.ReceivedAt,
			SentAt:            summary.SentAt,
			IsRead:            summary.IsRead,
			Importance:        summary.Importance,
			HasAttachments:    summary.HasAttachments,
			Preview:           summary.Preview,
		})
	}
	limit := uint64(limits.IMAPMaxThreadCandidates)
	count := uint64(len(rows))
	meta, _ := mail.NewProviderMetadata(imapErrProvider(c.provider), mail.MetadataThread, mail.ThreadProviderMetadata{
		Basis:          basis,
		Complete:       &complete,
		CandidateLimit: &limit,
		CandidateCount: &count,
	})
	return mail.Thread{Account: c.account, Provider: c.provider, ThreadID: threadID, Messages: rows, ProviderMeta: meta}
}

// imapMessageTime orders thread rows by the Date header, falling back to the
// arrival time.
func imapMessageTime(msg imapFetchedMessage) time.Time {
	if msg.envelope != nil && !msg.envelope.Date.IsZero() {
		return msg.envelope.Date
	}
	if msg.internalDate != nil {
		return *msg.internalDate
	}
	return time.Time{}
}
