package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/gateway"
	"github.com/fairlb/fairlb/settings"
)

// Community 二进制装配了哪些设置键，写死在这里。
//
// # 它现在问的是装配点，不是进程
//
// 注册表此前是**进程级**的、由各包的 `init()` 填充，于是一个二进制有哪些键取决于它
// 链接了哪些包——那也是这条用例当初必须存在于此的理由：Cloud 那份清单看得见 gateway
// 的键，却看不见 Community 这个二进制少了其中一个（Cloud 照样从自己的 import 图里
// 拿到它，测试照样绿）。
//
// 注册表改成值之后（ADR-0194），这条用例问的是 `gateway.SettingSpecs()`——**入口
// 交给 `settings.NewRegistry` 的正是同一份**。清单仍然写死，理由不变：每个键都会在
// 运营后台长出一行可写的控件，加键与删键都该是有人看过的决定。
//
// # 少一个键的症状
//
// Community 没有设置页（`public/api/staff.yaml` 里零个 settings 端点），所以症状不是
// 「后台少一格」，而更安静：
//
//   - `gateway.kill_switch` —— 请求热路径上的 `Settings.KillSwitch` 按键直读，不受影响；
//     但管理台横幅走的是 `KillSwitchState`，它遍历 `store.List()`，而 List 是按
//     `Registered()` 组装的。键没了，**横幅会显示「未启用」而闸实际已经拉下**。
//   - `gateway.fx_usd_cny` / `gateway.byok_fee_bps` —— 直接乘进账单。
//   - `gateway.anomaly_*` —— 异常告警的阈值。
//
// 加减键时改这张表，顺手复核新键在 Community 这一侧到底有没有消费者。
func TestCommunitySettingsRegistryInventory(t *testing.T) {
	want := map[string]struct {
		group  settings.Group
		impact settings.Impact
	}{
		"gateway.fx_usd_cny":                 {settings.GroupBilling, settings.ImpactHigh},
		"gateway.byok_fee_bps":               {settings.GroupBilling, settings.ImpactHigh},
		"gateway.kill_switch":                {settings.GroupOperations, settings.ImpactHigh},
		"gateway.anomaly_multiplier":         {settings.GroupOperations, settings.ImpactNormal},
		"gateway.anomaly_floor":              {settings.GroupOperations, settings.ImpactNormal},
		"gateway.resource_affinity_ttl_days": {settings.GroupRetention, settings.ImpactHigh},
		// 生成的视频是客户内容，调小这个就是把它删掉（ADR-0222）。
		"gateway.video_retention_hours": {settings.GroupRetention, settings.ImpactHigh},
	}

	// 与 main.go 同源：入口建 Store 时交进去的就是这一份。
	got := settings.NewRegistry(gateway.SettingSpecs()).All()
	if len(got) == 0 {
		t.Fatal("注册表为空——SettingSpecs 什么都没交出来，这条断言什么都没查")
	}

	seen := map[string]bool{}
	for _, sp := range got {
		seen[sp.Key] = true
		exp, ok := want[sp.Key]
		if !ok {
			t.Errorf("%s 是 Community 二进制里的新注册键，未登记在本清单里——"+
				"请确认它在这一侧真的有消费者", sp.Key)
			continue
		}
		if sp.Group != exp.group {
			t.Errorf("%s 的 Group = %q，期望 %q", sp.Key, sp.Group, exp.group)
		}
		if sp.Impact != exp.impact {
			t.Errorf("%s 的 Impact = %q，期望 %q", sp.Key, sp.Impact, exp.impact)
		}
		if !strings.HasPrefix(sp.DescriptionKey, "settingDesc") {
			t.Errorf("%s 的 DescriptionKey = %q，须以 settingDesc 开头", sp.Key, sp.DescriptionKey)
		}
	}

	var missing []string
	for k := range want {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("清单里有 %d 个键在 Community 二进制里没被注册：%v——"+
			"多半是某一层不再把它交给 SettingSpecs 了，而那不会有任何编译错误",
			len(missing), missing)
	}
}
