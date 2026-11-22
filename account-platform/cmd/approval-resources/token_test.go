package main

import "testing"

func TestTokenResourceArgumentsRejectBeforeCredentials(t *testing.T) {
	for _, args := range [][]string{nil, {"token-consumers"}, {"wrong", "check"}, {"token-consumers", "delete"}, {"token-consumers", "initialize", "extra"}} {
		if _, err := runTokenResources(args, func(string) string { t.Fatal("invalid command read credentials"); return "" }); err == nil || err.Error() != "TOKEN_RESOURCE_ARGUMENTS_INVALID" {
			t.Fatal("invalid Token operation accepted")
		}
	}
}

func TestTokenResourceReceiptPathRejectBeforeNetwork(t *testing.T) {
	for _, output := range []string{"", "relative.json"} {
		if _, err := runTokenResources([]string{"token-consumers", "check"}, func(key string) string {
			if key == "TOKEN_CONSUMER_RESOURCE_RECEIPT_FILE" {
				return output
			}
			return ""
		}); err == nil || err.Error() != "TOKEN_RESOURCE_RECEIPT_PATH_INVALID" {
			t.Fatal("invalid receipt path reached network operation")
		}
	}
}
