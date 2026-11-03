package blob

import "testing"

func TestR2Configuration(t *testing.T) {
	account := "0123456789abcdef0123456789abcdef"
	p, err := NewR2Projects(account, "kioku-projects", "key", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if p.Template.Endpoint != "https://"+account+".r2.cloudflarestorage.com" || p.Template.Region != "auto" {
		t.Fatal("unexpected R2 endpoint or signing region")
	}
	for _, input := range [][4]string{
		{"https://example.com", "kioku-projects", "key", "secret"},
		{account, "../bucket", "key", "secret"},
		{account, "kioku-projects", "", "secret"},
		{account, "kioku-projects", "key", ""},
	} {
		if _, err := NewR2Projects(input[0], input[1], input[2], input[3]); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
