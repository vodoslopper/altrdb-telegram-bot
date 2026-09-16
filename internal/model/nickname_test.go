package model

import (
	"strings"
	"testing"
)

func TestNormalizeNickname(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"plain", "grenka", "grenka", false},
		{"uppercase is folded", "GrenKa", "grenka", false},
		{"surrounding space", "  rider  ", "rider", false},
		{"leading at sign", "@glebfm", "glebfm", false},
		{"at sign then space", "@ glebfm", "glebfm", false},
		{"digits", "user42", "user42", false},
		{"dot plus underscore", "a.b_c+d-e", "a.b_c+d-e", false},
		{"single character", "x", "x", false},
		{"64 characters is the maximum", strings.Repeat("a", 64), strings.Repeat("a", 64), false},

		{"empty", "", "", true},
		{"only spaces", "   ", "", true},
		{"only an at sign", "@", "", true},
		{"65 characters", strings.Repeat("a", 65), "", true},
		{"leading dash", "-grenka", "", true},
		{"leading dot", ".grenka", "", true},
		{"leading plus", "+grenka", "", true},
		{"space inside", "alt linux", "", true},
		{"slash", "people/grenka", "", true},
		{"at sign inside", "gre@nka", "", true},
		{"unicode", "гренька", "", true},
		{"asterisk", "gre*nka", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeNickname(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeNickname(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeNickname(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeNickname(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseNicknames(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantValid   []string
		wantInvalid []string
	}{
		{
			name:      "space separated",
			in:        "grenka rider glebfm",
			wantValid: []string{"grenka", "rider", "glebfm"},
		},
		{
			name:      "comma separated",
			in:        "grenka,rider,glebfm",
			wantValid: []string{"grenka", "rider", "glebfm"},
		},
		{
			name:      "mixed separators, newlines and semicolons",
			in:        "grenka, rider\n@glebfm;ldv\tshabaev",
			wantValid: []string{"grenka", "rider", "glebfm", "ldv", "shabaev"},
		},
		{
			name:      "duplicates collapse, first spelling wins",
			in:        "grenka Grenka @GRENKA rider",
			wantValid: []string{"grenka", "rider"},
		},
		{
			name:        "invalid entries are reported, valid ones still accepted",
			in:          "grenka not valid!! rider",
			wantValid:   []string{"grenka", "not", "rider"},
			wantInvalid: []string{"valid!!"},
		},
		{
			name:        "everything invalid",
			in:          "@@ !! --",
			wantValid:   nil,
			wantInvalid: []string{"@@", "!!", "--"},
		},
		{
			name:      "empty input",
			in:        "   ",
			wantValid: nil,
		},
		{
			name:        "repeated invalid token is reported once",
			in:          "a! a! b!",
			wantValid:   nil,
			wantInvalid: []string{"a!", "b!"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			valid, invalid := ParseNicknames(tc.in)
			if strings.Join(valid, ",") != strings.Join(tc.wantValid, ",") {
				t.Errorf("valid = %v, want %v", valid, tc.wantValid)
			}
			if strings.Join(invalid, ",") != strings.Join(tc.wantInvalid, ",") {
				t.Errorf("invalid = %v, want %v", invalid, tc.wantInvalid)
			}
		})
	}
}

func TestParseNicknamesRespectsTheLengthCap(t *testing.T) {
	valid, invalid := ParseNicknames(strings.Repeat("a", 64) + " " + strings.Repeat("b", 65))
	if len(valid) != 1 || valid[0] != strings.Repeat("a", 64) {
		t.Errorf("valid = %v", valid)
	}
	if len(invalid) != 1 {
		t.Errorf("invalid = %v, want exactly the over-long token", invalid)
	}
}

func TestValidNickname(t *testing.T) {
	if !ValidNickname("glebfm") {
		t.Error("glebfm must be valid")
	}
	if ValidNickname("Glebfm") {
		t.Error("ValidNickname is for already-normalised input")
	}
	if ValidNickname("") {
		t.Error("the empty string is not a nickname")
	}
}
