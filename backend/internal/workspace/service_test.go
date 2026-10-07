package workspace

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"infinite-canvas/backend/internal/privatefile"
)

func TestProviderConfigIsAtomicPrivateAndPreservesRedactedSecrets(t *testing.T) {
	dir := t.TempDir()
	store, err := NewProviderConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	initial := []byte(`{"channels":[{"id":"direct","name":"Direct","apiKey":"secret-key"}]}`)
	if err := store.SaveLocalModelConfig(initial); err != nil {
		t.Fatal(err)
	}

	// provider 配置里可能是真实上游密钥；Windows 上权限由 DACL 决定，
	// 比权限位在发布平台上说明不了任何事，交给跨平台校验。
	if err := privatefile.Verify(filepath.Join(dir, LocalProviderConfigFile)); err != nil {
		t.Fatalf("provider config 必须只对本机用户可读: %v", err)
	}

	redacted, err := store.ReadRedactedModelConfig()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(redacted, []byte("secret-key")) || !bytes.Contains(redacted, []byte(RedactedSecret)) {
		t.Fatal("redacted config leaks or drops secret marker")
	}
	var view map[string]any
	if err := json.Unmarshal(redacted, &view); err != nil {
		t.Fatal(err)
	}
	view["label"] = "updated"
	updated, _ := json.Marshal(view)
	if err := store.SaveLocalModelConfig(updated); err != nil {
		t.Fatal(err)
	}
	raw, err := store.ReadLocalModelConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("secret-key")) || !bytes.Contains(raw, []byte("updated")) {
		t.Fatal("secret-preserving update failed")
	}

	if err := store.SaveLocalModelConfig([]byte(`{"channels":`)); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	afterInvalid, err := store.ReadLocalModelConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, afterInvalid) {
		t.Fatal("invalid update changed the last valid provider config")
	}
}

func TestProviderConfigRejectsMissingDataDirectory(t *testing.T) {
	if _, err := NewProviderConfig(""); err == nil {
		t.Fatal("empty data directory was accepted")
	}
}
