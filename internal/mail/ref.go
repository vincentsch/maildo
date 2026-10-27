package mail

import "strings"

// ParseFolderRef works out what kind of folder argument the user typed,
// without asking the provider. ResolveFolderRef then finds the folder and
// gives the errors.
func ParseFolderRef(raw string) (FolderRef, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return FolderRef{Raw: "", Kind: FolderRefOpaque}, nil
	}
	if _, ok := wellKnownAliases[strings.ToLower(trimmed)]; ok {
		return FolderRef{Raw: trimmed, Kind: FolderRefWellKnown}, nil
	}
	if strings.Contains(trimmed, "/") {
		return FolderRef{Raw: trimmed, Kind: FolderRefPath}, nil
	}
	return FolderRef{Raw: trimmed, Kind: FolderRefOpaque}, nil
}
