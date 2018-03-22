package openshift

import (
	"strings"
	"testing"

	"k8s.io/kubernetes/staging/src/k8s.io/apimachinery/pkg/util/wait"
)

func TestCommandFor(t *testing.T) {
	cmd := CommandFor("openshift-router", wait.NeverStop)
	if !strings.HasPrefix(cmd.Use, "openshift-router ") {
		t.Errorf("expected command to start with prefix: %#v", cmd)
	}

	cmd = CommandFor("unknown", wait.NeverStop)
	if cmd.Use != "openshift" {
		t.Errorf("expected command to be openshift: %#v", cmd)
	}
}
