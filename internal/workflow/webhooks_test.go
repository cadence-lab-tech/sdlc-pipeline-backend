package workflow

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifyGitHubSignature(t *testing.T) {
	secret := "s3cret"
	body := []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !VerifyGitHubSignature(secret, body, good) {
		t.Fatal("верная подпись отбита")
	}
	if VerifyGitHubSignature(secret, []byte(`{}`), good) {
		t.Fatal("подпись другого тела прошла")
	}
	if VerifyGitHubSignature("other", body, good) {
		t.Fatal("подпись другим секретом прошла")
	}
	if VerifyGitHubSignature(secret, body, hex.EncodeToString(mac.Sum(nil))) {
		t.Fatal("подпись без префикса sha256= прошла")
	}
	// Без секрета вебхуки выключены — любая подпись недействительна.
	if VerifyGitHubSignature("", body, good) {
		t.Fatal("пустой секрет пропустил подпись")
	}
}
