package bootstrap_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// recordBrokenToolCalls 让真实模型验收把厂商的原始流式响应存进 dir（AINOVEL_LIVE_RAW），只留
// 工具参数拼不成 JSON 的那些：查参数是厂商发坏的，还是我们拼坏的。litellm 默认走
// http.DefaultClient。
func recordBrokenToolCalls(t *testing.T, dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	http.DefaultClient.Transport = &rawRecorder{dir: dir, next: http.DefaultTransport}
}

type rawRecorder struct {
	dir  string
	next http.RoundTripper
	n    atomic.Int64
}

func (r *rawRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.next.RoundTrip(request)
	if err != nil || !strings.HasSuffix(request.URL.Path, "/chat/completions") {
		return response, err
	}
	path := filepath.Join(r.dir, fmt.Sprintf("%06d.sse", r.n.Add(1)))
	file, err := os.Create(path)
	if err != nil {
		response.Body.Close()
		return nil, err
	}
	response.Body = &rawBody{Reader: io.TeeReader(response.Body, file), body: response.Body, file: file, path: path}
	return response, nil
}

type rawBody struct {
	io.Reader
	body io.Closer
	file *os.File
	path string
}

func (b *rawBody) Close() error {
	err := b.body.Close()
	b.file.Close()
	if !brokenToolCall(b.path) {
		os.Remove(b.path)
	}
	return err
}

// brokenToolCall 照 OpenAI 流式格式按 index 拼工具参数：有一个拼不成 JSON 就算坏。
func brokenToolCall(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	calls := make(map[int]string)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<26)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok || strings.TrimSpace(data) == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				calls[call.Index] += call.Function.Arguments
			}
		}
	}
	for _, arguments := range calls {
		if !json.Valid([]byte(arguments)) {
			return true
		}
	}
	return false
}
