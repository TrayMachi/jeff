package projects

import "testing"

func TestUnderRoot(t *testing.T) {
	for _, path := range []string{"/home/tray/projects/app", "/home/tray/projects/nested/app"} {
		if !underRoot(path) {
			t.Errorf("rejected %s", path)
		}
	}
	for _, path := range []string{"/home/tray/projectsive/app", "/home/tray/secrets", "/home/tray/projects/../secrets"} {
		if underRoot(path) {
			t.Errorf("accepted %s", path)
		}
	}
}
func TestValidAlias(t *testing.T) {
	if !validAlias("api-v2") || validAlias("api.v2") || validAlias("1api") {
		t.Fatal("alias validation incorrect")
	}
}
