package auth

import (
	"testing"

	"github.com/zkrebbekx/chronicle"
)

func creds() []Credential {
	return []Credential{
		{Token: "short", Principal: Principal{Actor: chronicle.Actor{ID: "a1"}, Role: RoleWriter}},
		{Token: "a-considerably-longer-token-value", Principal: Principal{Actor: chronicle.Actor{ID: "a2"}, Role: RoleAdmin}},
	}
}

func TestAuthenticate(t *testing.T) {
	a, err := New(creds())
	if err != nil {
		t.Fatal(err)
	}

	p, ok := a.Authenticate("short")
	if !ok || p.Actor.ID != "a1" || p.Role != RoleWriter {
		t.Fatalf("short token = (%+v, %v)", p, ok)
	}
	p, ok = a.Authenticate("a-considerably-longer-token-value")
	if !ok || p.Actor.ID != "a2" || p.Role != RoleAdmin {
		t.Fatalf("long token = (%+v, %v)", p, ok)
	}

	// Near misses of every shape fail: wrong value, prefix, extension,
	// empty. Length differences are absorbed by hashing before comparison,
	// which is what keeps the compare constant-time.
	for _, bad := range []string{"", "shor", "shortx", "SHORT", "a-considerably-longer-token-valu"} {
		if _, ok := a.Authenticate(bad); ok {
			t.Fatalf("token %q authenticated", bad)
		}
	}
}

func TestNewRejectsBadCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   []Credential
	}{
		{"empty table", nil},
		{"empty token", []Credential{{Token: "", Principal: Principal{Actor: chronicle.Actor{ID: "a"}, Role: RoleWriter}}}},
		{"no actor", []Credential{{Token: "t", Principal: Principal{Role: RoleWriter}}}},
		{"bad role", []Credential{{Token: "t", Principal: Principal{Actor: chronicle.Actor{ID: "a"}, Role: "root"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.in); err == nil {
				t.Fatal("New succeeded, want error")
			}
		})
	}
}
