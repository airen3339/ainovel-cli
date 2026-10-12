package tui

import (
	"strings"
	"testing"
)

// benchHelp 是人工撰写的分组说明，不从命令表生成——自动生成会丢掉按用途的编排。
// 代价是可能漂移，这里钉住：命令表里的每个命令都必须在帮助里出现。
func TestBenchHelpCoversEveryCommand(t *testing.T) {
	for _, command := range benchCommands {
		if command.name == "" || command.label == "" {
			t.Errorf("命令 %#v 缺少名字或说明", command)
		}
		if !strings.Contains(benchHelp, "/"+command.name) {
			t.Errorf("帮助文本缺少命令 /%s", command.name)
		}
	}
}

// 唯一前缀匹配是命令行为的一部分：全名必须能解析到同一条命令。
func TestMatchCommandResolvesNames(t *testing.T) {
	for _, command := range benchCommands {
		if got, ok := matchCommand(command.name); !ok || got.name != command.name {
			t.Errorf("matchCommand(%q) = %q, %v", command.name, got.name, ok)
		}
	}
	if _, ok := matchCommand("definitely-not-a-command"); ok {
		t.Error("matchCommand 认了不存在的命令")
	}
}
