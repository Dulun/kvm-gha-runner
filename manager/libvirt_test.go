package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestCloudInitCarriesJITOnlyAsEncodedSecret(t *testing.T) {
	const jit = "super-secret-jit-config"
	userData := buildCloudInitUserData(jit, "runner", "")
	if strings.Contains(userData, jit) {
		t.Fatal("cloud-init YAML contains the raw JIT config")
	}
	if !strings.Contains(userData, base64.StdEncoding.EncodeToString([]byte(jit))) {
		t.Fatal("cloud-init YAML does not contain the encoded JIT config")
	}
	for _, required := range []string{
		"/run/actions-runner-jit",
		"/usr/local/sbin/kvm-actions-runner-once",
		"github-actions-runner.service",
		"systemctl, start, --no-block",
	} {
		if !strings.Contains(userData, required) {
			t.Fatalf("cloud-init YAML is missing %q", required)
		}
	}
}
