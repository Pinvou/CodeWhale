package billingschema

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestPaymentDispatchProvenancePins(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := Ordered(loaded)
	if err != nil {
		t.Fatal("canonical package rejected", err)
	}
	for _, version := range []int64{900004, 900005} {
		at := slices.IndexFunc(loaded, func(m migrations.Migration) bool { return m.Version == version })
		if at < 0 {
			t.Fatal("canonical resource absent", version)
		}
		t.Run(loaded[at].Name, func(t *testing.T) {
			for _, mutation := range []string{"missing-up", "missing-down", "up", "up-and-checksum", "down", "name", "checksum", "version", "duplicate", "alias-name", "alias-checksum", "alias-body"} {
				t.Run(mutation, func(t *testing.T) {
					input := slices.Clone(loaded)
					item := &input[at]
					switch mutation {
					case "missing-up":
						item.UpSQL = ""
					case "missing-down":
						item.DownSQL = ""
					case "up", "up-and-checksum":
						item.UpSQL += "\nSELECT 1;\n"
						if mutation == "up-and-checksum" {
							item.Checksum = sqlChecksum(item.UpSQL)
						}
					case "down":
						item.DownSQL = "SELECT 1;\n"
					case "name":
						item.Name += "_changed"
					case "checksum":
						item.Checksum = strings.Repeat("0", 64)
					case "version":
						item.Version = 999999
					case "duplicate":
						input = append(input, *item)
					default:
						alias := *item
						alias.Version = 999999
						if mutation != "alias-name" {
							alias.Name = "custom_name"
						}
						if mutation != "alias-checksum" {
							alias.Checksum = "custom_checksum"
						}
						if mutation != "alias-body" {
							alias.UpSQL = "SELECT 1;"
						}
						input = append(input, alias)
					}
					// A nil connection makes any premature database access fail;
					// all four public/domain entrypoints must reject the identity first.
					for name, call := range map[string]func() error{
						"ordered": func() error { _, err := Ordered(input); return err },
						"up":      func() error { return Up(context.Background(), nil, input) },
						"down":    func() error { return DownOne(context.Background(), nil, input) },
						"verify": func() error {
							_, err := VerifyApplied(context.Background(), nil, input)
							return err
						},
					} {
						t.Run(name, func(t *testing.T) {
							if err := call(); !errors.Is(err, errPaymentDispatchProvenanceIdentity) {
								t.Fatal("unfrozen identity accepted before database access", err)
							}
						})
					}
				})
			}
		})
	}
	for _, variant := range []string{"canonical-line-endings", "earlier-carrier"} {
		t.Run(variant, func(t *testing.T) {
			input := slices.Clone(loaded)
			if variant == "earlier-carrier" {
				input = slices.DeleteFunc(input, func(m migrations.Migration) bool { return m.Version == 900004 || m.Version == 900005 })
			} else {
				for i := range input {
					input[i].UpSQL = strings.ReplaceAll(strings.ReplaceAll(input[i].UpSQL, "\r\n", "\n"), "\n", "\r\n")
					input[i].DownSQL = strings.ReplaceAll(strings.ReplaceAll(input[i].DownSQL, "\r\n", "\n"), "\n", "\r\n")
				}
			}
			actual, err := Ordered(input)
			if err != nil {
				t.Fatal("existing carrier contract rejected", err)
			}
			expected := slices.Clone(ordered)
			if variant == "earlier-carrier" {
				expected = slices.DeleteFunc(expected, func(m migrations.Migration) bool { return m.Version == 900004 || m.Version == 900005 })
			}
			if !slices.EqualFunc(actual, expected, func(a, b migrations.Migration) bool {
				return a.Version == b.Version && a.Name == b.Name && a.Checksum == b.Checksum
			}) {
				t.Fatal("existing migration order or identity changed")
			}
		})
	}
}
