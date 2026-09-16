package diagagent

import "strings"

// defaultOutputLanguage is used when no DIAGFLOW_LLM__LANGUAGE is configured.
// It is Simplified Chinese because the rendered report (labels, agent stream
// events, web UI) is Chinese — English LLM prose made reports read as
// half-translated.
const defaultOutputLanguage = "zh-CN"

// languageName maps a config language code to the human name handed to the
// model. Unknown codes are returned verbatim so a custom value still works.
func languageName(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "", "zh", "zh-cn", "zh-hans", "cn", "chinese", "中文", "简体中文":
		return "简体中文"
	case "en", "en-us", "english":
		return "English"
	default:
		return strings.TrimSpace(code)
	}
}

// languageDirective is appended to every prompt that produces text a human
// reads, so the model emits it in the configured language instead of defaulting
// to English.
func languageDirective(code string) string {
	name := languageName(code)
	if name == "English" {
		return "\n\nIMPORTANT: Write every user-facing field (root cause, evidence " +
			"analysis, suggestions, investigation intent/judgment) in English."
	}
	return "\n\n重要：所有面向用户输出的字段（根因、证据分析、修复建议、排查思路与判断）" +
		"必须使用" + name + "书写，不要输出英文。"
}
