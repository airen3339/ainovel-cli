package models

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/voocel/litellm/catalog"
)

// factsTTL 是本地模型清单的有效期，过期后重新下载。
const factsTTL = 24 * time.Hour

// Facts 是模型的已知事实（上下文窗口、标价），取自 LiteLLM 社区维护的模型清单，下载后
// 缓存在本地文件。清单没收录的模型（中转改了名、自部署）没有事实：界面不显示上下文窗口，
// 用量不计花费。
type Facts struct {
	path  string
	url   string
	fresh bool // 启动时读到的本地清单还在有效期内
	table catalog.Catalog
}

func NewFacts(path string) *Facts { return &Facts{path: path, url: catalog.DefaultURL} }

// Read 读本地清单，不联网；没有或读不出时没有事实，等 Refresh 下载。
func (f *Facts) Read() {
	info, err := os.Stat(f.path)
	if err != nil {
		return
	}
	data, err := os.ReadFile(f.path)
	if err == nil && f.table.LoadFromReader(bytes.NewReader(data)) == nil {
		f.fresh = time.Since(info.ModTime()) < factsTTL
	}
}

// Refresh 在 Read 读到的清单缺失、损坏或过期时下载并写回，报告是否换上了新清单；下载
// 失败时保留旧清单。
func (f *Facts) Refresh(ctx context.Context) (bool, error) {
	if f.fresh {
		return false, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return false, err
	}
	client := &http.Client{Timeout: time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return false, fmt.Errorf("download model list: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("download model list: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return false, fmt.Errorf("download model list: %w", err)
	}
	if err := f.table.LoadFromReader(bytes.NewReader(body)); err != nil {
		return false, err
	}
	return true, writeFile(f.path, body)
}

// ContextWindow 是 provider 格式连接上 model 的上下文窗口（token），不知道为 0。
func (f *Facts) ContextWindow(provider, model string) int {
	return f.find(provider, model).MaxInputTokens
}

// Pricing 是 provider 格式连接上 model 的标价（美元），没有为 nil。中转站按原名转发
// 官方模型时用的是官方标价，与中转的实际计费可能不同。
func (f *Facts) Pricing(provider, model string) *catalog.Pricing {
	return f.find(provider, model).Pricing
}

// find 先按清单「厂商/模型」的键找，再按模型名本身找：中转站转发官方模型时多用原名。
func (f *Facts) find(provider, model string) catalog.Model {
	for _, key := range []string{provider + "/" + model, model} {
		if facts, ok := f.table.Get(key); ok {
			return facts
		}
	}
	return catalog.Model{}
}

// writeFile 先写临时文件再改名，读的一方不会看到写了一半的清单。
func writeFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".models-*")
	if err != nil {
		return fmt.Errorf("save model list: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("save model list: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("save model list: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("save model list: %w", err)
	}
	return nil
}
