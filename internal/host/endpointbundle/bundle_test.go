package endpointbundle

import (
	"bytes"
	"debug/elf"
	"testing"
)

func TestPayloadArchitecture(t *testing.T) {
	for platform, machine := range map[string]elf.Machine{"linux/amd64": elf.EM_X86_64, "linux/x86_64": elf.EM_X86_64, "linux/arm64": elf.EM_AARCH64, "linux/aarch64": elf.EM_AARCH64} {
		data, err := Payload(platform)
		if err != nil {
			t.Fatal(err)
		}
		binary, err := elf.NewFile(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if binary.Machine != machine {
			t.Fatalf("%s contains %s", platform, binary.Machine)
		}
	}
	for _, platform := range []string{"linux/arm", "darwin/arm64", "", "linux"} {
		if _, err := Payload(platform); err == nil {
			t.Errorf("accepted %q", platform)
		}
	}
}
