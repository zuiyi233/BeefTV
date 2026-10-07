package protocol

import (
	"context"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

func TestEvaluateManifestSplitToFloatDivide(t *testing.T) {
	env := map[string]any{"request": map[string]any{"aspectRatio": "1280x720"}}
	parts, err := evaluateManifestValue(map[string]any{"$split": []any{map[string]any{"$ref": "request.aspectRatio"}, "x"}}, env)
	if err != nil {
		t.Fatal(err)
	}
	items, ok := parts.([]any)
	if !ok || len(items) != 2 || items[0] != "1280" || items[1] != "720" {
		t.Fatalf("split = %#v", parts)
	}
	width, err := evaluateManifestValue(map[string]any{"$toFloat": map[string]any{"$at": []any{parts, 0}}}, env)
	if err != nil {
		t.Fatal(err)
	}
	height, err := evaluateManifestValue(map[string]any{"$toFloat": map[string]any{"$at": []any{parts, 1}}}, env)
	if err != nil {
		t.Fatal(err)
	}
	ratio, err := evaluateManifestValue(map[string]any{"$divide": []any{width, height}}, env)
	if err != nil {
		t.Fatal(err)
	}
	if got := manifestFloat(ratio); got < 1.77 || got > 1.78 {
		t.Fatalf("ratio = %v", got)
	}
}

func TestEvaluateManifestDivideRejectsZero(t *testing.T) {
	_, err := evaluateManifestValue(map[string]any{"$divide": []any{10, 0}}, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "denominator") {
		t.Fatalf("error = %v", err)
	}
}

func TestEvaluateManifestToFloatLeavesInvalidInputEmpty(t *testing.T) {
	value, err := evaluateManifestValue(map[string]any{
		"$coalesce": []any{
			map[string]any{"$toFloat": "not-a-number"},
			1,
		},
	}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got := manifestFloat(value); got != 1 {
		t.Fatalf("coalesced float = %v", got)
	}
}

func TestEvaluateManifestURLEncodeProducesQueryValue(t *testing.T) {
	env := map[string]any{
		"request": map[string]any{
			"filename":  "cat #50%.png",
			"subfolder": "2026/10 output",
			"steps":     float64(20),
			"flag":      true,
		},
	}
	// Query-value semantics: reserved characters are escaped and a space
	// becomes "+", matching url.Values.Encode used by the Go providers.
	cases := []struct {
		operand any
		want    string
	}{
		{map[string]any{"$ref": "request.filename"}, "cat+%2350%25.png"},
		{map[string]any{"$ref": "request.subfolder"}, "2026%2F10+output"},
		{"a&b=c?d#e%f g/h", "a%26b%3Dc%3Fd%23e%25f+g%2Fh"},
		{map[string]any{"$ref": "request.steps"}, "20"},
		{map[string]any{"$ref": "request.flag"}, "true"},
		{map[string]any{"$ref": "request.missing"}, ""},
	}
	for _, item := range cases {
		got, err := evaluateManifestValue(map[string]any{"$urlencode": item.operand}, env)
		if err != nil {
			t.Fatalf("$urlencode %#v: %v", item.operand, err)
		}
		if got != item.want {
			t.Fatalf("$urlencode %#v = %#v, want %#v", item.operand, got, item.want)
		}
	}
}

func TestEvaluateManifestURLEncodeMatchesFormEncoding(t *testing.T) {
	// ComfyUI fetches results through GET /view?filename=...&subfolder=...
	// and its filenames can carry non-ASCII text, so "$urlencode" must stay
	// byte-identical to the encoding url.Values.Encode produces.
	const filename = "一只猫 #50%.png"
	value, err := evaluateManifestValue(map[string]any{
		"$urlencode": map[string]any{"$literal": filename},
	}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{}
	query.Set("filename", filename)
	if want := query.Encode()[len("filename="):]; value != want {
		t.Fatalf("$urlencode = %#v, want %#v", value, want)
	}
	urlText, err := evaluateManifestValue(map[string]any{
		"$concat": []any{
			"http://127.0.0.1:8188/view?filename=",
			map[string]any{"$urlencode": map[string]any{"$literal": filename}},
			"&type=output",
		},
	}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	text, ok := urlText.(string)
	if !ok || !strings.HasPrefix(text, "http://127.0.0.1:8188/view?filename=%E4%B8%80%E5%8F%AA%E7%8C%AB+") {
		t.Fatalf("view url = %#v", urlText)
	}
}

func TestEvaluateManifestLookupTranslatesTableValues(t *testing.T) {
	env := map[string]any{
		"request": map[string]any{"sampler": "euler_a", "steps": float64(20)},
		"upstream": map[string]any{
			"sampler": map[string]any{"ancestral": "euler_ancestral"},
		},
	}
	// Hit: the user-facing value is translated to the upstream token.
	got, err := evaluateManifestValue(map[string]any{"$lookup": map[string]any{
		"from": map[string]any{"$ref": "request.sampler"},
		"map":  map[string]any{"euler_a": "euler_ancestral", "dpmpp_2m": "dpmpp_2m"},
	}}, env)
	if err != nil {
		t.Fatal(err)
	}
	if got != "euler_ancestral" {
		t.Fatalf("lookup = %#v", got)
	}
	// Table values stay template nodes, so they can reuse other operators.
	nested := map[string]any{
		"euler_a": map[string]any{"$concat": []any{"euler", "_", "ancestral"}},
		"lms":     map[string]any{"$upper": "lms"},
		"unified": map[string]any{"$ref": "upstream.sampler.ancestral"},
	}
	for key, want := range map[string]string{"euler_a": "euler_ancestral", "lms": "LMS", "unified": "euler_ancestral"} {
		got, err := evaluateManifestValue(map[string]any{"$lookup": map[string]any{
			"from": key,
			"map":  nested,
		}}, env)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got != want {
			t.Fatalf("lookup %s = %#v, want %#v", key, got, want)
		}
	}
}

func TestEvaluateManifestLookupMissFallsBackToSource(t *testing.T) {
	env := map[string]any{"request": map[string]any{"sampler": "dpmpp_sde", "steps": float64(20)}}
	// Without "default" the original value passes through unchanged, keeping
	// its type, so values that need no translation still reach the payload.
	got, err := evaluateManifestValue(map[string]any{"$lookup": map[string]any{
		"from": map[string]any{"$ref": "request.sampler"},
		"map":  map[string]any{"euler_a": "euler_ancestral"},
	}}, env)
	if err != nil {
		t.Fatal(err)
	}
	if got != "dpmpp_sde" {
		t.Fatalf("pass-through = %#v", got)
	}
	got, err = evaluateManifestValue(map[string]any{"$lookup": map[string]any{
		"from": map[string]any{"$ref": "request.steps"},
	}}, env)
	if err != nil {
		t.Fatal(err)
	}
	if typed, ok := got.(float64); !ok || typed != 20 {
		t.Fatalf("pass-through type = %#v (%T)", got, got)
	}
	// An explicit default node is evaluated when the key is present.
	got, err = evaluateManifestValue(map[string]any{"$lookup": map[string]any{
		"from":    map[string]any{"$ref": "request.sampler"},
		"map":     map[string]any{"euler_a": "euler_ancestral"},
		"default": map[string]any{"$ref": "request.steps"},
	}}, env)
	if err != nil {
		t.Fatal(err)
	}
	if typed, ok := got.(float64); !ok || typed != 20 {
		t.Fatalf("default = %#v (%T)", got, got)
	}
	// A null default is a present key, so the value is dropped instead of
	// passing through.
	got, err = evaluateManifestValue(map[string]any{"$lookup": map[string]any{
		"from":    map[string]any{"$ref": "request.sampler"},
		"map":     map[string]any{"euler_a": "euler_ancestral"},
		"default": nil,
	}}, env)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("null default = %#v", got)
	}
}

func TestEvaluateManifestLookupRejectsMalformedOperand(t *testing.T) {
	_, err := evaluateManifestValue(map[string]any{"$lookup": "euler_a"}, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "$lookup requires an object") {
		t.Fatalf("error = %v", err)
	}
	_, err = evaluateManifestValue(map[string]any{"$lookup": map[string]any{
		"from": "euler_a",
		"map":  []any{"euler_a"},
	}}, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "$lookup map requires an object") {
		t.Fatalf("error = %v", err)
	}
	// An unknown operator still fails loudly.
	_, err = evaluateManifestValue(map[string]any{"$only": "x"}, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "unsupported manifest operator") {
		t.Fatalf("error = %v", err)
	}
}

func TestBinaryPayloadCreateResultAudio(t *testing.T) {
	manifest := []byte(`{
		"apiVersion":"beeftv.plugin/v1",
		"id":"audio-bin","version":"1.0.0","name":"Audio Bin","author":"Test","documentation":"# Audio",
		"contributes":{"providers":[{
			"id":"openai-audio","label":"OpenAI Audio","capabilities":["audio"],"scopes":["canvas"],
			"create":{"method":"POST","path":"/v1/audio/speech","body":{"model":{"$ref":"request.model"},"input":{"$ref":"request.prompt"}}},
			"response":{"binaryPayload":true,"resultKind":"audio","status":"succeeded"}
		}]}
	}`)
	adapter, err := LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("ID3fake-mp3-bytes")
	result, err := adapter.ParseCreate(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusSucceeded || result.Result == nil || len(result.Result.Audios) != 1 {
		t.Fatalf("result = %#v", result)
	}
	audio := result.Result.Audios[0]
	if !strings.HasPrefix(audio.DataURL, "data:") || !strings.Contains(audio.DataURL, base64.StdEncoding.EncodeToString(payload)) {
		t.Fatalf("audio dataUrl = %q", audio.DataURL)
	}
}

func TestBinaryPayloadCreateResultRejectsEmpty(t *testing.T) {
	_, err := binaryPayloadCreateResult("audio", nil)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error = %v", err)
	}
}
