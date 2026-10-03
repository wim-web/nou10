package appspec

import (
	"strings"
	"testing"
)

const valid = "version: 0.0\nos: linux\nhooks:\n  ValidateService:\n    - location: check.sh\n      timeout: 30\n"

func TestStrictSubset(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		ok         bool
	}{
		{"minimal", valid, true},
		{"files", valid + "files:\n  - source: /\n    destination: /srv/app\nfile_exists_behavior: RETAIN\n", true},
		{"permissions", valid + "permissions:\n  - object: /srv/app\n    mode: 755\n    owner: app\n    type: [directory]\n", true},
		{"wrong version", strings.Replace(valid, "0.0", "1.0", 1), false},
		{"wrong os", strings.Replace(valid, "linux", "windows", 1), false},
		{"typo", valid + "unknown: yes\n", false},
		{"permissions ACL", valid + "permissions:\n  - object: /srv/app\n    acls: [user:app:rw]\n", false},
		{"permissions pattern", valid + "permissions:\n  - object: /srv/app\n    pattern: '**'\n    mode: 755\n", false},
		{"privileged mode", valid + "permissions:\n  - object: /srv/app\n    mode: 4755\n", false},
		{"bad behavior", valid + "file_exists_behavior: overwrite\n", false},
		{"zero timeout", strings.Replace(valid, "timeout: 30", "timeout: 0", 1), false},
		{"huge timeout", strings.Replace(valid, "timeout: 30", "timeout: 3601", 1), false},
		{"event timeout sum", strings.Replace(valid, "timeout: 30", "timeout: 3600", 1) + "    - location: second.sh\n      timeout: 1\n", false},
		{"unsupported event", valid + "  BeforeAllowTraffic:\n    - location: check.sh\n", false},
		{"reserved event", valid + "  Install:\n    - location: check.sh\n", false},
		{"second document", valid + "---\nversion: 0.0\n", false},
		{"alias", valid + "files: &files []\npermissions: *files\n", false},
		{"duplicate field", valid + "os: linux\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.text))
			if (err == nil) != tc.ok {
				t.Fatalf("got %v", err)
			}
		})
	}
}
