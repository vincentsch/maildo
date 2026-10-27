package outlook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

// archivePolicyWarnings lists the warnings a report can carry, in the order
// they are shown.
var archivePolicyWarnings = []mail.ArchivePolicyWarning{
	mail.ArchivePolicyWarningMailboxSettingsUnavailable,
	mail.ArchivePolicyWarningFolderIDMissing,
	mail.ArchivePolicyWarningFolderUnresolved,
}

// graphArchivePolicySettings holds the two mailbox settings the report uses.
// They stay raw so a missing value and null can be told apart.
type graphArchivePolicySettings struct {
	ArchiveFolder json.RawMessage `json:"archiveFolder"`
	UserPurpose   json.RawMessage `json:"userPurpose"`
}

// graphArchivePolicyFolder is the archive folder as Graph returns it. The
// counts stay raw so maildo can tell a missing count from zero and check that
// it fits Graph's Int32 range.
type graphArchivePolicyFolder struct {
	ID               json.RawMessage `json:"id"`
	DisplayName      json.RawMessage `json:"displayName"`
	TotalItemCount   json.RawMessage `json:"totalItemCount"`
	UnreadItemCount  json.RawMessage `json:"unreadItemCount"`
	ChildFolderCount json.RawMessage `json:"childFolderCount"`
}

// ArchivePolicyReport reads what the Graph mail API says about archiving:
// the mailbox purpose and the archive folder. Graph does not show the
// Exchange Online Archive or retention (MRM) policies, so the report always
// lists those as unavailable.
func (c *Client) ArchivePolicyReport(ctx context.Context, _ mail.ArchivePolicyReportRequest) (mail.ArchivePolicyReport, error) {
	settingsValues := url.Values{}
	settingsValues.Set("$select", "archiveFolder,userPurpose")
	settingsTarget := withQuery(c.graphBaseURL+"/me/mailboxSettings", settingsValues)
	status, body, err := c.graphReadRequest(ctx, graphReadArchivePolicy, settingsTarget, "")
	if err != nil {
		return mail.ArchivePolicyReport{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return c.finalizeArchivePolicyReport(
			mail.ArchivePolicyReportUnavailable, "", "", nil,
			[]mail.ArchivePolicyWarning{mail.ArchivePolicyWarningMailboxSettingsUnavailable},
		)
	}
	if status != http.StatusOK {
		return mail.ArchivePolicyReport{}, c.archivePolicyError()
	}

	var settings graphArchivePolicySettings
	if err := decodeJSONObjectWithUniqueFields(body, []string{"archiveFolder", "userPurpose"}, &settings); err != nil {
		return mail.ArchivePolicyReport{}, c.archivePolicyError()
	}
	archiveFolderID, err := decodeArchiveFolderID(settings.ArchiveFolder)
	if err != nil {
		return mail.ArchivePolicyReport{}, c.archivePolicyError()
	}
	mailboxPurpose, err := decodeMailboxPurpose(settings.UserPurpose)
	if err != nil {
		return mail.ArchivePolicyReport{}, c.archivePolicyError()
	}
	if archiveFolderID == "" {
		return c.finalizeArchivePolicyReport(
			mail.ArchivePolicyReportPartial, mailboxPurpose, "", nil,
			[]mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderIDMissing},
		)
	}

	// The id comes from Outlook and is not a well-known folder name, so it
	// goes into the URL as one escaped path segment, unchanged.
	folderValues := url.Values{}
	folderValues.Set("$select", "id,displayName,totalItemCount,unreadItemCount,childFolderCount")
	folderTarget, err := c.archiveFolderTarget(archiveFolderID, folderValues)
	if err != nil {
		return mail.ArchivePolicyReport{}, c.archivePolicyError()
	}
	status, body, err = c.graphReadRequest(ctx, graphReadArchivePolicy, folderTarget.String(), "")
	if err != nil {
		return mail.ArchivePolicyReport{}, err
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		return c.finalizeArchivePolicyReport(
			mail.ArchivePolicyReportPartial, mailboxPurpose, archiveFolderID, nil,
			[]mail.ArchivePolicyWarning{mail.ArchivePolicyWarningFolderUnresolved},
		)
	}
	if status != http.StatusOK {
		return mail.ArchivePolicyReport{}, c.archivePolicyError()
	}
	folder, err := decodeArchivePolicyFolder(body, archiveFolderID)
	if err != nil {
		return mail.ArchivePolicyReport{}, c.archivePolicyError()
	}
	return c.finalizeArchivePolicyReport(mail.ArchivePolicyReportPartial, mailboxPurpose, archiveFolderID, &folder, nil)
}

// decodeArchiveFolderID treats a blank id as missing. Any other id is returned
// unchanged, since it is compared and put into a URL as it is.
func decodeArchiveFolderID(raw json.RawMessage) (string, error) {
	value, err := decodeArchivePolicyString(raw)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	return value, nil
}

// decodeMailboxPurpose accepts both forms Graph has been seen to send for
// userPurpose: a string, or an object with a string "value". Values maildo
// does not know are kept, because Microsoft may add new ones.
func decodeMailboxPurpose(raw json.RawMessage) (string, error) {
	if rawJSONAbsent(raw) {
		return "", nil
	}
	direct, directErr := decodeArchivePolicyString(raw)
	if directErr == nil {
		if strings.TrimSpace(direct) == "" {
			return "", nil
		}
		return direct, nil
	}
	var envelope map[string]json.RawMessage
	if err := decodeJSONObjectWithUniqueFields(raw, []string{"value"}, &envelope); err != nil {
		return "", err
	}
	valueRaw, ok := envelope["value"]
	if !ok || rawJSONAbsent(valueRaw) {
		return "", fmt.Errorf("mailbox purpose value is unavailable")
	}
	value, err := decodeArchivePolicyString(valueRaw)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	return value, nil
}

// decodeArchivePolicyFolder accepts the folder only when it is the folder
// maildo asked for and every count it has is valid.
func decodeArchivePolicyFolder(body []byte, expectedID string) (mail.ArchivePolicyFolder, error) {
	var raw graphArchivePolicyFolder
	if err := decodeJSONObjectWithUniqueFields(body, []string{
		"id", "displayName", "totalItemCount", "unreadItemCount", "childFolderCount",
	}, &raw); err != nil {
		return mail.ArchivePolicyFolder{}, err
	}
	id, err := decodeArchivePolicyString(raw.ID)
	if err != nil {
		return mail.ArchivePolicyFolder{}, err
	}
	displayName, err := decodeArchivePolicyString(raw.DisplayName)
	if err != nil {
		return mail.ArchivePolicyFolder{}, err
	}
	if strings.TrimSpace(id) == "" || id != expectedID || strings.TrimSpace(displayName) == "" {
		return mail.ArchivePolicyFolder{}, fmt.Errorf("invalid archive folder identity")
	}
	total, totalKnown, err := decodeGraphCount(raw.TotalItemCount)
	if err != nil {
		return mail.ArchivePolicyFolder{}, err
	}
	unread, unreadKnown, err := decodeGraphCount(raw.UnreadItemCount)
	if err != nil {
		return mail.ArchivePolicyFolder{}, err
	}
	children, childrenKnown, err := decodeGraphCount(raw.ChildFolderCount)
	if err != nil {
		return mail.ArchivePolicyFolder{}, err
	}
	return mail.ArchivePolicyFolder{
		ID: id, DisplayName: displayName,
		TotalItemCount: total, TotalItemCountKnown: totalKnown,
		UnreadItemCount: unread, UnreadItemCountKnown: unreadKnown,
		ChildFolderCount: children, ChildFolderCountKnown: childrenKnown,
	}, nil
}

func decodeArchivePolicyString(raw json.RawMessage) (string, error) {
	if rawJSONAbsent(raw) {
		return "", nil
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("archive policy string is not valid UTF-8")
	}
	if !validJSONUnicodeEscapes(raw) {
		return "", fmt.Errorf("archive policy string contains an invalid Unicode escape")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

// validJSONUnicodeEscapes rejects unpaired UTF-16 surrogate escapes, which
// encoding/json would silently turn into U+FFFD. A real U+FFFD is fine. This
// way a string from Outlook is either kept exactly or rejected.
func validJSONUnicodeEscapes(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		unit, ok := jsonHexCodeUnit(raw, i+1)
		if !ok {
			return false
		}
		i += 4
		if unit >= 0xd800 && unit <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, ok := jsonHexCodeUnit(raw, i+3)
			if !ok || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
			continue
		}
		if unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
	}
	return true
}

func jsonHexCodeUnit(raw []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, b := range raw[start : start+4] {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value |= uint16(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

// decodeJSONObjectWithUniqueFields rejects a response that has one of the
// fields maildo reads twice, or in another casing (encoding/json would pick
// one of them silently). Fields maildo does not read are ignored.
func decodeJSONObjectWithUniqueFields(body []byte, consumed []string, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := start.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("archive policy response is not an object")
	}

	seen := make(map[string]bool, len(consumed))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("archive policy object key is invalid")
		}
		for _, expected := range consumed {
			if key == expected {
				if seen[expected] {
					return fmt.Errorf("archive policy field %q is duplicated", expected)
				}
				seen[expected] = true
				break
			}
			if strings.EqualFold(key, expected) {
				return fmt.Errorf("archive policy field %q has ambiguous casing", expected)
			}
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	end, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := end.(json.Delim); !ok || delimiter != '}' {
		return fmt.Errorf("archive policy response object is incomplete")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("archive policy response contains trailing data")
	}
	return json.Unmarshal(body, dst)
}

// decodeGraphCount tells a missing count from zero and checks that the count
// fits Graph's Int32 range.
func decodeGraphCount(raw json.RawMessage) (int, bool, error) {
	if rawJSONAbsent(raw) {
		return 0, false, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false, err
	}
	if value < 0 || value > math.MaxInt32 {
		return 0, false, fmt.Errorf("Graph count is outside the Int32 range")
	}
	return int(value), true, nil
}

func rawJSONAbsent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// archiveFolderTarget adds the folder id to the URL as it is, without path
// cleaning, and checks the finished URL before the token is sent to it.
func (c *Client) archiveFolderTarget(id string, values url.Values) (*url.URL, error) {
	target, err := url.Parse(c.graphBaseURL + "/me/mailFolders")
	if err != nil || !c.validGraphURL(target.String()) {
		return nil, c.archivePolicyError()
	}
	if err := appendOpaqueGraphPathSegment(target, id); err != nil {
		return nil, c.archivePolicyError()
	}
	target.RawQuery = values.Encode()
	if !c.validGraphRuleRequestURL(target.String()) {
		return nil, c.archivePolicyError()
	}
	return target, nil
}

// finalizeArchivePolicyReport builds every successful report. It puts the
// warnings in their fixed order and checks that the status fits the data.
// It also adds the sections Graph cannot report, as unavailable.
func (c *Client) finalizeArchivePolicyReport(
	status mail.ArchivePolicyReportStatus,
	mailboxPurpose string,
	archiveFolderID string,
	archiveFolder *mail.ArchivePolicyFolder,
	warnings []mail.ArchivePolicyWarning,
) (mail.ArchivePolicyReport, error) {
	// Unknown or repeated warnings are a bug. The rest are sorted into the
	// fixed order, so the output does not depend on the caller's order.
	seen := make(map[mail.ArchivePolicyWarning]bool, len(warnings))
	for _, warning := range warnings {
		switch warning {
		case mail.ArchivePolicyWarningMailboxSettingsUnavailable,
			mail.ArchivePolicyWarningFolderIDMissing,
			mail.ArchivePolicyWarningFolderUnresolved:
		default:
			return mail.ArchivePolicyReport{}, c.archivePolicyError()
		}
		if seen[warning] {
			return mail.ArchivePolicyReport{}, c.archivePolicyError()
		}
		seen[warning] = true
	}
	orderedWarnings := make([]mail.ArchivePolicyWarning, 0, len(warnings))
	for _, warning := range archivePolicyWarnings {
		if seen[warning] {
			orderedWarnings = append(orderedWarnings, warning)
		}
	}
	if strings.TrimSpace(mailboxPurpose) == "" {
		mailboxPurpose = ""
	}

	// Each status allows exactly one combination of data and warnings, so a
	// half-built report never reaches the output.
	valid := false
	switch {
	case status == mail.ArchivePolicyReportUnavailable:
		valid = mailboxPurpose == "" && archiveFolderID == "" && archiveFolder == nil &&
			warningsEqual(orderedWarnings, mail.ArchivePolicyWarningMailboxSettingsUnavailable)
	case status == mail.ArchivePolicyReportPartial && archiveFolderID == "":
		valid = archiveFolder == nil && warningsEqual(orderedWarnings, mail.ArchivePolicyWarningFolderIDMissing)
	case status == mail.ArchivePolicyReportPartial && archiveFolder == nil:
		valid = strings.TrimSpace(archiveFolderID) != "" && warningsEqual(orderedWarnings, mail.ArchivePolicyWarningFolderUnresolved)
	case status == mail.ArchivePolicyReportPartial && archiveFolder != nil:
		valid = len(orderedWarnings) == 0 && strings.TrimSpace(archiveFolderID) != "" &&
			validArchivePolicyFolder(*archiveFolder, archiveFolderID)
	}
	if !valid {
		return mail.ArchivePolicyReport{}, c.archivePolicyError()
	}

	return mail.ArchivePolicyReport{
		Account: c.account, Provider: "outlook", Status: status,
		MailboxPurpose: mailboxPurpose, ArchiveFolderID: archiveFolderID, ArchiveFolder: archiveFolder,
		OnlineArchive: mail.ArchivePolicyAvailability{
			Status: mail.ArchivePolicyAvailabilityUnavailable,
			Reason: mail.ArchivePolicyReasonNotExposedByGraphMailAPI,
		},
		RetentionPolicy: mail.ArchivePolicyAvailability{
			Status: mail.ArchivePolicyAvailabilityUnavailable,
			Reason: mail.ArchivePolicyReasonRequiresExchangeAdminContext,
		},
		Warnings:         orderedWarnings,
		ProviderMetadata: mail.ArchivePolicyProviderMetadata{Source: mail.ArchivePolicySourceMicrosoftGraph},
	}, nil
}

// validArchivePolicyFolder checks the decoded folder once more, including
// that an unknown count is zero.
func validArchivePolicyFolder(folder mail.ArchivePolicyFolder, expectedID string) bool {
	if folder.ID != expectedID || strings.TrimSpace(folder.ID) == "" || strings.TrimSpace(folder.DisplayName) == "" {
		return false
	}
	for _, count := range []int{folder.TotalItemCount, folder.UnreadItemCount, folder.ChildFolderCount} {
		if count < 0 || int64(count) > math.MaxInt32 {
			return false
		}
	}
	if (!folder.TotalItemCountKnown && folder.TotalItemCount != 0) ||
		(!folder.UnreadItemCountKnown && folder.UnreadItemCount != 0) ||
		(!folder.ChildFolderCountKnown && folder.ChildFolderCount != 0) {
		return false
	}
	return true
}

func warningsEqual(warnings []mail.ArchivePolicyWarning, expected ...mail.ArchivePolicyWarning) bool {
	if len(warnings) != len(expected) {
		return false
	}
	for i := range warnings {
		if warnings[i] != expected[i] {
			return false
		}
	}
	return true
}

// archivePolicyError names only the account. It leaves out what Outlook
// sent, on purpose.
func (c *Client) archivePolicyError() error {
	return rungrad.NewError(rungrad.ExitAPI, fmt.Sprintf("could not read archive policy for account %q (outlook)", c.account))
}
