package domain

import "testing"

func TestBrowserCapabilityVerifierPreservesV1WireDigest(t *testing.T) {
	if got := BrowserCapabilityVerifier("source", "source-token"); got != "WBi1HuPs0jzRemkSNvZSza-M92-5QsVzp7asA81nHZk" {
		t.Fatal("v1 capability digest changed", got)
	}
	if BrowserCapabilityVerifier("", "source-token") != "" || BrowserCapabilityVerifier("source", "") != "" {
		t.Fatal("missing launch identity gained evidence")
	}
}
