//go:build windows

package windowsacl

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestValidateDACL(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	base := fmt.Sprintf("(A;;GA;;;%s)(A;;GA;;;SY)(A;;GA;;;BA)", user.User.Sid.String())
	tests := []struct {
		name    string
		extra   string
		wantErr bool
	}{
		{name: "trusted writers only"},
		{name: "world read", extra: "(A;;GR;;;WD)"},
		{name: "world write", extra: "(A;;GW;;;WD)", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString("D:P" + base + test.extra)
			if err != nil {
				t.Fatalf("SecurityDescriptorFromString: %v", err)
			}
			dacl, _, err := descriptor.DACL()
			if err != nil {
				t.Fatalf("DACL: %v", err)
			}
			err = validateDACL("path", dacl, user.User.Sid)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateDACL() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
