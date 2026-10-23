package rpcsdk_test

// `--manifest` 是装配期探测的唯一入口（ADR-0005 决策 5）：核心起
// `bin --manifest`，插件往 stdout 打一段 JSON 然后退出 0，核心据此决定
// 要不要起这个进程。所以它必须由**插件的产物**应答，不能由核心替它猜。

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/knight000/go_qqbot/pkg/host"
	"github.com/knight000/go_qqbot/pkg/rpc"
)

// 探测协议：一个 JSON 对象、退出码 0、stdout 上不许有别的东西。
//
// 解析用 rpc.DecodeManifest（会拒绝尾随内容），所以"先打一行版本号再打 JSON"
// 这种形态在这里就会红——现场它的表现是"插件装上了但一条命令都不响应"。
func TestManifestFlagPrintsManifestAndExitsZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, fixtureBinary(t), "--manifest")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bin --manifest 失败: %v", err)
	}

	m, optional, err := rpc.DecodeManifest(out)
	if err != nil {
		t.Fatalf("stdout 不是合法清单: %v\n%s", err, out)
	}
	if m.Name != "testplugin" {
		t.Fatalf("Name = %q，期望 testplugin", m.Name)
	}
	if m.APIVersion != host.APIVersion {
		t.Fatalf("APIVersion = %q，期望与本核心一致（%q）", m.APIVersion, host.APIVersion)
	}
	if len(m.Commands) == 0 {
		t.Fatal("命令表为空：探测拿到的清单必须是插件自我申报的完整那一份")
	}
	// 三个可选接口都要报出来：核心据此决定发不发 start / close / run_job。
	if len(optional) != 3 {
		t.Fatalf("Optional = %v，期望三个可选方法全在", optional)
	}
}
