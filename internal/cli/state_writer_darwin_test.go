//go:build darwin

package cli

import "testing"

func TestStateWriterDarwinLsofIdentityControl(t *testing.T) {
	raw := "p42\nftxt\nD0x1000004\ni123\nn/opt/libexec/a2a/image.Ab2xY9/a2a\n"
	image, err := parseBindingImage(raw)
	if err != nil || image != "0x1000004:123" {
		t.Fatalf("lsof image = %q, %v", image, err)
	}
	if got := stateWriterLsofPath(raw, image); got != "/opt/libexec/a2a/image.Ab2xY9/a2a" {
		t.Fatalf("lsof path = %q", got)
	}
	if _, err := parseBindingImage("p42\nftxt\nn/missing-vnode\n"); err == nil {
		t.Fatal("missing vnode accepted")
	}
}
