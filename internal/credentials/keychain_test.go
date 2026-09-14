package credentials

import "testing"

func TestParseAccount(t *testing.T) {
	output := `keychain: "/Users/test/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "acct"<blob>="parent@example.test"
    "svce"<blob>="ednevnik-cli"`
	if got := parseAccount(output); got != "parent@example.test" {
		t.Fatalf("parseAccount() = %q", got)
	}
}
