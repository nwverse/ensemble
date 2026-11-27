package extension

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func replies(opts []option) []string {
	var out []string
	for _, o := range opts {
		out = append(out, o.Reply)
	}
	return out
}

// All texts here are made up for the test.
func TestParseOptionsReadsLetteredChoices(t *testing.T) {
	for name, tc := range map[string]struct {
		text string
		want []string
	}{
		"fullwidth colon, with a closing choice": {
			"报表任务卡住了，下一步怎么办？\nA：现在就重跑一次\nB：先查清原因再说\n先放着",
			[]string{"A：现在就重跑一次", "B：先查清原因再说", "先放着"}},
		"ascii colon":                                {"选一个：\nA: first thing\nB: second thing", []string{"A：first thing", "B：second thing"}},
		"period and brackets":                        {"Pick:\nA. alpha\n(B) beta\nC) gamma", []string{"A：alpha", "B：beta", "C：gamma"}},
		"bold markers and bullets":                   {"请选择\n- **A：甲方案**\n- **B：乙方案**", []string{"A：甲方案", "B：乙方案"}},
		"option prefix":                              {"方案如下，你选：\n选项A：保守\n选项B：激进", []string{"A：保守", "B：激进"}},
		"windows line ends":                          {"选哪个：\r\nA：one\r\nB：two\r\n", []string{"A：one", "B：two"}},
		"lowercase markers":                          {"Pick:\na) one\nb) two", []string{"A：one", "B：two"}},
		"sign-off is not an option":                  {"选哪个？\nA：one\nB：two\nCheers", []string{"A：one", "B：two"}},
		"a question after the list is not an option": {"选一个：\nA：one\nB：two\n你选哪个？", []string{"A：one", "B：two"}},
		"colon and a choosing word":                  {"有两个办法：\nA：现在就重跑\nB：做根治\n先放着", []string{"A：现在就重跑", "B：做根治", "先放着"}},
		"english question":                           {"Which should I do next?\n1. Retry\n2. Investigate", []string{"1：Retry", "2：Investigate"}},
		"numbered list that asks":                    {"你想选哪一个？\n1. 旧办法\n2. 新办法", []string{"1：旧办法", "2：新办法"}},
		"numbered list with the question after":      {"要不要继续？\n1、保留\n2、删除\n先放着", []string{"1：保留", "2：删除", "先放着"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := replies(parseOptions(tc.text))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseOptions(%q)\n got %q\nwant %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestParseOptionsLeavesOrdinaryTextAlone(t *testing.T) {
	for name, text := range map[string]string{
		"empty":                          "",
		"plain prose":                    "已经处理好了，不需要你做什么。",
		"one marked line":                "结论：\nA：只有一项",
		"markers out of order":           "x\nB：two\nA：one",
		"markers not from the start":     "x\nB：two\nC：three",
		"steps are not a question":       "按下面的步骤做：\n1. 打开设置\n2. 点击保存\n3. 重启",
		"prose after the list":           "A：one\nB：two\n这两项我都试过了。\n第一项会失败，因为缓存没有清空。\n第二项可以，但很慢。\n所以我建议先看日志。",
		"too many":                       "选哪个？\n1：a\n2：b\n3：c\n4：d\n5：e\n6：f\n7：g",
		"abbreviation":                   "出自 U.S. 的报告",
		"markers of two kinds":           "选哪个\nA：one\n2：two",
		"steps with a which in the lead": "Here are the steps, which you can follow:\n1. Install it\n2. Run it",
		"results then a question":        "I did this:\n1. Fixed the bug\n2. Added tests\nAnything else?",
		"sizes are not markers":          "1.5 GB used\n2.0 GB free\nWant more detail?",
		"a lettered list with no lead":   "OK.\nA：你好\nB：你好呀",
		"sample dialogue":                "示例对话：\nA：你好\nB：你好呀",
		"sample exchange":                "Here's a sample exchange:\nA: hello\nB: hi there",
		"compare plans":                  "Compare the two plans:\nA. costs $10\nB. costs $20",
		"changes then offer":             "I made these changes:\n1. Fixed the bug\n2. Added tests\nLet me know if you need anything else.",
		"cue word far from the list":     "问题原因是否和缓存有关，我列了日志：\n1. 10:01 cache miss\n2. 10:02 cache miss",
		"cue word only after the list":   "1. one\n2. two\n你选哪个？",
	} {
		t.Run(name, func(t *testing.T) {
			if got := parseOptions(text); got != nil {
				t.Fatalf("parseOptions(%q) = %q, want none", text, replies(got))
			}
		})
	}
}

func TestParseOptionsTakesTheLastList(t *testing.T) {
	text := "先前的方案：\nA：旧甲\nB：旧乙\n\n后来改成了这样，你选：\nA：新甲\nB：新乙"
	if got, want := replies(parseOptions(text)), []string{"A：新甲", "B：新乙"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseOptionsLimits(t *testing.T) {
	long := strings.Repeat("长", 300)
	got := parseOptions("选哪个\nA：" + long + "\nB：短")
	if len(got) != 2 {
		t.Fatalf("got %d options", len(got))
	}
	if n := utf8.RuneCountInString(got[0].Label); n != maxOptionLabel || !strings.HasSuffix(got[0].Label, "…") {
		t.Fatalf("label is %d characters: %q", n, got[0].Label)
	}
	if n := utf8.RuneCountInString(got[0].Reply); n != maxOptionReply {
		t.Fatalf("reply is %d characters", n)
	}
	// at most two unmarked closing lines
	got = parseOptions("选哪个：\nA：one\nB：two\n甲\n乙\n丙")
	if want := []string{"A：one", "B：two", "甲", "乙"}; !reflect.DeepEqual(replies(got), want) {
		t.Fatalf("got %q, want %q", replies(got), want)
	}
	if got[2].Marker != "" || got[0].Marker != "A" {
		t.Fatalf("markers: %+v", got)
	}
}
