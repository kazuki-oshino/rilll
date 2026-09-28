package learnings

import "testing"

// TestValidCategory_AcceptsClosedSet は 4 値 closed enum がすべて true で受け入れられることを確認する。
// Requirement 5.3 (category closed set) のカバレッジ。
func TestValidCategory_AcceptsClosedSet(t *testing.T) {
	t.Parallel()

	cases := []string{
		CategoryFailureRecovery,
		CategorySuccessProcedure,
		CategoryAntipattern,
		CategoryToolPattern,
	}

	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			if !ValidCategory(c) {
				t.Fatalf("ValidCategory(%q) = false, want true", c)
			}
		})
	}
}

// TestValidCategory_ConstantValues は 4 値 closed enum の定数値が design.md と一致することを確認する。
// 値が変わると akashic table の category 列に書かれた既存値や playbook ディレクトリ名と乖離するため、
// 値そのものの不変式として記録する。
func TestValidCategory_ConstantValues(t *testing.T) {
	t.Parallel()

	wants := map[string]string{
		"CategoryFailureRecovery":  "failure-recovery",
		"CategorySuccessProcedure": "success-procedure",
		"CategoryAntipattern":      "antipattern",
		"CategoryToolPattern":      "tool-pattern",
	}
	gots := map[string]string{
		"CategoryFailureRecovery":  CategoryFailureRecovery,
		"CategorySuccessProcedure": CategorySuccessProcedure,
		"CategoryAntipattern":      CategoryAntipattern,
		"CategoryToolPattern":      CategoryToolPattern,
	}

	for name, want := range wants {
		if gots[name] != want {
			t.Fatalf("%s = %q, want %q", name, gots[name], want)
		}
	}
}

// TestValidCategory_RejectsOutOfSet は closed enum 外の値がすべて false で reject されることを確認する。
// Requirement 5.3 / 6.4 (category 不一致 reject) のカバレッジ。
func TestValidCategory_RejectsOutOfSet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "unknown lowercase", input: "unknown"},
		{name: "uppercase variant", input: "Failure-Recovery"},
		{name: "underscore variant", input: "failure_recovery"},
		{name: "trailing space", input: "failure-recovery "},
		{name: "leading space", input: " failure-recovery"},
		{name: "japanese", input: "失敗回復"},
		{name: "v1 distiller category", input: "anti-pattern"}, // closed set は antipattern (no hyphen)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if ValidCategory(tc.input) {
				t.Fatalf("ValidCategory(%q) = true, want false", tc.input)
			}
		})
	}
}

// TestValidTopicName_Accepts は regex `^[a-z][a-z0-9-]*$` に合致する topic 名が受け入れられることを確認する。
// Requirement 7.6 (topic name validation) のカバレッジ。
func TestValidTopicName_Accepts(t *testing.T) {
	t.Parallel()

	cases := []string{
		"a",
		"ab",
		"akashic",
		"agent-call",
		"sub-agent-id-collision",
		"abc123",
		"a-1",
		"x9",
	}

	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			if !ValidTopicName(c) {
				t.Fatalf("ValidTopicName(%q) = false, want true", c)
			}
		})
	}
}

// TestValidTopicName_Rejects は regex `^[a-z][a-z0-9-]*$` に違反する topic 名が reject されることを確認する。
// Requirement 7.6 (topic name validation) のカバレッジ。Japanese / 大文字 / underscore / empty / 数字始まり
// / hyphen 始まりをすべてカバーする。
func TestValidTopicName_Rejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "uppercase only", input: "ABC"},
		{name: "mixed case", input: "Akashic"},
		{name: "underscore", input: "agent_call"},
		{name: "japanese", input: "アカシック"},
		{name: "leading digit", input: "1abc"},
		{name: "leading hyphen", input: "-abc"},
		{name: "trailing space", input: "abc "},
		{name: "internal space", input: "agent call"},
		{name: "dot", input: "agent.call"},
		{name: "slash", input: "agent/call"},
		{name: "single hyphen", input: "-"},
		{name: "single digit", input: "1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if ValidTopicName(tc.input) {
				t.Fatalf("ValidTopicName(%q) = true, want false", tc.input)
			}
		})
	}
}

// TestCategoryPath は CategoryPath が `<category>/<topic>.md` 形式 (forward slash) を返すことを確認する。
// Requirement 7.1 (旧 category subdirectory layout `<category>/<topic>.md`) のカバレッジ。
//
// 注: agent-learnings-wiki spec で playbook の物理 layout は wiki 単一 file
// (`docs_dev/playbook/<category>.md` + `## <topic>` section) に移行済。CategoryPath は
// legacy 表記を返す純関数として残るが、Apply phase は直接利用しない。OS 依存セパレータには
// 寄せず forward slash を使う (design.md の表記と整合)。
func TestCategoryPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		category string
		topic    string
		want     string
	}{
		{
			name:     "failure-recovery / akashic",
			category: CategoryFailureRecovery,
			topic:    "akashic",
			want:     "failure-recovery/akashic.md",
		},
		{
			name:     "success-procedure / playbook-format",
			category: CategorySuccessProcedure,
			topic:    "playbook-format",
			want:     "success-procedure/playbook-format.md",
		},
		{
			name:     "antipattern / sub-agent-id-collision",
			category: CategoryAntipattern,
			topic:    "sub-agent-id-collision",
			want:     "antipattern/sub-agent-id-collision.md",
		},
		{
			name:     "tool-pattern / git-resolve",
			category: CategoryToolPattern,
			topic:    "git-resolve",
			want:     "tool-pattern/git-resolve.md",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CategoryPath(tc.category, tc.topic)
			if got != tc.want {
				t.Fatalf("CategoryPath(%q, %q) = %q, want %q", tc.category, tc.topic, got, tc.want)
			}
		})
	}
}
