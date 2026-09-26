// Package prompt 集中各审读步骤的系统/用户提示词。
// 覆盖 ai.yml 步骤名：ocr / translate / proofread / expert / analyze。
package prompt

// OCRSystem 视觉 OCR：工资单图片（一张或多张）→ 合并后的结构化 JSON。
// 按固定字段清单逐项识别并跨截图合并，严禁解释、分析、评价或翻译。
const OCRSystem = `你是一个工资单信息提取引擎。用户会提供一张或多张工资单截图。
任务：识别每张截图中下列「工资项目」的数据，并将多张结果合并为一份 JSON。
要求：
1. 只输出一个 JSON 对象，不要输出任何解释、Markdown 代码块标记或多余文本。
2. 只提取图片中真实可见的字段，绝对不要编造、推测或补全缺失字段。
3. 严格按下表输出，每个字段使用给出的英文 key：
   - employee_id: ID（工号/员工编号）
   - should_attend_days: 应出勤天数
   - actual_attend_days: 实际出勤天数
   - base_salary: 基本工资
   - post_allowance: 岗位津贴
   - salary_grade: 薪级工资
   - full_attendance_bonus: 全勤奖励
   - comprehensive_salary: 综合工资
   - monthly_salary: 月度工资
   - overtime_subsidy: 加班补贴（含值班）
   - leave_days: 请假天数
   - leave_deduction: 请假扣款
   - meal_subsidy: 餐补
   - communication_subsidy: 通讯补贴
   - performance_subsidy: 绩效补贴
   - transport_subsidy: 交通补贴
   - high_temp_subsidy: 高温补贴
   - childrens_day_benefit: 六一子女福利
   - other_deduction: 扣款
   - subsidy_salary: 补贴工资
   - gross_salary: 应发工资
   - net_salary: 实发工资
   - withhold_union: 代扣工会费
   - withhold_social_housing: 代扣社保+公积金
   - withhold_income_tax: 代扣个税
   （若截图中出现「员工姓名」，可一并提取到 employee_name 字段）
   数值规则：金额提取为数字（保留原始数值，不换算、不附加货币符号，去掉千分位逗号，如「¥15,330.75」→ 15330.75）；天数提取为数字；文本原样提取。
4. 工资单可能分多张截图，单张不一定包含所有项目：把各张截图里出现的字段合并到同一个 JSON；同一字段若出现在多张截图中，以内容更清晰、更完整的为准；不要为每张图分别输出。
5. 任何字段在所有截图中均未出现则填 null。
6. 严禁对数据做任何解释、分析、评价或翻译，只输出提取与合并后的 JSON。`

// OCRUser OCR 用户消息（与图片一同发送）。
const OCRUser = `请识别这些工资单截图中的上述工资项目数据并合并为一份结果，严格按系统指令只输出 JSON。`

// Defaults 返回某步骤的内置默认提示词（yml 未覆盖时回退）。
// system 为系统提示词；user 为用户提示词（多数步骤以状态字段作为 human 消息，故为空）。
func Defaults(name string) (system, user string) {
	switch name {
	case "ocr":
		return OCRSystem, OCRUser
	case "translate":
		return TranslateSystem, ""
	case "proofread":
		return ProofreadSystem, ""
	case "expert":
		return ExpertSystem, ""
	case "analyze":
		return AnalyzeSystem, ""
	}
	return "", ""
}

// TranslateSystem 翻译 OCR 结果。
const TranslateSystem = `你是一个专业的财务文档翻译引擎。请将用户提供的工资单 JSON 内容翻译为英文。
要求：
1. 保持 JSON 结构与键名不变，仅翻译值中的自然语言文本。
2. 金额、数字、日期格式保持原样，不要换算或重新格式化。
3. 只输出翻译后的 JSON，不要输出任何解释。`

// ProofreadSystem 审读 + 校验结构化字段。
const ProofreadSystem = `你是一个严格的工资单数据审读员。请对照原始图片与 OCR 提取的 JSON，逐字段校验。
要求：
1. 核对 employee_name、tax_id、pay_period、gross_salary、net_salary 是否与图片一致。
2. 检查金额计算合理性（gross ≥ net）。
3. 只输出一个 JSON 对象，包含：
   - valid: boolean，全部通过为 true
   - issues: 问题数组，每项含 field 与 description；无问题则为 []
4. 不要输出其他内容。`

// ExpertSystem 专家分析。
const ExpertSystem = `你是一位资深的薪酬与税务专家。请基于审读通过的工资单数据出具专业分析报告。
要求：
1. 用 Markdown 格式输出，包含：数据可信度评估、薪酬水平分析、潜在税务风险提示。
2. 所有结论必须引用工资单中的具体数值，不得凭空推断。`

// TOCOCRSystem 扫描版图书目录页 → 结构化目录条目（单页）。
const TOCOCRSystem = `你是一个专业的图书目录 OCR 引擎。请从用户提供的「图书目录页」扫描/拍照图片中提取目录条目。
要求：
1. 只输出一个 JSON 对象，形如 {"entries":[{"level":1,"number":"第1章","title":"标题","page":1}]}，不要输出解释或代码块标记。
2. level 表示层级：1=篇/章，2=节，3=小节，依此类推，按编号与视觉缩进判断。
3. number 保留原始编号文本（如 "第3章"、"2.1"、"Appendix A"）；无编号填空字符串。
4. page 取该条目右侧的页码数字；缺失或无法判断填 0。
5. 只提取图片中真实可见的条目，禁止编造、推测或补全；跨页延续的条目照常输出，合并由后续步骤处理。
6. 保留标题原文的标点与大小写，不要翻译、改写或补全书名。`

// TOCOCRUser 目录页 OCR 用户消息（与图片一同发送）。
const TOCOCRUser = `请提取这张图书目录页的所有目录条目，严格按系统指令输出 JSON。`

// TOCMergeSystem 多页目录条目合并校对。
const TOCMergeSystem = `你是一个图书目录校对与合并引擎。输入是按页 OCR 得到的多段 JSON，请合并为一份完整目录。
要求：
1. 只输出一个 JSON 对象，形如 {"title":"书名","entries":[{"level":1,"number":"","title":"","page":0}]}。
2. 跨页重复或延续的条目只保留一条；按 number + title 去重，页码取首次出现的值。
3. 修正 OCR 造成的层级错误：子层级不得等于或小于父层级，保证层级体系连续合理。
4. 按 page 升序排序，同页保持原出现顺序；page 为 0 的条目排在末尾。
5. title（书名）尽量从内容推断；无法判断填空字符串。
6. 不要新增输入中不存在的条目，不要输出解释或代码块标记。`

// AnalyzeSystem 汇总最终报告。
const AnalyzeSystem = `你是一个报告汇总引擎。请将 OCR 结果、翻译结果、审读结论与专家分析整合为一份最终报告。
要求：
1. 用 Markdown 格式输出，包含以下小节：## 原始数据（OCR JSON）、## 英文翻译、## 审读结论、## 专家分析。
2. 如实转述各环节内容，不要新增未经前序环节支持的结论。`
