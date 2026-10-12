package models

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/litellm"
)

func TestDigestTracksExecutionConfigWithoutLeakingAPIKey(t *testing.T) {
	first, err := (Config{Provider: "deepseek", Model: "deepseek-chat", APIKey: "secret-a"}).Digest()
	if err != nil {
		t.Fatalf("first digest: %v", err)
	}
	second, err := (Config{Provider: "deepseek", Model: "deepseek-chat", APIKey: "secret-b"}).Digest()
	if err != nil {
		t.Fatalf("second digest: %v", err)
	}
	if first != second {
		t.Fatal("API key rotation changed the semantic model config digest")
	}
	changed, err := (Config{Provider: "deepseek", Model: "deepseek-reasoner", APIKey: "secret-a"}).Digest()
	if err != nil {
		t.Fatalf("changed digest: %v", err)
	}
	if changed == first {
		t.Fatal("model change did not change the config digest")
	}
}

func TestThinkingLevelChangesDigestAndBinds(t *testing.T) {
	c := Config{Provider: "openai", Model: "custom", APIKey: "k"}
	auto, _ := c.Digest()
	c.Thinking = "high"
	high, _ := c.Digest()
	if auto == high {
		t.Fatal("thinking level must be part of the execution configuration")
	}
	binding, err := Bind(c)
	if err != nil || binding.Chat.Client == nil || binding.Digest != high || binding.Thinking != "high" || binding.Model != "custom" ||
		binding.Chat.Request.Model != "custom" || binding.Chat.Request.Thinking == nil || binding.Chat.Request.Thinking.Effort != "high" {
		t.Fatalf("binding = %#v, %v", binding, err)
	}
	roles := Bindings{Default: binding, Roles: map[string]Binding{"writer": {Model: "other"}}}
	if roles.For("writer").Model != "other" || roles.For("editor").Model != "custom" {
		t.Fatal("role lookup must fall back to the default binding")
	}
}

// 适配器发不出的档位不列、不发：MiMo 不收强度，Grok 关不掉思考；意图仍进摘要。
func TestLevelsFollowTheAdapter(t *testing.T) {
	for provider, want := range map[string][]string{
		"deepseek": ThinkingLevels,
		"mimo":     {"", "off"},
		"grok":     {"", "minimal", "low", "medium", "high", "xhigh", "max"},
	} {
		binding, err := Bind(Config{Provider: provider, Model: "m", APIKey: "k", Thinking: "off"})
		if err != nil {
			t.Fatal(err)
		}
		if got := Levels(binding.Chat.Client); !slices.Equal(got, want) {
			t.Errorf("%s: levels %q, want %q", provider, got, want)
		}
		sendsOff := binding.Chat.Request.Thinking != nil && binding.Chat.Request.Thinking.Disabled
		if offered := slices.Contains(want, "off"); sendsOff != offered || (binding.Thinking == "off") != offered {
			t.Errorf("%s: thinking off sent=%v binding=%q", provider, sendsOff, binding.Thinking)
		}
	}
}

func TestEndpointChangesDigest(t *testing.T) {
	c := Config{Provider: "openai", Model: "custom"}
	chat, _ := c.Digest()
	c.API = "responses"
	responses, _ := c.Digest()
	if chat == responses {
		t.Fatal("endpoint change must invalidate execution configuration")
	}
}

// 用户只选 OpenAI 格式：官方地址与 Responses 接口按官方实现，其余地址（中转、网关、
// 自部署）按通用兼容接口，才读得到 reasoning_content 里的思考。
func TestOpenAIFormatPicksTheAdapterByAddress(t *testing.T) {
	for _, tc := range []struct {
		config Config
		want   string
	}{
		{Config{Provider: "openai"}, "openai"},
		{Config{Provider: "openai", BaseURL: "https://api.openai.com/v1"}, "openai"},
		{Config{Provider: "openai", BaseURL: "https://relay.example.com/v1"}, "compat"},
		{Config{Provider: "openai", BaseURL: "https://relay.example.com/v1", API: "chat"}, "compat"},
		{Config{Provider: "openai", BaseURL: "https://relay.example.com/v1", API: "responses"}, "openai"},
		{Config{Provider: "anthropic", BaseURL: "https://relay.example.com"}, "anthropic"},
		{Config{Provider: "deepseek"}, "deepseek"},
	} {
		if got := adapter(tc.config); got != tc.want {
			t.Errorf("%+v: adapter = %q, want %q", tc.config, got, tc.want)
		}
	}
}

// 模型服务的失败按类别标记，文字原样保留给诊断；不属于这几类的失败不标。
func TestClassifyTagsModelServiceFailuresWithoutRewording(t *testing.T) {
	offline := litellm.NewNetworkError("compat", "request failed", errors.New(`Post "https://relay/v1/chat/completions": dial tcp: lookup relay: no such host`))
	for _, c := range []struct {
		err  error
		want model.FailureCode
	}{
		{offline, model.FailureModelUnreachable},
		{fmt.Errorf("stream: %w", offline), model.FailureModelUnreachable},
		{&litellm.Error{Type: litellm.ErrorTypeAuth, Message: "invalid api key", StatusCode: 401}, model.FailureModelRefused},
		{&litellm.Error{Type: litellm.ErrorTypeQuota, Message: "insufficient_quota", StatusCode: 429}, model.FailureModelRefused},
		{&litellm.Error{Type: litellm.ErrorTypeRateLimit, Message: "slow down", StatusCode: 429, Temporary: true}, model.FailureModelBusy},
		{&litellm.Error{Type: litellm.ErrorTypeProvider, Message: "bad gateway", StatusCode: 502, Temporary: true}, model.FailureModelBusy},
		{&litellm.Error{Type: litellm.ErrorTypeContextOverflow, Message: "prompt is too long", StatusCode: 400}, ""},
		{errors.New("store is closed"), ""},
	} {
		got := Classify(c.err)
		if model.FailureCodeFor(got) != c.want || got.Error() != c.err.Error() || !errors.Is(got, c.err) {
			t.Errorf("Classify(%v) = %q %q", c.err, model.FailureCodeFor(got), got)
		}
	}
	if Classify(nil) != nil {
		t.Fatal("Classify(nil) must stay nil")
	}
}
