{{- /* 图书目录 · Markdown 表格：层级 / 编号 / 标题 / 页码 */ -}}
# {{ default "图书目录" .Title }}

| 层级 | 编号 | 标题 | 页码 |
|---:|---|---|---:|
{{ range .Entries -}}
| {{ .Level }} | {{ .Number }} | {{ indent (sub .Level 1) }}{{ .Title }} | {{ if gt .Page 0 }}{{ .Page }}{{ end }} |
{{ end }}

> 共 {{ len .Entries }} 条 · 来源 {{ .PageCount }} 页 · {{ .GeneratedAt }} · 模板 `{{ .Template }}`
