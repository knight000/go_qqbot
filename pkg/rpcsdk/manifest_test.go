package rpcsdk_test

// `--manifest` 是装配期探测的唯一入口（ADR-0005 决策 5）：核心起
// `bin --manifest`，插件往 stdout 打一段 JSON 然后退出 0，核心据此决定
// 要不要起这个进程。所以它必须由**插件的产物**应答，不能由核心替它猜。

import (
	"context"
	"os/exec"
	"slices"
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
	// 使用面必须真的从产物里念出来：核心的兼容判定读的就是这一份，而它
	// 只在「探测 → 线协议 → 判定」这条路上走一次，中间任何一段丢字段都会
	// 让所有插件悄悄退化成"没声明使用面"（= 整体相等），且没有任何报错。
	//
	// 期望值是两个来源的并集（#56）：夹具的 Manifest.Uses 手写一条
	// （Event.Args），APISurface 报两条（其中 Event.Args 重复），合并后
	// 去重、排序。少一条说明合并没发生，多一条说明去重没生效。
	if want := []string{"Event.Args", "JobSpec.Timeout"}; !slices.Equal(m.Uses, want) {
		t.Fatalf("Uses = %v，期望 %v（使用面必须穿过 --manifest，且与手写追加项合并去重）", m.Uses, want)
	}
	// 三个可选接口都要报出来：核心据此决定发不发 start / close / run_job。
	if len(optional) != 3 {
		t.Fatalf("Optional = %v，期望三个可选方法全在", optional)
	}
}
