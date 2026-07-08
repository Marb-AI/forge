package proc

import (
	"os"
	"testing"
)

func TestAliveSelf(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("current process should be alive")
	}
}

func TestAliveMissing(t *testing.T) {
	// A pid this large is not in use.
	if Alive(1 << 30) {
		t.Error("expected non-existent pid to be not-alive")
	}
}

func TestAttrsNonNil(t *testing.T) {
	if DetachAttr() == nil {
		t.Error("DetachAttr nil")
	}
	if ChildAttr() == nil {
		t.Error("ChildAttr nil")
	}
}
