package mail

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
)

func TestParseFolderRef(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want FolderRef
	}{
		{name: "empty", raw: "  ", want: FolderRef{Raw: "", Kind: FolderRefOpaque}},
		{name: "well-known", raw: " sentitems ", want: FolderRef{Raw: "sentitems", Kind: FolderRefWellKnown}},
		{name: "path", raw: "Clients/ACME", want: FolderRef{Raw: "Clients/ACME", Kind: FolderRefPath}},
		{name: "opaque", raw: "Archive1", want: FolderRef{Raw: "Archive1", Kind: FolderRefOpaque}},
		{name: "slash with empty segment", raw: "Clients//ACME", want: FolderRef{Raw: "Clients//ACME", Kind: FolderRefPath}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFolderRef(tt.raw)
			if err != nil {
				t.Fatalf("ParseFolderRef returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParseFolderRef(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestNormalizeWellKnown(t *testing.T) {
	for raw, want := range map[string]string{
		"inbox":        WellKnownInbox,
		"sentitems":    WellKnownSent,
		"deleteditems": WellKnownDeleted,
		"junkemail":    WellKnownJunk,
		"spam":         WellKnownJunk,
		" ARCHIVE ":    WellKnownArchive,
	} {
		got, ok := NormalizeWellKnown(raw)
		if !ok || got != want {
			t.Fatalf("NormalizeWellKnown(%q) = %q,%t want %q,true", raw, got, ok, want)
		}
	}
	if got, ok := NormalizeWellKnown("custom"); ok || got != "" {
		t.Fatalf("NormalizeWellKnown(custom) = %q,%t want empty,false", got, ok)
	}
}

func TestValidateFolderDisplayName(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "trim", raw: "  Clients  ", want: "Clients"},
		{name: "unicode", raw: "Buchhaltung", want: "Buchhaltung"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateFolderDisplayName(tt.raw)
			if err != nil {
				t.Fatalf("ValidateFolderDisplayName(%q) error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("ValidateFolderDisplayName(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}

	for _, raw := range []string{"", "   ", "Clients/ACME", "Bad\tName", "Bad\x7fName", "\tClients", "Clients\n"} {
		t.Run(raw, func(t *testing.T) {
			_, err := ValidateFolderDisplayName(raw)
			if err == nil {
				t.Fatalf("ValidateFolderDisplayName(%q) succeeded", raw)
			}
			got, ok := err.(*errs.Error)
			if !ok || got.Code() != errs.CodeInvalidFolderName {
				t.Fatalf("error = %#v, want invalid_folder_name", err)
			}
		})
	}

	_, err := ValidateFolderDisplayName("Clients/ACME")
	product, ok := err.(*errs.Error)
	if !ok || !strings.Contains(product.Message(), "between the folders of a path") {
		t.Fatalf("slash validation error = %#v, want folder path separator wording", err)
	}
}

func TestValidateFolderDisplayNameRejectsEveryASCIIControlAtEveryPosition(t *testing.T) {
	controls := make([]byte, 0, 33)
	for value := byte(0); value <= 0x1f; value++ {
		controls = append(controls, value)
	}
	controls = append(controls, 0x7f)

	for _, control := range controls {
		for _, raw := range []string{
			string([]byte{control}) + "Alpha",
			"Al" + string([]byte{control}) + "pha",
			"Alpha" + string([]byte{control}),
		} {
			name := fmt.Sprintf("0x%02x/%x", control, []byte(raw))
			t.Run(name, func(t *testing.T) {
				_, err := ValidateFolderDisplayName(raw)
				product, ok := err.(*errs.Error)
				if !ok || product.Code() != errs.CodeInvalidFolderName {
					t.Fatalf("ValidateFolderDisplayName(%q) error = %#v, want invalid_folder_name", raw, err)
				}
			})
		}
	}
}

func TestValidateFolderDisplayNameKeepsLegitimateEscapedAndUnicodeText(t *testing.T) {
	for _, raw := range []string{
		`Client's "Quoted" Folder`,
		`Backslash \\ Folder`,
		"Buchhaltung – 東京",
		"Projects 📁",
	} {
		t.Run(raw, func(t *testing.T) {
			got, err := ValidateFolderDisplayName(raw)
			if err != nil {
				t.Fatalf("ValidateFolderDisplayName(%q) error: %v", raw, err)
			}
			if got != raw {
				t.Fatalf("ValidateFolderDisplayName(%q) = %q", raw, got)
			}
		})
	}
}
