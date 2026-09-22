package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseEnvLiteralValues(t *testing.T) {
	got, err := ParseEnv([]byte("# comment\nexport TOKEN=abc==\nEMPTY=\nQUOTED=\"two words\\nnext\" # note\nLITERAL='$(touch nope) $HOME'\nHASH=abc#def\nCOMMENT=value # note\n"))
	want := map[string]string{"TOKEN": "abc==", "EMPTY": "", "QUOTED": "two words\nnext", "LITERAL": "$(touch nope) $HOME", "HASH": "abc#def", "COMMENT": "value"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, %v", got, err)
	}
	for _, bad := range []string{"diagnostic text", "BAD-NAME=secret", "A='secret", "A=\"secret\"junk", "_DWORM_BASE=secret", "A=\"\\x00\""} {
		if _, err := ParseEnv([]byte(bad)); err == nil {
			t.Errorf("accepted invalid input %q", bad)
		}
	}
}

func TestLoadAndPrecedence(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Load(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".dworm.config"), []byte("bind = '0.0.0.0'\n[host_env]\ncommand = ['bash', './token.sh']\nrefresh_interval = '1h'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".dworm.env"), []byte("TOKEN=static\nKEEP=yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, static, err := Load(root)
	if err != nil || cfg.Bind != "0.0.0.0" || len(cfg.HostEnv.Command) != 2 {
		t.Fatalf("%+v %v", cfg, err)
	}
	if got := Merge(static, map[string]string{"TOKEN": "generated"}, map[string]string{"TOKEN": "cli"}); got["TOKEN"] != "cli" || got["KEEP"] != "yes" {
		t.Fatal(got)
	}
	for _, invalid := range []string{"unknown = true", "[host_env]\nurl = 'https://example.com'", "[host_env]\nrefresh_interval = '1h'", "[host_env]\ncommand=['true']\ntimeout='0s'"} {
		os.WriteFile(filepath.Join(root, ".dworm.config"), []byte(invalid), 0600)
		if _, _, err := Load(root); err == nil {
			t.Errorf("accepted %s", invalid)
		}
	}
}
