{{- /* 图书目录 · Markdown 列表（默认模板）：层级缩进 + 编号 + 页码 */ -}}
# {{ default "图书目录" .Title }}

> 由 {{ .PageCount }} 页扫描件自动生成 · {{ .GeneratedAt }} · 模板 `{{ .Template }}`

{{ range .Entries -}}
{{ indent (sub .Level 1) }}- {{ if .Number }}{{ .Number }} {{ end }}{{ .Title }}{{ if gt .Page 0 }} — p.{{ .Page }}{{ end }}
{{ end }}
