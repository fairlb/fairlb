package settings

import (
	"encoding/json"
	"testing"
)

// Value validation per kind. Money and exchange rates travel as strings rather
// than JSON numbers: a JSON number round-trips through float64 and loses
// precision in the decimals.
func TestSpecValidateKinds(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		raw  string
		ok   bool
	}{
		{"bool accepted", Spec{Kind: KindBool}, `true`, true},
		{"bool rejects a string", Spec{Kind: KindBool}, `"true"`, false},

		{"int accepted", Spec{Kind: KindInt}, `1500`, true},
		{"int rejects a fraction", Spec{Kind: KindInt}, `1.5`, false},
		{"int rejects a string", Spec{Kind: KindInt}, `"10"`, false},
		{"int within range", Spec{Kind: KindInt, Range: &Range{Min: 0, Max: 10000}}, `2000`, true},
		{"int above the upper bound", Spec{Kind: KindInt, Range: &Range{Min: 0, Max: 10000}}, `10001`, false},
		{"int below the lower bound", Spec{Kind: KindInt, Range: &Range{Min: 1, Max: 10}}, `0`, false},
		{"int unconstrained without a range", Spec{Kind: KindInt}, `-999999`, true},
		// A lower bound of exactly zero must still constrain. An earlier
		// implementation used "Min and Max both zero" as the sentinel for
		// unconstrained, which made a lower bound of zero inexpressible — and a
		// key shaped exactly like that let a negative floor through into the
		// database. When a sentinel collides with a legitimate value, nothing
		// errors; the check just quietly stops running.
		{"int with a zero lower bound still rejects negatives", Spec{Kind: KindInt, Range: &Range{Min: 0, Max: 100}}, `-1`, false},
		{"int with a zero lower bound accepts 0", Spec{Kind: KindInt, Range: &Range{Min: 0, Max: 100}}, `0`, true},

		{"map within range", Spec{Kind: KindMapStringInt, Range: &Range{Min: 0, Max: 100}}, `{"a":50}`, true},
		{"map out of range", Spec{Kind: KindMapStringInt, Range: &Range{Min: 0, Max: 100}}, `{"a":101}`, false},
		{"map with a zero lower bound still rejects negatives", Spec{Kind: KindMapStringInt, Range: &Range{Min: 0, Max: 100}}, `{"a":-1}`, false},

		{"decimal integer form", Spec{Kind: KindDecimalString}, `"7"`, true},
		{"decimal fractional form", Spec{Kind: KindDecimalString}, `"7.1523"`, true},
		{"decimal negative", Spec{Kind: KindDecimalString}, `"-0.5"`, true},
		{"decimal rejects a JSON number", Spec{Kind: KindDecimalString}, `7.15`, false},
		{"decimal rejects scientific notation", Spec{Kind: KindDecimalString}, `"7.15e2"`, false},
		{"decimal rejects an empty string", Spec{Kind: KindDecimalString}, `""`, false},
		{"decimal rejects surrounding whitespace", Spec{Kind: KindDecimalString}, `" 7.15"`, false},
		{"decimal rejects a non-number", Spec{Kind: KindDecimalString}, `"abc"`, false},

		// Money: main units on the wire, range in nano. The empty string is "not
		// configured", the same convention as an email; a negative amount is
		// never a setting.
		{"money accepted", Spec{Kind: KindMoney}, `"10.00"`, true},
		{"money integer form", Spec{Kind: KindMoney}, `"10"`, true},
		{"money nine decimals", Spec{Kind: KindMoney}, `"0.000000001"`, true},
		{"money empty means unset", Spec{Kind: KindMoney}, `""`, true},
		{"money rejects ten decimals", Spec{Kind: KindMoney}, `"0.0000000001"`, false},
		{"money rejects a JSON number", Spec{Kind: KindMoney}, `10`, false},
		{"money rejects scientific notation", Spec{Kind: KindMoney}, `"1e3"`, false},
		{"money rejects a negative amount", Spec{Kind: KindMoney}, `"-1"`, false},
		{"money rejects a currency symbol", Spec{Kind: KindMoney}, `"$10"`, false},
		{"money within a nano range", Spec{Kind: KindMoney, Range: &Range{Min: 0, Max: 1_000_000_000_000_000}}, `"1000000"`, true},
		{"money above a nano range", Spec{Kind: KindMoney, Range: &Range{Min: 0, Max: 1_000_000_000_000_000}}, `"1000000.000000001"`, false},
		{"money below a nano range", Spec{Kind: KindMoney, Range: &Range{Min: 1_000_000_000, Max: 2_000_000_000}}, `"0.5"`, false},

		{"an unknown kind is always rejected", Spec{Kind: Kind("bogus")}, `1`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate(json.RawMessage(c.raw))
			if c.ok && err != nil {
				t.Fatalf("should accept %s: %v", c.raw, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("should reject %s", c.raw)
			}
		})
	}
}

func TestRegistryLookupAndStableOrder(t *testing.T) {
	reg := NewRegistry([]Spec{{Key: "test.z", Kind: KindBool}, {Key: "test.a", Kind: KindString}})

	spec, ok := reg.Lookup("test.z")
	if !ok || spec.Kind != KindBool {
		t.Fatalf("lookup = (%+v, %v)", spec, ok)
	}
	registered := reg.All()
	if len(registered) != 2 || registered[0].Key != "test.a" || registered[1].Key != "test.z" {
		t.Fatalf("registered = %+v", registered)
	}
}

func TestNewRegistryRejectsDuplicateKeys(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
	}()
	NewRegistry(
		[]Spec{{Key: "test.duplicate", Kind: KindString}},
		[]Spec{{Key: "test.duplicate", Kind: KindString}},
	)
}

// 未装配的服务器一个设置都没有；装配之后，交进去的每一项都出现。
//
// **这条判据在注册表是包级 map 时写不出来**：测试二进制链接了每一个会注册的包，
// 于是「空集」这一半永远造不出来，而它正是那个缺陷的形状——一个包不再被链接，
// 它的键就从设置页上消失，页面照常渲染，剩下的每一行都是对的（ADR-0194）。
func TestUnassembledRegistryIsEmptyAndAssembledCarriesEachKey(t *testing.T) {
	// 空集这一半：nil 注册表是「什么都没装」的诚实读法。
	var unassembled *Registry
	if got := unassembled.All(); len(got) != 0 {
		t.Fatalf("未装配的注册表应当是空集，得到 %d 项：%+v", len(got), got)
	}
	if _, ok := unassembled.Lookup("anything"); ok {
		t.Fatal("未装配的注册表不该认得任何键——认得就意味着键来自别处，不是装配点")
	}

	// 逐项出现这一半：分两组交进去，两组的键都要在。一组一组地验，是因为
	// 「只有第一组在」与「全都在」在总数上分不出来，除非两组各非空且各自点名。
	layerA := []Spec{{Key: "a.one", Kind: KindBool}, {Key: "a.two", Kind: KindString}}
	layerB := []Spec{{Key: "b.one", Kind: KindInt}}
	reg := NewRegistry(layerA, layerB)
	for _, group := range [][]Spec{layerA, layerB} {
		for _, want := range group {
			if _, ok := reg.Lookup(want.Key); !ok {
				t.Fatalf("装配时交了 %q，注册表却不认得它", want.Key)
			}
		}
	}
	if got := len(reg.All()); got != len(layerA)+len(layerB) {
		t.Fatalf("装配后应当恰好有 %d 项，得到 %d——多出来的项没有装配点交过它",
			len(layerA)+len(layerB), got)
	}
}
