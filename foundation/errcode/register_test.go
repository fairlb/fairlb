package errcode

import "testing"

// 包内测试：它要验的是 Register 往私有 registry 里写了什么，以及重复注册会不会 panic。
// registry 不再导出（ADR-0161），而「测试能直接 delete 掉一个键」正是不导出要防的那种写入——
// 这条测试因此属于包内，不属于外部测试包。

const probe = "gateway.register_probe"

func TestRegisterAddsDefinitions(t *testing.T) {
	t.Cleanup(func() { delete(registry, probe) })

	if _, ok := registry[probe]; ok {
		t.Fatalf("%s was already registered; this test cannot tell its own effect apart from a pre-existing one", probe)
	}
	Register(map[string]Def{
		probe: {Code: probe, Status: 418, Title: "Probe", TitleZH: "Probe"},
	})
	got, ok := registry[probe]
	if !ok {
		t.Fatalf("%s did not reach the registry", probe)
	}
	if got.Status != 418 || got.Title != "Probe" {
		t.Fatalf("registered definition came back changed: %+v", got)
	}
}

// A second definition for one code must stop the process rather than be
// resolved by init order: whichever one lost would be a public contract that
// silently does not describe what clients get.
func TestRegisterRejectsADuplicate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering an already-registered code returned normally")
		}
	}()
	Register(map[string]Def{
		CommonNotFound: {Code: CommonNotFound, Status: 404, Title: "Not found"},
	})
}

// An empty segment is what a generator run that read nothing produces, and it
// is indistinguishable from a build that legitimately ships fewer segments
// unless registering nothing is itself an error.
func TestRegisterRejectsAnEmptySegment(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering an empty segment returned normally")
		}
	}()
	Register(map[string]Def{})
}
